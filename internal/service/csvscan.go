package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// CSVScanOptions 是一次"只写 CSV"的扫描参数。
//
// 与 ScanOptions 的区别是这里**没有**会话、库、续测这些概念：
// 结果直接追加到 CSV，中断就是中断，已经写下去的行就是结果。
type CSVScanOptions struct {
	// OutputPath 是 CSV 文件路径。
	OutputPath string

	// Append 为真时追加到已有文件（不重复写表头）。
	Append bool

	// Workers 是并发探测数（<=0 用默认值）。
	Workers int

	// Timeout 是单个连接超时（<=0 用默认值）。
	Timeout time.Duration

	// Limit 只测前 N 个目标（<=0 表示全部）。
	Limit int

	// Trace 为真时对**探测成功**的目标做线路跟踪。
	Trace bool

	// TraceConfig 是跟踪配置。
	TraceConfig TraceOptions

	// Progress 接收进度（nil 表示不关心）。
	Progress func(ProgressEvent)

	// OnTarget 在每个目标**开始**探测时调用（可为 nil，会并发调用）。
	OnTarget func(target string)

	// OnTrace 在一条线路跟踪完成后调用（可为 nil）。
	OnTrace func(target string, result *trace.TraceResult)
}

// CSVScanResult 是扫描结果摘要。
type CSVScanResult struct {
	// OutputPath 是实际写入的文件。
	OutputPath string

	// Targets 是本次考虑的目标总数（应用 Limit 之后）。
	Targets int

	// Probed / Succeeded / Failed 是探测统计。
	Probed    int
	Succeeded int
	Failed    int

	// Traced / TracedOK 是跟踪统计。
	Traced   int
	TracedOK int

	// RowsWritten 是写进 CSV 的数据行数。
	RowsWritten int

	// Errors 是落盘失败次数。
	//
	// 必须计数并上报：磁盘满、权限错时如果只是静默失败，
	// 用户会以为"测完了"，实际上什么都没存下来。
	Errors int

	// TraceUnavailable 说明"要求跟踪但引擎不可用"的原因（可空）。
	TraceUnavailable string

	// Interrupted 表示被中途取消。
	Interrupted bool

	// SourceURL / SourceFromCache 描述目标列表来源。
	SourceURL       string
	SourceFromCache bool

	// StartedAt / FinishedAt 是起止时间。
	StartedAt  time.Time
	FinishedAt time.Time
}

// Elapsed 返回耗时。
func (r CSVScanResult) Elapsed() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ProgressEvent 是一次进度更新。
type ProgressEvent struct {
	// Phase 是阶段："probe" 或 "trace"。
	Phase string

	// Completed / Total 是完成数与总数。
	Completed int
	Total     int

	// Success / Failed 是成功与失败数。
	Success int
	Failed  int

	// CurrentTarget 是当前正在处理的目标。
	CurrentTarget string
}

// RunCSVScan 执行一次"结果直接进 CSV"的扫描。
//
// 设计要点：
//
//   - **不用数据库**：没有会话表、没有迁移、没有幂等去重。
//   - **不恢复会话**：每次都是新的开始。中断后想继续，
//     自己决定要不要重新跑——工具不假装知道你的意图。
//   - **每行立即刷盘**：中途被杀/断电，已完成的测量仍在文件里。
//   - **跟踪只对成功的目标做**：连不上的目标追了也没意义。
func (s *Service) RunCSVScan(ctx context.Context, opts CSVScanOptions) (*CSVScanResult, error) {
	// ---- 1) 参数校验（在任何副作用之前） ----
	if strings.TrimSpace(opts.OutputPath) == "" {
		return nil, fmt.Errorf("%w: an output CSV path is required", ErrUsage)
	}
	if opts.Limit < 0 {
		return nil, fmt.Errorf("%w: limit must not be negative", ErrUsage)
	}
	if opts.Trace {
		if _, err := trace.Mode(opts.TraceConfig.Mode).Normalize(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
	}

	// ---- 2) 目标列表 ----
	targets, meta, fromCache, err := s.loadTargets(ctx)
	if err != nil {
		return nil, err
	}
	if opts.Limit > 0 && opts.Limit < len(targets) {
		targets = targets[:opts.Limit]
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: no targets to scan (source %s returned an empty list)",
			ErrNoTargets, meta.URL)
	}
	s.log("scan: %d target(s) from %s (cache=%v)", len(targets), meta.URL, fromCache)

	// ---- 3) 打开 CSV（在探测之前，这样连表头都先落了盘） ----
	store, err := csvstore.Open(csvstore.Options{Path: opts.OutputPath, Append: opts.Append})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			s.log("scan: closing csv: %v", cerr)
		}
	}()

	// ---- 4) 探测 ----
	result := &CSVScanResult{
		OutputPath:      store.Path(),
		Targets:         len(targets),
		SourceURL:       meta.URL,
		SourceFromCache: fromCache,
		StartedAt:       time.Now().UTC(),
	}

	prober := probe.New(probeConfig(opts))

	// 探测成功的目标（后面要跟踪的那些）。
	successful := make([]string, 0, len(targets))

	for index, target := range targets {
		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}

		if opts.OnTarget != nil {
			opts.OnTarget(target.ID)
		}

		probeResult := prober.Probe(ctx, target)
		result.Probed++
		if probeResult.Success {
			result.Succeeded++
			successful = append(successful, target.ID)
		} else {
			result.Failed++
		}

		// **立即写盘**：这是"中断不丢结果"的实现点。
		if err := store.Append(probeRow(target, probeResult)); err != nil {
			result.Errors++
			s.log("scan: writing row for %s failed: %v", target.ID, err)
		} else {
			result.RowsWritten++
		}

		if opts.Progress != nil {
			opts.Progress(ProgressEvent{
				Phase:         "probe",
				Completed:     index + 1,
				Total:         len(targets),
				Success:       result.Succeeded,
				Failed:        result.Failed,
				CurrentTarget: target.ID,
			})
		}
	}

	// ---- 5) 线路跟踪（只对成功的目标） ----
	if opts.Trace && !result.Interrupted && len(successful) > 0 {
		engine, engineErr := s.buildTraceEngine(ctx, opts.TraceConfig)
		if engineErr != nil {
			// 引擎不可用**不算扫描失败**：TCP 结果已经写进 CSV 了。
			// 如实记录，让调用方展示原因。
			s.log("scan: trace engine unavailable: %v", engineErr)
			result.TraceUnavailable = engineErr.Error()
		} else {
			s.runTracePhase(ctx, store, engine, targets, successful, opts, result)
		}
	}

	result.FinishedAt = time.Now().UTC()
	s.log("scan: finished probed=%d ok=%d fail=%d traced=%d rows=%d errors=%d",
		result.Probed, result.Succeeded, result.Failed, result.Traced, result.RowsWritten, result.Errors)

	// 一次都没写成功、但也没探测任何目标，说明文件层面就有问题。
	if result.Errors > 0 && result.RowsWritten == 0 {
		return result, fmt.Errorf("could not write any result to %s (%d failures); check disk space and permissions",
			store.Path(), result.Errors)
	}
	return result, nil
}

// runTracePhase 对成功的目标逐个跟踪，并把线路信息写进 CSV。
func (s *Service) runTracePhase(
	ctx context.Context,
	store *csvstore.Store,
	engine *trace.NextTraceEngine,
	targets []model.Target,
	successful []string,
	opts CSVScanOptions,
	result *CSVScanResult,
) {
	// 把 ID 映射回 Target（跟踪引擎要的是 Target）。
	byID := make(map[string]model.Target, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
	}

	for index, id := range successful {
		if ctx.Err() != nil {
			result.Interrupted = true
			return
		}

		target, ok := byID[id]
		if !ok {
			continue
		}

		traceResult, traceErr := engine.Trace(ctx, target)
		result.Traced++
		if traceErr != nil {
			result.Errors++
			s.log("scan: tracing %s failed: %v", id, traceErr)
			if opts.Progress != nil {
				opts.Progress(ProgressEvent{
					Phase: "trace", Completed: index + 1, Total: len(successful),
					CurrentTarget: id,
				})
			}
			continue
		}
		if traceResult.Success {
			result.TracedOK++
		}

		if opts.OnTrace != nil {
			opts.OnTrace(id, traceResult)
		}

		if err := store.Append(traceRow(target, traceResult)); err != nil {
			result.Errors++
			s.log("scan: writing trace row for %s failed: %v", id, err)
		} else {
			result.RowsWritten++
		}

		if opts.Progress != nil {
			opts.Progress(ProgressEvent{
				Phase: "trace", Completed: index + 1, Total: len(successful),
				Success: result.TracedOK, Failed: result.Traced - result.TracedOK,
				CurrentTarget: id,
			})
		}
	}
}

// probeRow 把探测结果转成 CSV 行。
func probeRow(target model.Target, result probe.ProbeResult) csvstore.Row {
	row := csvstore.Row{
		Timestamp:     time.Now().UTC(),
		Target:        target.ID,
		IP:            target.IP,
		Port:          target.Port,
		Success:       result.Success,
		LatencyMS:     result.LatencyMS,
		ErrorType:     string(result.ErrorType),
		ErrorMessage:  result.ErrorMessage,
		ClientVersion: version.Version,
	}
	return row
}

// traceRow 把跟踪结果转成 CSV 行。
//
// 出错时也写一行（Success=false 带原因）："这个目标追不了"
// 同样是有用的信息，静默丢掉会让 CSV 看起来像是漏测了。
func traceRow(target model.Target, result *trace.TraceResult) csvstore.Row {
	if result == nil {
		return csvstore.Row{
			Timestamp:     time.Now().UTC(),
			Target:        target.ID,
			IP:            target.IP,
			Port:          target.Port,
			ErrorType:     string(trace.ErrorTypeOther),
			ErrorMessage:  "trace returned no result",
			ClientVersion: version.Version,
		}
	}

	hops := make([]string, 0, len(result.Hops))
	for _, hop := range result.Hops {
		hops = append(hops, hop.ASN)
	}

	return csvstore.Row{
		Timestamp:     time.Now().UTC(),
		Target:        target.ID,
		IP:            target.IP,
		Port:          target.Port,
		Success:       result.Success,
		ErrorType:     string(result.ErrorType),
		ErrorMessage:  result.ErrorMessage,
		HopCount:      result.HopCount(),
		ASPath:        asnmap.ShortPath(hops),
		Hops:          formatHops(result.Hops),
		ClientVersion: version.Version,
	}
}

// formatHops 把逐跳压成 "1:10.0.0.1;2:203.0.113.4:163" 形式。
//
// 压进一个单元格而不是每跳一行：一条路径 10~30 跳，
// 每跳一行会让同一个目标在 CSV 里重复几十次，表格就没法用了。
func formatHops(hops []trace.Hop) string {
	var b strings.Builder
	for i, hop := range hops {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(fmt.Sprintf("%d:", hop.TTL))
		if strings.TrimSpace(hop.IP) == "" {
			// 超时跳明确标 *，留空会被读成"没有这一跳"。
			b.WriteByte('*')
		} else {
			b.WriteString(hop.IP)
		}
		if name := asnmap.ShortLabel(hop.ASN); name != "" {
			b.WriteByte(':')
			b.WriteString(name)
		}
	}
	return b.String()
}

// probeConfig 组装探测配置。
func probeConfig(opts CSVScanOptions) probe.Config {
	cfg := probe.DefaultConfig()
	if opts.Workers > 0 {
		cfg.Workers = opts.Workers
	}
	if opts.Timeout > 0 {
		cfg.Timeout = opts.Timeout
	}
	return cfg
}

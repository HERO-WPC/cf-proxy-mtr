package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
	"github.com/cf-route-tester/cf-route-tester/internal/worker"
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
	//
	// 它只用于"当前正在测哪个"这类实时状态展示，
	// 不该拿来写日志行——那会让每个目标的日志变成两条。
	OnTarget func(target string)

	// OnProbe 在每个目标探测**完成后**调用，携带延迟与连通性。
	//
	// 进度回调只给"完成 37/100"这种聚合数字，而使用者真正想知道的是
	// "这个 IP 通不通、多少毫秒、在哪儿"——那需要逐条结果，
	// 因此单独一个回调。
	OnProbe func(outcome ProbeOutcome)

	// OnTrace 在一条线路跟踪完成后调用（可为 nil，**会并发调用**）。
	OnTrace func(outcome TraceOutcome)

	// ASNPrefix 按"线路的 IP 段"识别路径走了哪些线路。
	//
	// 为 nil 时用下一项的参数按需创建一个；两者都为空则整段跳过。
	ASNPrefix *asnprefix.Resolver

	// ASNPrefixOptions 是创建 ASNPrefix 的参数（缓存目录、TTL 等）。
	ASNPrefixOptions asnprefix.Options

	// NoASNPrefix 为真时完全不用前缀匹配。
	NoASNPrefix bool

	// probeOverride 允许测试替换探测配置（并发性测试需要
	// 一个"每个目标都恰好耗时 timeout"的确定性拨号器）。
	//
	// 刻意不导出：它是测试接缝，不是使用者的选项。
	probeOverride *probe.Config

	// traceEngineOverride 允许测试注入一个跟踪引擎。
	//
	// 存在的理由：跟踪阶段的并发性与"每行拿到就写盘"必须被测试守住，
	// 而真实 nexttrace 在测试里既慢又依赖本机权限（TCP/UDP 模式
	// 需要管理员 + WinDivert）。没有这个接缝，那段代码就是零覆盖——
	// 事实上一度就是如此。
	//
	// 同样刻意不导出。
	traceEngineOverride *trace.NextTraceEngine
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

		// 数据源与 PoW 源也在这里校验，而不是等到跟踪阶段。
		//
		// 放到跟踪阶段的话，写错数据源名的后果是"先测完所有目标、
		// 写好 CSV，然后报跟踪被跳过"——那看起来像引擎没装，
		// 排查方向完全错。真正的问题是参数写错。
		//
		// 这个位置同时覆盖命令行与图形界面（两边都走 RunCSVScan）。
		if _, err := trace.DataProvider(opts.TraceConfig.DataProvider).Normalize(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		if _, err := trace.PowProvider(opts.TraceConfig.PowProvider).Normalize(); err != nil {
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

	// ---- 3.5) 提前开始抓线路前缀，与探测**并行** ----
	//
	// 放在这里而不是跟踪阶段开始时：抓取本身已经并发，但如果在
	// 跟踪前才启动，TCP 扫完之后仍要干等几秒——使用者看到的就是
	// "TCP 秒完，然后卡住"。实测串行抓取要 57 秒，那段等待非常明显。
	//
	// 真跑一轮上万个目标时 TCP 阶段要几分钟，而抓取只要几秒，
	// 并行之后感知上的停顿是零。
	//
	// 只在确实要跟踪时才预热：不跟踪却去联网是白费。
	warmer := startPrefixWarmup(ctx, opts, s.log)

	// ---- 4) 探测 ----
	result := &CSVScanResult{
		OutputPath:      store.Path(),
		Targets:         len(targets),
		SourceURL:       meta.URL,
		SourceFromCache: fromCache,
		StartedAt:       time.Now().UTC(),
	}

	// ---- 4) 探测（并发） ----
	//
	// **必须走 Runner 的 worker 池**。这里曾经是一个 `for targets` 循环
	// 直接调用 prober.Probe：语义上没错，但完全串行——60 个目标实测
	// 32 秒，而 100 并发的预期是 2~3 秒。更糟的是 CLI 照样打印
	// "workers: 100"，让使用者以为并发已经生效。
	//
	// 结果通过 channel 回来，所以每拿到一条就立刻写盘，
	// "中断不丢已完成结果"的性质保持不变。
	runner := probe.NewRunner(probe.RunnerConfig{
		Probe: probeConfig(opts),
		OnTarget: func(target model.Target) {
			if opts.OnTarget != nil {
				opts.OnTarget(target.ID)
			}
		},
	})

	handle := runner.Start(ctx, targets)

	// 探测成功的目标（后面要跟踪的那些）。
	successful := make([]string, 0, len(targets))

	// 只需要 ID → Target 的映射，供写行与跟踪阶段使用。
	byID := make(map[string]model.Target, len(targets))
	for _, target := range targets {
		byID[target.ID] = target
	}

	completed := 0
	for probeResult := range handle.Results() {
		target, ok := byID[probeResult.TargetID]
		if !ok {
			// 理论上不会发生；真发生了要如实记录，
			// 而不是写一行不知道是谁的结果。
			s.log("scan: probe result for unknown target %q dropped", probeResult.TargetID)
			continue
		}

		completed++
		result.Probed++
		if probeResult.Success {
			result.Succeeded++
			successful = append(successful, probeResult.TargetID)
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

		// 逐条结果的日志回调（调用方通常用它写"这个 IP 通不通、多少毫秒"）。
		if opts.OnProbe != nil {
			opts.OnProbe(ProbeOutcome{
				Target:       probeResult.TargetID,
				IP:           target.IP,
				Port:         target.Port,
				Landing:      landing(target),
				Success:      probeResult.Success,
				LatencyMS:    probeResult.LatencyMS,
				ErrorType:    string(probeResult.ErrorType),
				ErrorMessage: probeResult.ErrorMessage,
			})
		}

		if opts.Progress != nil {
			opts.Progress(ProgressEvent{
				Phase:         "probe",
				Completed:     completed,
				Total:         len(targets),
				Success:       result.Succeeded,
				Failed:        result.Failed,
				CurrentTarget: target.ID,
			})
		}
	}

	// 收尾统计（也负责等待全部 worker 退出）。
	probeStats := handle.Wait()

	// 用到渠道丢弃的目标（消费者提前退出）：如实计入"未测"，
	// 否则汇总里的数字加起来对不上总数。
	if probeStats.Dropped > 0 {
		s.log("scan: %d probe result(s) were dropped (consumer stopped early)", probeStats.Dropped)
	}
	if ctx.Err() != nil {
		result.Interrupted = true
	}

	// ---- 5) 线路跟踪（只对成功的目标） ----
	if opts.Trace && !result.Interrupted && len(successful) > 0 {
		engine, engineErr := s.resolveTraceEngine(ctx, opts)
		if engineErr != nil {
			// 引擎不可用**不算扫描失败**：TCP 结果已经写进 CSV 了。
			// 如实记录，让调用方展示原因。
			s.log("scan: trace engine unavailable: %v", engineErr)
			result.TraceUnavailable = engineErr.Error()
		} else {
			// 取预热结果。通常此时已经就绪（探测阶段用掉了更多时间），
			// 因此这里不会阻塞；只有极小规模的扫描才可能真等一会儿。
			prefixes := awaitPrefixWarmup(warmer, opts, s.log)
			s.runTracePhase(ctx, store, engine, prefixes, targets, successful, opts, result)
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

// runTracePhase 对成功的目标做线路跟踪，并把线路信息写进 CSV。
//
// **并发执行**：一次 traceroute 要十几秒（实测平均 17 秒），
// 串行跟踪 10 个目标就是近 3 分钟。每个目标各自启动一个外部进程，
// 彼此完全独立，因此用有界 worker 池并行——并发度由
// `--trace-workers` 控制（默认 10），刻意远低于 TCP 的 100：
// 每个 worker 都要 fork 一个进程，且多数跟踪模式需要管理员权限
// 与 WinDivert 驱动，开太大只会互相拖慢。
func (s *Service) runTracePhase(
	ctx context.Context,
	store *csvstore.Store,
	engine *trace.NextTraceEngine,
	prefixes *asnprefix.Resolver,
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

	workers := opts.TraceConfig.Workers
	if workers <= 0 {
		workers = trace.DefaultWorkers
	}
	total := len(successful)

	// 队列大小取 2×workers：够让 worker 一直有活干，
	// 又不会在取消时留下一大堆没人处理的待办。
	queueSize := workers * 2

	// 进度的共享计数：worker 并发更新，必须加锁。
	var (
		mu        sync.Mutex
		completed int
		successN  int
		failN     int
	)

	emitProgress := func(id string) {
		if opts.Progress == nil {
			return
		}
		mu.Lock()
		done, ok, bad := completed, successN, failN
		mu.Unlock()
		opts.Progress(ProgressEvent{
			Phase: "trace", Completed: done, Total: total,
			Success: ok, Failed: bad, CurrentTarget: id,
		})
	}

	stats := worker.Run(ctx, workers, queueSize,
		func(ctx context.Context, send func(string) bool) int {
			sent := 0
			for _, id := range successful {
				if !send(id) {
					break
				}
				sent++
			}
			return sent
		},
		func(ctx context.Context, id string, _ func(traceOutcome)) error {
			target, ok := byID[id]
			if !ok {
				return nil
			}

			traceResult, traceErr := engine.Trace(ctx, target)

			mu.Lock()
			result.Traced++
			completed++
			switch {
			case traceErr != nil:
				failN++
			case traceResult != nil && traceResult.Success:
				successN++
			default:
				failN++
			}
			mu.Unlock()

			if traceErr != nil {
				mu.Lock()
				result.Errors++
				mu.Unlock()
				s.log("scan: tracing %s failed: %v", id, traceErr)
				emitProgress(id)
				return nil
			}

			if opts.OnTrace != nil {
				// 并发调用：多个 worker 会同时进入这里，
				// 回调实现必须自己保证线程安全。
				opts.OnTrace(TraceOutcome{
					Target:       id,
					Landing:      landing(target),
					HopCount:     traceResult.HopCount(),
					Route:        routeSummary(traceResult, prefixes),
					Success:      traceResult.Success,
					ErrorType:    string(traceResult.ErrorType),
					ErrorMessage: traceResult.ErrorMessage,
				})
			}

			// 每行拿到就立刻写盘，与探测阶段同样的性质：
			// 中断时已完成的线路不会丢。
			if err := store.Append(traceRow(target, traceResult, prefixes)); err != nil {
				mu.Lock()
				result.Errors++
				mu.Unlock()
				s.log("scan: writing trace row for %s failed: %v", id, err)
			} else {
				mu.Lock()
				result.RowsWritten++
				mu.Unlock()
			}

			emitProgress(id)
			return nil
		},
		func(traceOutcome) {},
	)

	// 收尾：把共享计数写回结果（这些字段在并发期间只被锁保护地更新）。
	mu.Lock()
	result.TracedOK = successN
	result.Interrupted = ctx.Err() != nil
	mu.Unlock()

	if stats.Skipped > 0 {
		s.log("scan: %d target(s) were not traced because the scan stopped early", stats.Skipped)
	}
}

// resolveTraceEngine 取得跟踪引擎：优先用注入的（测试），否则按配置构造。
func (s *Service) resolveTraceEngine(ctx context.Context, opts CSVScanOptions) (*trace.NextTraceEngine, error) {
	if opts.traceEngineOverride != nil {
		return opts.traceEngineOverride, nil
	}
	return s.buildTraceEngine(ctx, opts.TraceConfig)
}

// startPrefixWarmup 在后台开始抓线路前缀，立即返回。
//
// 返回 nil 表示不需要（没开跟踪、明确关掉、或调用方已注入了解析器）。
func startPrefixWarmup(ctx context.Context, opts CSVScanOptions, logf func(string, ...any)) *asnprefix.Warmer {
	if opts.NoASNPrefix || opts.ASNPrefix != nil {
		return nil
	}
	if !opts.Trace {
		// 不跟踪就不需要线路名，别白联网。
		return nil
	}

	prefixOpts := opts.ASNPrefixOptions
	if prefixOpts.Logf == nil {
		prefixOpts.Logf = logf
	}
	return asnprefix.Warm(ctx, prefixOpts)
}

// awaitPrefixWarmup 取预热好的前缀解析器。
//
// 会等待抓取完成——但因为在探测之前就启动了，正常情况下
// 这里已经就绪，等待时间为零。
func awaitPrefixWarmup(warmer *asnprefix.Warmer, opts CSVScanOptions, logf func(string, ...any)) *asnprefix.Resolver {
	// 调用方自己注入的解析器优先（测试用）。
	if opts.ASNPrefix != nil {
		return opts.ASNPrefix
	}
	if opts.NoASNPrefix || warmer == nil {
		return nil
	}

	if !warmer.Ready() {
		// 只有在"探测阶段比抓取还快"时才会走到这里（例如只测两个目标）。
		// 如实说明，免得使用者以为卡住了。
		logf("scan: 等待线路前缀抓取完成…")
	}
	return warmer.Get()
}

// traceOutcome 是跟踪阶段的"结果类型"。
//
// worker.Run 要求一个结果类型 R，但这个阶段的产物是**直接写进 CSV**
// 的，没有需要交付给消费者的结构化结果。用一个空结构体占位，
// 比为了满足签名而把结果绕一圈再丢弃更诚实。
type traceOutcome struct{}

// ProbeOutcome 是一条 TCP 探测完成后交给界面展示的信息。
//
// 它是"给人看的一行"，因此只带日志需要的东西：
// 目标、落地地区、通不通、多少毫秒、失败原因。
// 结构化数据在 CSV 里，这里的字段刻意保持扁平。
type ProbeOutcome struct {
	// Target 是 "IP:Port"。
	Target string

	// IP / Port 是拆开的形式。
	IP   string
	Port int

	// Landing 是**落地地区**（目标 IP 自身的地理位置），
	// 形如 "US/Illinois/Chicago"；没有数据时是 "-"。
	Landing string

	// Success 表示这次连接是否成功。
	Success bool

	// LatencyMS 是握手耗时；失败时为 0（不展示）。
	LatencyMS float64

	// ErrorType / ErrorMessage 是失败原因（成功时为空）。
	ErrorType    string
	ErrorMessage string
}

// TraceOutcome 是一条线路跟踪完成后交给界面展示的信息。
type TraceOutcome struct {
	// Target 是 "IP:Port"。
	Target string

	// Landing 是落地地区，同上。
	Landing string

	// HopCount 是路径跳数。
	HopCount int

	// Route 是**线路串，含 ASN 编号与线路名称**，
	// 形如 "163(AS4134) > CN2(AS4809) > AS13335"。
	//
	// 用 FullPath 而不是 ShortPath：日志是拿来核对与排查的，
	// 光有"163"无法确认到底是不是 AS4134，而光有编号
	// 又认不出这条线路意味着什么。两者都要。
	Route string

	// Success 表示这次跟踪是否成功拿到路径。
	Success bool

	// ErrorType / ErrorMessage 是失败原因（成功时为空）。
	ErrorType    string
	ErrorMessage string
}

// landing 把目标的地理位置格式化成日志用的一行短语。
func landing(target model.Target) string {
	return target.Location.String()
}

// resolveRoute 返回一条路径经过的线路（ASN 列表，按跳序去重）。
//
// 优先用**前缀匹配**（prefixes 非 nil 且认出了东西）：
// 它按"线路的 IP 段"判断，直接来自 BGP 数据，不依赖任何
// 限流的 GeoIP 服务。命中时给出的就是"走没走 CN2/163/CMI"，
// 而且即使引擎那边配了 disable-geoip 也照样工作。
//
// 前缀匹配没认出任何线路时，退回引擎给的逐跳 ASN：
// 有些路径确实不经过 asnmap 认识的那 30 条线路，
// 而引擎可能有更广的 ASN 覆盖。
//
// CSV 的 as_path 与日志里的线路串都走这个函数，因此两者
// **不会**出现"日志说走 CN2、CSV 说走 163"这种自相矛盾。
func resolveRoute(result *trace.TraceResult, prefixes *asnprefix.Resolver) []string {
	if result == nil {
		return nil
	}

	if prefixes != nil && prefixes.Ready() {
		hopIPs := make([]string, 0, len(result.Hops))
		for _, hop := range result.Hops {
			hopIPs = append(hopIPs, hop.IP)
		}
		if matched := prefixes.ResolvePath(hopIPs); len(matched) > 0 {
			return matched
		}
	}

	return hopASNs(result.Hops)
}

// routeSummary 把线路渲染成**含 ASN 编号**的形式（给日志用）。
func routeSummary(result *trace.TraceResult, prefixes *asnprefix.Resolver) string {
	return asnprefix.PathWithNames(resolveRoute(result, prefixes), asnmap.Name)
}

// hopASNs 取出每一跳的 ASN。
//
// 跳过空值：没有 ASN 的跳（超时、内网）对"线路"这件事没有信息量，
// 留着会让线路串里出现一堆重复的空段。
func hopASNs(hops []trace.Hop) []string {
	out := make([]string, 0, len(hops))
	for _, hop := range hops {
		if strings.TrimSpace(hop.ASN) == "" {
			continue
		}
		out = append(out, hop.ASN)
	}
	return out
}

// probeRow 把探测结果转成 CSV 行。
func probeRow(target model.Target, result probe.ProbeResult) csvstore.Row {
	// 只有成功时才写延迟。
	//
	// probe 包**故意**在失败时也记录"等待了多久"（用来区分
	// "立即被拒"与"等到超时"，那是有价值的诊断信息）。但那是
	// 内部语义，不该原样进结果文件：超时 1 秒的目标会写成
	// latency_ms=1000，看起来像"延迟 1 秒"，而它根本没连上。
	// 那种行会被平均延迟、分位数全部算进去，把统计拉偏。
	latency := 0.0
	if result.Success {
		latency = result.LatencyMS
	}

	return csvstore.Row{
		Timestamp:     time.Now().UTC(),
		Target:        target.ID,
		IP:            target.IP,
		Port:          target.Port,
		Success:       result.Success,
		LatencyMS:     latency,
		ErrorType:     string(result.ErrorType),
		ErrorMessage:  result.ErrorMessage,
		ClientVersion: version.Version,
	}
}

// traceRow 把跟踪结果转成 CSV 行。
//
// 出错时也写一行（Success=false 带原因）："这个目标追不了"
// 同样是有用的信息，静默丢掉会让 CSV 看起来像是漏测了。
//
// prefixes 与日志用的是同一份判断，避免两边说法不一致。
func traceRow(target model.Target, result *trace.TraceResult, prefixes *asnprefix.Resolver) csvstore.Row {
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

	return csvstore.Row{
		Timestamp:    time.Now().UTC(),
		Target:       target.ID,
		IP:           target.IP,
		Port:         target.Port,
		Success:      result.Success,
		ErrorType:    string(result.ErrorType),
		ErrorMessage: result.ErrorMessage,
		HopCount:     result.HopCount(),
		// CSV 只写线路**名称**（如 "CN2 > CMI"）：这一列是要拿去
		// 排序聚合的，越干净越好；编号在日志里给。
		ASPath:        asnmap.ShortPath(resolveRoute(result, prefixes)),
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
	if opts.probeOverride != nil {
		// 测试接缝：完全用给定配置，不再叠加下面的覆盖项，
		// 否则"注入的拨号器"可能被默认值悄悄换掉。
		cfg := *opts.probeOverride
		return cfg
	}

	cfg := probe.DefaultConfig()
	if opts.Workers > 0 {
		cfg.Workers = opts.Workers
	}
	if opts.Timeout > 0 {
		cfg.Timeout = opts.Timeout
	}
	return cfg
}

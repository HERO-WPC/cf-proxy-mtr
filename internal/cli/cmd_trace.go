package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
	"github.com/cf-route-tester/cf-route-tester/internal/worker"
)

// stringList 是一个可重复出现的字符串参数（例如多个 --target）。
type stringList []string

// String 实现 flag.Value。
func (l *stringList) String() string { return strings.Join(*l, ",") }

// Set 实现 flag.Value。
func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*l = append(*l, value)
	return nil
}

// traceParams 是 `cf-route-tester trace` 的参数。
type traceParams struct {
	targets stringList

	binary  string
	mode    string
	workers int
	timeout durationFlag
	limit   int

	// 数据源相关：分别决定 ASN/地区从哪儿来、
	// 以及 NextTrace API 的 PoW 令牌从哪儿取。
	dataProvider string
	powProvider  string

	noDownload  bool
	downloadDir string

	// from / countries / maxLatency 用于"先测 TCP、再挑一批跟踪"：
	//
	//	--from results.csv --country US,DE --max-latency 200ms --limit 10
	//
	// 设了 --from 就从那份 CSV 里挑目标，不再从数据源取列表，
	// 也不再重新测 TCP（延迟数据已经在文件里了）。
	from       string
	countries  countryList
	maxLatency durationFlag

	jsonOut bool
	verbose bool
	quiet   bool

	// source 参数仅在"没有 --target"时用于取目标列表。
	source sourceParams
}

// newTraceCommand 构造 trace 子命令。
func newTraceCommand() Command {
	return Command{
		Name:    "trace",
		Summary: "使用 NextTrace 对 IP:Port 做线路跟踪",
		Usage:   ClientName + " trace --target IP:PORT [flags]",
		Run:     runTrace,
		Flags: func(w io.Writer) {
			var p traceParams
			fs := traceFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nHow the engine is invoked:\n")
			fmt.Fprintf(w, "  nexttrace --json --tcp --port <target's own port> <ip>\n")
			fmt.Fprintf(w, "  端口取自目标本身：1.2.3.4:2053 就传 2053，不会固定成 443。\n")

			fmt.Fprintf(w, "\nWindows note:\n")
			fmt.Fprintf(w, "  TCP/UDP 模式依赖 WinDivert，需要**管理员权限**；\n")
			fmt.Fprintf(w, "  首次使用请先跑一次 `nexttrace --init` 释放 WinDivert 运行时。\n")
			fmt.Fprintf(w, "  没有管理员权限时可以先用 --mode icmp 验证链路是否通。\n")

			fmt.Fprintf(w, "\nFailure classification:\n%s\n", describeTraceErrorTypes())
		},
	}
}

// traceFlagSet 构造 trace 的 FlagSet。
func traceFlagSet(p *traceParams) *flag.FlagSet {
	fs := newFlagSet("trace")

	fs.Var(&p.targets, "target", "要跟踪的 IP:Port（可重复；不传则取目标列表）")
	fs.StringVar(&p.binary, "binary", trace.DefaultBinary, "nexttrace 可执行文件路径或名字")
	fs.StringVar(&p.mode, "mode", string(trace.ModeTCP), "跟踪模式：tcp / icmp / udp")
	fs.StringVar(&p.dataProvider, "data-provider", "",
		"GeoIP 数据源。留空=用 NextTrace 默认。可选："+providerList())
	fs.BoolVar(&p.noDownload, "no-download", false,
		"不要在 nexttrace 缺失时自动下载（默认会自动下载到程序目录的 data/bin）")
	fs.StringVar(&p.downloadDir, "download-dir", "",
		"自动下载 nexttrace 的落点（留空=程序目录下的 data/bin）")
	fs.StringVar(&p.powProvider, "pow-provider", "",
		"NextTrace API v3 的 PoW 令牌源（仅 --data-provider NextTrace-API 时生效），可选："+powProviderList())
	fs.IntVar(&p.workers, "workers", trace.DefaultWorkers,
		"跟踪并发数（上限 "+itoa(trace.MaxWorkers)+"；每个 worker 会启动一个进程）")
	fs.Var(&p.timeout, "timeout", "单个跟踪超时（默认 "+trace.DefaultTimeout.String()+"）")
	fs.IntVar(&p.limit, "limit", 0, "取目标列表的前 N 个（0 表示全部）")

	fs.StringVar(&p.from, "from", "",
		"从这份结果 CSV 里挑目标（配合 --country / --max-latency / --limit），只跟踪、不重测 TCP")
	fs.Var(&p.countries, "target-country",
		"只挑这些国家的目标（可重复，或逗号分隔；留空=不限）")
	fs.Var(&p.maxLatency, "max-latency",
		"只挑延迟不超过它的目标，例如 200ms（留空=不限）")

	fs.BoolVar(&p.jsonOut, "json", false, "把每条结果作为一行 JSON 写到 stdout")
	fs.BoolVar(&p.verbose, "verbose", false, "为每个目标打印路径跳表")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出汇总")

	// 目标列表参数：仅在没用 --target 时才需要。
	registerSourceFlags(fs, &p.source)
	return fs
}

// runTrace 实现 `cf-route-tester trace`。
//
// 本命令是**诊断式**的：对指定目标做一次线路跟踪并打印结果。
// 把跟踪结果落库属于扫描流程（scan --trace），由 scheduler 统一负责
// ——那里才有会话、采集者与去重键的上下文。
func runTrace(env *Env, args []string) error {
	var p traceParams
	fs := traceFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if p.limit < 0 {
		return usageError("--limit must not be negative")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 0) 从已有结果里挑目标：先测 TCP、再挑一批跟踪的那条流程。
	//
	// 走 service.RunTraceSelection 而不是本文件里的跟踪循环，
	// 是为了让命令行与图形界面**是同一条实现**——两边各写一套
	// 选取与跟踪逻辑，迟早会在"什么算符合条件的行"上分歧。
	if strings.TrimSpace(p.from) != "" {
		return runTraceFromSelection(ctx, env, p)
	}

	// 1) 目标列表。
	targets, err := resolveTraceTargets(ctx, p, env)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return usageError("no targets to trace: pass --target IP:PORT (repeatable) " +
			"or provide a target list via --cache / --url")
	}

	// 2) 引擎。
	//
	// 引擎不可用只终止**本命令**。TCP Probe / scan 完全不依赖它，
	// 这正是需求第 25 条要求的解耦；错误信息里已带安装提示。
	engine, err := buildTraceEngine(ctx, p)
	if err != nil {
		return err
	}

	if !p.quiet {
		printTraceHeader(env.Stderr, engine, targets, p)
	}

	// 3) 并发跟踪。
	started := time.Now()
	results, notTraced := runTraceBatch(ctx, engine, targets, p, env)
	elapsed := time.Since(started)

	// 4) 输出。
	if p.jsonOut {
		writer := bufio.NewWriter(env.Stdout)
		for _, result := range results {
			if err := writeTraceJSONLine(writer, result); err != nil {
				fmt.Fprintln(env.Stderr, "Error: writing json output: "+err.Error())
				break
			}
		}
		if err := writer.Flush(); err != nil {
			fmt.Fprintln(env.Stderr, "Error: flushing json output: "+err.Error())
		}
	}

	// --verbose 与 --quiet 是**正交**的：前者决定"要不要逐条列出"，
	// 后者决定"要不要进度与头部信息"。
	// 因此这里不再用 !p.quiet 去抑制它——否则
	// `trace --verbose --quiet`（很常见的组合：要路径、不要噪音）
	// 会什么都不打印，而用户明明要求了详细输出。
	if p.verbose {
		for _, result := range results {
			printTracePath(env.Stderr, result)
		}
	}

	stats := summarizeTraces(results)
	if !p.jsonOut || !p.quiet {
		printTraceSummary(env.Stdout, stats, elapsed, p, notTraced)
	}
	return nil
}

// resolveTraceTargets 得到要跟踪的目标列表。
//
// 两种来源：
//   - 显式 --target（可重复）：手动排查单个 IP 时用；
//   - 目标列表（缓存/网络）：与 probe / scan 一致。
func resolveTraceTargets(ctx context.Context, p traceParams, env *Env) ([]model.Target, error) {
	if len(p.targets) > 0 {
		out := make([]model.Target, 0, len(p.targets))
		for _, raw := range p.targets {
			// 允许只写 IP（默认 443）；目标里写明的端口优先。
			target, err := probe.ParseTarget(raw, 443)
			if err != nil {
				return nil, usageError("invalid --target %q: %v", raw, err)
			}
			out = append(out, target)
		}
		return model.DedupTargets(out), nil
	}

	res, err := loadTargets(ctx, p.source)
	if err != nil {
		return nil, err
	}
	targets := res.Targets
	if p.limit > 0 && p.limit < len(targets) {
		targets = targets[:p.limit]
	}
	if !p.quiet {
		fmt.Fprintf(env.Stderr, "source:    %s (%d target(s))\n", res.Meta.URL, len(targets))
	}
	return model.DedupTargets(targets), nil
}

// buildTraceEngine 构造 NextTrace 引擎。
func buildTraceEngine(ctx context.Context, p traceParams) (*trace.NextTraceEngine, error) {
	mode, err := trace.Mode(p.mode).Normalize()
	if err != nil {
		return nil, usageError("%v", err)
	}

	opts := trace.EngineOptions{
		BinaryPath: p.binary,
		Mode:       mode,
		// 找不到就自动下载；显式指定了路径或加了 --no-download 则不下载
		// （引擎自己会判断"显式路径"这件事）。
		AutoDownload: !p.noDownload,
		DownloadDir:  p.downloadDir,
		// 数据源与 PoW 源在这里原样传下去，由引擎构造时校验。
		// 非法值必须变成**用法错误**（而不是让 nexttrace 悄悄换源）。
		DataProvider: trace.DataProvider(p.dataProvider),
		PowProvider:  trace.PowProvider(p.powProvider),
	}
	if p.timeout.set {
		opts.Timeout = p.timeout.d
	}

	engine, err := trace.NewNextTraceEngine(ctx, opts)
	if err != nil {
		// "数据源名字写错"是使用者能自己修的问题，因此归为用法错误；
		// 而"引擎找不到"不是，保持原样（调用方会据此只禁用跟踪）。
		if _, providerErr := trace.DataProvider(p.dataProvider).Normalize(); providerErr != nil {
			return nil, usageError("%v", providerErr)
		}
		if _, powErr := trace.PowProvider(p.powProvider).Normalize(); powErr != nil {
			return nil, usageError("%v", powErr)
		}
		return nil, err
	}
	return engine, nil
}

// runTraceBatch 用共享的有界 worker pool 并发跟踪。
//
// 为什么用同一套池子：trace 与 probe 的并发需求完全一致——
// 固定并发、严格有界、可取消、结果边产生边交付。
// 唯一的区别是"每个 job 做什么"。
//
// 返回结果按**输入顺序**排列，以及"未能跟踪的目标数"。
func runTraceBatch(
	ctx context.Context,
	engine *trace.NextTraceEngine,
	targets []model.Target,
	p traceParams,
	env *Env,
) ([]*trace.TraceResult, int) {

	slots := make([]*trace.TraceResult, len(targets))

	var (
		completed int
		success   int
		failed    int
	)
	progress := newProgressPrinter(env.Stderr, len(targets), p.quiet)

	stats := worker.Run(ctx, clampTraceWorkers(p.workers), 0,
		func(ctx context.Context, send func(int) bool) int {
			sent := 0
			for i := range targets {
				if !send(i) {
					break
				}
				sent++
			}
			return sent
		},
		func(ctx context.Context, index int, emit func(*trace.TraceResult)) error {
			target := targets[index]

			result, err := engine.Trace(ctx, target)
			if err != nil {
				// 引擎层面不可用（例如二进制在运行中消失）：
				// 构造一个带分类的结果，保证每个目标都有交代，
				// 而不是静默少一条让汇总对不上。
				result = &trace.TraceResult{
					TargetID:      target.String(),
					IP:            target.IP,
					Port:          target.Port,
					Engine:        engine.Name(),
					EngineVersion: engine.Version(),
					Mode:          engine.Mode,
					ErrorType:     trace.ErrorTypeOther,
					ErrorMessage:  err.Error(),
					Timestamp:     time.Now().UTC(),
				}
			}

			slots[index] = result
			emit(result)
			return nil
		},
		func(result *trace.TraceResult) {
			completed++
			if result.Success {
				success++
			} else {
				failed++
			}
			progress.update(completed, success, failed)
		},
	)
	progress.done()

	// 取消时 worker 会停止消费，未处理的槽位为 nil。
	out := make([]*trace.TraceResult, 0, len(slots))
	for _, result := range slots {
		if result != nil {
			out = append(out, result)
		}
	}
	return out, stats.Skipped
}

// clampTraceWorkers 把并发限制在安全范围内。
//
// trace 的每个 worker 都会启动一个外部进程，因此上限远小于 probe
// （需求第 31 条：trace 并发必须独立且更小）。
func clampTraceWorkers(workers int) int {
	if workers <= 0 {
		return 1
	}
	if workers > trace.MaxWorkers {
		return trace.MaxWorkers
	}
	return workers
}

// summarizeTraces 汇总一批跟踪结果。
func summarizeTraces(results []*trace.TraceResult) trace.Stats {
	stats := trace.Stats{Total: len(results)}
	for _, result := range results {
		stats.Add(result)
	}
	return stats
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printTraceHeader 打印跟踪前的说明。
func printTraceHeader(w io.Writer, engine *trace.NextTraceEngine, targets []model.Target, p traceParams) {
	fmt.Fprintf(w, "engine:    %s", engine.Name())
	if v := engine.Version(); v != "" {
		fmt.Fprintf(w, " %s", v)
	}
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "binary:    %s\n", engine.Path())
	fmt.Fprintf(w, "mode:      %s\n", engine.Mode)
	fmt.Fprintf(w, "targets:   %d\n", len(targets))
	fmt.Fprintf(w, "workers:   %d (each worker spawns one process)\n", clampTraceWorkers(p.workers))
	fmt.Fprintf(w, "timeout:   %s per target\n", traceTimeout(p))
	fmt.Fprintln(w)
}

// traceTimeout 返回生效的超时时间。
func traceTimeout(p traceParams) time.Duration {
	if p.timeout.set {
		return p.timeout.d
	}
	return trace.DefaultTimeout
}

// printTracePath 打印单个目标的路径。
func printTracePath(w io.Writer, result *trace.TraceResult) {
	if result == nil {
		return
	}
	if !result.Success {
		fmt.Fprintf(w, "%s  FAILED  %s: %s\n", result.TargetID, result.ErrorType, result.ErrorMessage)
		return
	}

	fmt.Fprintf(w, "%s  %d hops (%.1f ms)\n", result.TargetID, result.HopCount(), result.DurationMS)
	for _, hop := range result.Hops {
		if hop.Timeout {
			fmt.Fprintf(w, "  %2d  *\n", hop.TTL)
			continue
		}
		line := fmt.Sprintf("  %2d  %-39s %8.2f ms", hop.TTL, hop.IP, hop.MinRTT())

		// ASN 后面跟上线路名称：`AS4134` 对多数人没有意义，
		// 而 `AS4134 (163)` 一眼就知道走的是电信普通出口还是 CN2。
		// 未知 ASN 只显示编号，不编造名称。
		if hop.ASN != "" {
			line += "  " + asnmap.Label(hop.ASN)
		}
		if hop.ASOrganization != "" {
			line += "  " + truncateForDisplay(hop.ASOrganization, 32)
		}
		fmt.Fprintln(w, line)
	}

	// 路径汇总：哪几家、按什么顺序，比逐跳列表更容易看出问题。
	if path := formatASPathSummary(result.Hops); path != "" {
		fmt.Fprintf(w, "\nroute:      %s\n", path)
	}
}

// printTraceSummary 打印汇总。
func printTraceSummary(w io.Writer, stats trace.Stats, elapsed time.Duration, p traceParams, notTraced int) {
	fmt.Fprintf(w, "traced:      %d target(s)\n", stats.Completed)
	fmt.Fprintf(w, "success:     %d (%.1f%%)\n", stats.Success, stats.SuccessRate()*100)
	fmt.Fprintf(w, "failed:      %d (%.1f%%)\n", stats.Failed, (1-stats.SuccessRate())*100)
	fmt.Fprintf(w, "elapsed:     %s", elapsed.Round(time.Millisecond))
	if stats.Completed > 0 && elapsed > 0 {
		fmt.Fprintf(w, " (%.1f/s)", float64(stats.Completed)/elapsed.Seconds())
	}
	fmt.Fprintln(w)

	if stats.Success > 0 {
		fmt.Fprintf(w, "\naverage hops: %.1f\n", stats.AverageHops())
	}

	if len(stats.ErrorCounts) > 0 {
		fmt.Fprintf(w, "\nfailures by type:\n")
		for _, line := range formatTraceErrorCounts(stats.ErrorCounts, p.verbose) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	// 环境问题与路径问题必须分开说清，否则用户会去排查网络。
	for kind := range stats.ErrorCounts {
		if !kind.CountsTowardPathQuality() {
			fmt.Fprintf(w, "\nnote: %q means the trace could not be performed\n"+
				"      (environment / permission), not that the path is bad.\n", kind)
			break
		}
	}

	if notTraced > 0 {
		fmt.Fprintf(w, "\nwarning: tracing was interrupted; %d target(s) were not traced\n", notTraced)
	}
}

// formatTraceErrorCounts 把错误分类计数格式化成"按数量降序"的列表。
func formatTraceErrorCounts(counts map[trace.ErrorType]int, verbose bool) []string {
	type kv struct {
		kind trace.ErrorType
		n    int
	}
	items := make([]kv, 0, len(counts))
	for kind, n := range counts {
		items = append(items, kv{kind, n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].kind < items[j].kind
	})

	const maxShown = 8
	shown := items
	rest := 0
	if !verbose && len(items) > maxShown {
		shown = items[:maxShown]
		for _, it := range items[maxShown:] {
			rest += it.n
		}
	}

	out := make([]string, 0, len(shown)+1)
	for _, it := range shown {
		out = append(out, fmt.Sprintf("%-24s %d", it.kind+":", it.n))
	}
	if rest > 0 {
		out = append(out, fmt.Sprintf("%-24s %d", "... others:", rest))
	}
	return out
}

// formatTraceErrorCountLines 是 formatTraceErrorCounts 的通用入口，
// 供 scan 的两级汇总复用（避免两处各写一份排序与截断逻辑）。
func formatTraceErrorCountLines(counts map[trace.ErrorType]int, verbose bool) []string {
	return formatTraceErrorCounts(counts, verbose)
}

// truncateForDisplay 截断过长的展示文本。
func truncateForDisplay(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}

// describeTraceErrorTypes 返回跟踪失败分类的说明（用于 --help）。
func describeTraceErrorTypes() string {
	descriptions := []struct {
		kind trace.ErrorType
		text string
	}{
		{trace.ErrorTypeTimeout, "跟踪超时"},
		{trace.ErrorTypeNotFound, "找不到 nexttrace（环境问题，不是路径问题）"},
		{trace.ErrorTypePermission, "权限不足（Windows 的 TCP/UDP 需要管理员权限 + WinDivert）"},
		{trace.ErrorTypeExitCode, "引擎以非零退出码结束"},
		{trace.ErrorTypeParse, "输出无法解析（可能是引擎版本变化）"},
		{trace.ErrorTypeCanceled, "本次运行被中断（不计入路径质量）"},
		{trace.ErrorTypeInvalidTarget, "目标数据非法（不计入路径质量）"},
		{trace.ErrorTypeOther, "无法归类的失败"},
	}

	var b strings.Builder
	for _, d := range descriptions {
		b.WriteString(fmt.Sprintf("  %-20s %s\n", string(d.kind)+":", d.text))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// JSONL 输出
// ---------------------------------------------------------------------------

// traceJSONRecord 是单条跟踪结果在 JSONL 中的形状。
//
// 与 probe 的 JSONL 一致：自带 schema_version，且**不含任何采集者信息**
// （那属于导出/上传层，Phase 9/10）。
type traceJSONRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ClientVersion string `json:"client_version"`

	TargetID string `json:"target_id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	Mode          string `json:"mode"`
	Protocol      string `json:"protocol"`

	Success    bool    `json:"success"`
	DurationMS float64 `json:"duration_ms"`
	HopCount   int     `json:"hop_count"`

	Hops []traceJSONHop `json:"hops"`

	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	Timestamp string `json:"timestamp"`
}

// traceJSONHop 是 JSONL 里的一跳。
type traceJSONHop struct {
	TTL            int       `json:"ttl"`
	IP             string    `json:"ip,omitempty"`
	Hostname       string    `json:"hostname,omitempty"`
	RTTMS          []float64 `json:"rtt_ms,omitempty"`
	Timeout        bool      `json:"timeout,omitempty"`
	ASN            string    `json:"asn,omitempty"`
	ASOrganization string    `json:"as_organization,omitempty"`
	Country        string    `json:"country,omitempty"`
	Province       string    `json:"province,omitempty"`
	City           string    `json:"city,omitempty"`
}

// writeTraceJSONLine 把一条结果写成一行 JSON。
func writeTraceJSONLine(w io.Writer, result *trace.TraceResult) error {
	if result == nil {
		return nil
	}

	record := traceJSONRecord{
		SchemaVersion: version.SchemaVersion,
		ClientVersion: version.Version,
		TargetID:      result.TargetID,
		IP:            result.IP,
		Port:          result.Port,
		Engine:        result.Engine,
		EngineVersion: result.EngineVersion,
		Mode:          string(result.Mode),
		Protocol:      result.Protocol,
		Success:       result.Success,
		DurationMS:    result.DurationMS,
		HopCount:      result.HopCount(),
		ErrorType:     string(result.ErrorType),
		ErrorMessage:  result.ErrorMessage,
		Timestamp:     result.Timestamp.UTC().Format(time.RFC3339Nano),
	}

	record.Hops = make([]traceJSONHop, 0, len(result.Hops))
	for _, hop := range result.Hops {
		record.Hops = append(record.Hops, traceJSONHop{
			TTL:            hop.TTL,
			IP:             hop.IP,
			Hostname:       hop.Hostname,
			RTTMS:          hop.RTTMS,
			Timeout:        hop.Timeout,
			ASN:            hop.ASN,
			ASOrganization: hop.ASOrganization,
			Country:        hop.Country,
			Province:       hop.Province,
			City:           hop.City,
		})
	}

	blob, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := w.Write(blob); err != nil {
		return err
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

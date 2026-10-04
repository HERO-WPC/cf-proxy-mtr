package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// scanParams 是 `cf-route-tester scan` 的参数。
type scanParams struct {
	source sourceParams

	workers int
	timeout durationFlag
	limit   int

	// out 是结果 CSV 路径；appendOut 为真时追加而不是覆盖。
	//
	// 结果**只写 CSV**：没有数据库、没有会话、没有续测。
	// 每测完一个目标就立刻刷盘，因此中途中断也不丢已完成的结果。
	out       string
	appendOut bool

	identityPath string

	trace bool

	// 第二级（线路跟踪）参数，仅在 --trace 时生效。
	traceBinary  string
	traceMode    string
	traceWorkers int
	traceTimeout durationFlag

	// 数据源相关：分别决定 ASN/地区从哪儿来、
	// 以及 NextTrace API 的 PoW 令牌从哪儿取。
	traceDataProvider string
	tracePowProvider  string

	// 本地 ASN 前缀识别（默认开启）。
	traceNoASNPrefix      bool
	traceASNPrefixDir     string
	traceRefreshASNPrefix bool
	traceASNBulkURL       string

	// nexttrace 缺失时是否自动下载。
	traceNoDownload  bool
	traceDownloadDir string

	// 采集者画像覆盖项。
	collectorCountry   string
	collectorProvince  string
	collectorCity      string
	collectorISP       string
	collectorASN       string
	collectorIPVersion string

	verbose bool
	quiet   bool
}

// newScanCommand 构造 scan 子命令。
func newScanCommand() Command {
	return Command{
		Name:    "scan",
		Summary: "全量扫描：TCP Probe 并把结果实时写入 CSV",
		Usage:   ClientName + " scan [flags]",
		Run:     runScan,
		Flags: func(w io.Writer) {
			var p scanParams
			fs := scanFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\n结果文件（CSV）:\n")
			fmt.Fprintf(w, "  扫描结果**只**写进 CSV，不使用数据库、没有会话、没有续测。\n")
			fmt.Fprintf(w, "  每测完一个目标就立即刷盘，因此中途 Ctrl+C、进程被杀、断电，\n")
			fmt.Fprintf(w, "  已经测完的部分一行都不会丢。\n")
			fmt.Fprintf(w, "\n  --append          追加到已有文件；默认每次覆盖上次结果\n")
		},
	}
}

// scanFlagSet 构造 scan 的 FlagSet。
func scanFlagSet(p *scanParams) *flag.FlagSet {
	fs := newFlagSet("scan")

	registerSourceFlags(fs, &p.source)

	fs.IntVar(&p.workers, "workers", probe.DefaultWorkers, "TCP 并发数（上限 "+itoa(probe.MaxWorkers)+"）")
	fs.Var(&p.timeout, "timeout", "单个 TCP 连接超时（默认 "+probe.DefaultTimeout.String()+"）")
	fs.IntVar(&p.limit, "limit", 0, "只扫描前 N 个目标（0 表示全部，用于快速验证）")

	fs.StringVar(&p.out, "out", "data/results.csv", "结果 CSV 路径（每测完一个目标就写入一行）")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径（collector_id）")

	fs.BoolVar(&p.appendOut, "append", false, "追加到已有结果文件（默认覆盖）")

	fs.BoolVar(&p.trace, "trace", false, "对**探测成功**的目标执行线路跟踪（需要 NextTrace）")
	fs.StringVar(&p.traceBinary, "trace-binary", trace.DefaultBinary,
		"nexttrace 可执行文件路径或名字（--trace 时使用）")
	fs.StringVar(&p.traceMode, "trace-mode", string(trace.ModeTCP),
		"跟踪模式：tcp / icmp / udp（--trace 时使用）")
	fs.StringVar(&p.traceDataProvider, "trace-data-provider", "",
		"线路跟踪的 GeoIP 数据源。留空=自动（本地 ASN 前缀识别启用时用 disable-geoip，"+
			"从而不依赖任何限流服务；否则用 NextTrace-API）。可选："+providerList())
	fs.BoolVar(&p.traceNoASNPrefix, "trace-no-asn-prefix", false,
		"不用本地 ASN 前缀识别线路（默认开启；它是无限、不限流、无需账号的线路识别方式）")
	fs.StringVar(&p.traceASNPrefixDir, "trace-asn-prefix-dir", asnprefix.DefaultDir,
		"ASN 前缀缓存目录")
	fs.BoolVar(&p.traceNoDownload, "trace-no-download", false,
		"不要在 nexttrace 缺失时自动下载（默认会自动下载到程序目录的 data/bin）")
	fs.StringVar(&p.traceDownloadDir, "trace-download-dir", "",
		"自动下载 nexttrace 的落点（留空=程序目录下的 data/bin）")
	fs.StringVar(&p.traceASNBulkURL, "trace-asn-bulk-url", "",
		"ASN 前缀全量表的地址（留空=用内置默认；可指向镜像或本地文件）")
	fs.BoolVar(&p.traceRefreshASNPrefix, "trace-refresh-asn-prefix", false,
		"忽略缓存，重新抓取 ASN 前缀（默认 7 天过期）")
	fs.StringVar(&p.tracePowProvider, "trace-pow-provider", "",
		"线路跟踪的 PoW 令牌源（仅 --data-provider NextTrace-API 时生效），可选："+powProviderList())
	fs.IntVar(&p.traceWorkers, "trace-workers", trace.DefaultWorkers,
		"跟踪并发数（上限 "+itoa(trace.MaxWorkers)+"；每 worker 启动一个进程）")
	fs.Var(&p.traceTimeout, "trace-timeout", "单个跟踪超时（默认 "+trace.DefaultTimeout.String()+"）")

	fs.StringVar(&p.collectorCountry, "country", "", "采集者国家代码（覆盖本地标识）")
	fs.StringVar(&p.collectorProvince, "province", "", "采集者省份（覆盖本地标识）")
	fs.StringVar(&p.collectorCity, "city", "", "采集者城市（覆盖本地标识）")
	fs.StringVar(&p.collectorISP, "isp", "", "采集者运营商（覆盖本地标识）")
	fs.StringVar(&p.collectorASN, "asn", "", "采集者 ASN（覆盖本地标识）")
	fs.StringVar(&p.collectorIPVersion, "ip-version", "", "采集者 IP 版本（覆盖本地标识）")

	fs.BoolVar(&p.verbose, "verbose", false, "显示每个目标的测量结果")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出汇总，不输出进度")
	return fs
}

// describeProfile 把采集者画像渲染成人类可读的一行。
//
// 刻意不用 GroupKey()：那个键固定包含 6 个字段，画像还没检测过时会
// 显示成 "|||||"（分隔符之间全是空），既不美观也看不出问题所在。
// 聚合键必须保持固定形状（那正是它可比的原因），因此这里只改展示。
func describeProfile(profile model.CollectorProfile) string {
	profile.Normalize()

	parts := make([]string, 0, 6)
	for _, value := range []string{
		profile.Country, profile.Province, profile.City,
		profile.ISP, profile.ASN, string(profile.IPVersion),
	} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "(未检测：请先运行 '" + ClientName + " detect --write')"
	}
	return strings.Join(parts, " / ")
}

// buildScanTraceEngine 构造扫描用的线路跟踪引擎。
//
// 与 `trace` 命令共用同一套 EngineOptions，因此行为一致
// （模式校验、超时、版本查询）。
func buildScanTraceEngine(ctx context.Context, p scanParams) (*trace.NextTraceEngine, error) {
	mode, err := trace.Mode(p.traceMode).Normalize()
	if err != nil {
		return nil, err
	}

	opts := trace.EngineOptions{
		BinaryPath: p.traceBinary,
		Mode:       mode,
	}
	if p.traceTimeout.set {
		opts.Timeout = p.traceTimeout.d
	}
	return trace.NewNextTraceEngine(ctx, opts)
}

// indentBlock 给多行文本的每一行加上前缀（用于把错误信息嵌进缩进输出）。
func indentBlock(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// collectorOverrides 把命令行给出的画像整理成覆盖表（只含非空项）。
func (p scanParams) collectorOverrides() map[string]string {
	out := make(map[string]string, 6)
	for key, value := range map[string]string{
		"country":    p.collectorCountry,
		"province":   p.collectorProvince,
		"city":       p.collectorCity,
		"isp":        p.collectorISP,
		"asn":        p.collectorASN,
		"ip-version": p.collectorIPVersion,
	} {
		if strings.TrimSpace(value) != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

// runScan 实现 `cf-route-tester scan`。
func runScan(env *Env, args []string) error {
	var p scanParams
	fs := scanFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if p.limit < 0 {
		return usageError("--limit must not be negative")
	}
	if strings.TrimSpace(p.out) == "" {
		return usageError("--out must not be empty (scan writes its results to a CSV file)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 先校验"用法类"参数，再去做任何有代价的事（下载、建文件）。
	//
	// 顺序很重要：放到后面的话，用户写错 --trace-mode 时会先经历
	// 下载目标列表、创建结果文件，最后才看到那条错误。
	// 这类规则靠这条注释与 cmd_web_test.go / cmd_trace_test.go 的测试守住。
	if p.trace {
		if _, err := trace.Mode(p.traceMode).Normalize(); err != nil {
			return usageError("%v", err)
		}
		// 数据源也在这里校验。
		//
		// 少了这一步，写错数据源名的后果是"跟踪被跳过"——
		// 而那看起来像是引擎没装，排查方向完全错。
		// 真正的问题是参数写错，就该现在说。
		if _, err := trace.DataProvider(p.traceDataProvider).Normalize(); err != nil {
			return usageError("%v", err)
		}
		if _, err := trace.PowProvider(p.tracePowProvider).Normalize(); err != nil {
			return usageError("%v", err)
		}
	}

	if !p.quiet {
		printCSVScanHeader(env.Stderr, p)
	}

	// 把 service 的日志接到 stderr。
	//
	// 此前这里**没有**接 Logf，于是 service 说的话全部丢失——
	// 其中包含重要警告，例如"N/30 ASN 的前缀不可用"。
	// 那条警告正是"线路名为什么少了几条"的唯一线索，
	// 丢掉它等于让使用者对着不完整的线路名猜原因。
	//
	// 代价是 stderr 会多出几行（每轮扫描几条），但那些行
	// 要么说明正在做什么，要么说明出了什么问题。
	var logMu sync.Mutex

	svc := service.New(service.Options{
		IdentityPath: p.identityPath,
		Source:       p.source.toConfig(),
		Logf: func(format string, args ...any) {
			// 跟踪阶段的 worker 会**并发**调用，不加锁会让
			// 两行日志交错成一行乱码。
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(env.Stderr, format+"\n", args...)
		},
	})

	opts := service.CSVScanOptions{
		OutputPath: p.out,
		Append:     p.appendOut,
		Workers:    p.workers,
		Limit:      p.limit,
		Trace:      p.trace,
		TraceConfig: service.TraceOptions{
			Binary:       p.traceBinary,
			Mode:         p.traceMode,
			DataProvider: p.traceDataProvider,
			PowProvider:  p.tracePowProvider,
			// 命令行默认自动下载；--trace-no-download 关掉。
			AutoDownload: !p.traceNoDownload,
			DownloadDir:  p.traceDownloadDir,
		},
		NoASNPrefix: p.traceNoASNPrefix,
		ASNPrefixOptions: asnprefix.Options{
			Dir:     p.traceASNPrefixDir,
			BulkURL: p.traceASNBulkURL,
			TTL:     asnPrefixTTL(p.traceRefreshASNPrefix),
		},
	}

	if p.timeout.set {
		opts.Timeout = p.timeout.d
	}
	if p.traceTimeout.set {
		opts.TraceConfig.Timeout = p.traceTimeout.d
	}
	if p.traceWorkers > 0 {
		opts.TraceConfig.Workers = p.traceWorkers
	}

	printer := newCSVProgressPrinter(env.Stderr, p.quiet)
	opts.Progress = printer.onEvent

	// --verbose：逐条打印每个 IP 的延迟与连通性，以及每条线路的
	// ASN 编号 + 线路名称 + 落地地区。
	//
	// 默认关闭：14000 多个目标逐条打印会把终端刷爆，而"测完了多少"
	// 由进度行表达。要看细节时再开——那时的输出正是排查所需。
	//
	// 顺带说明：这个 flag 在此之前只被声明、从未生效，
	// 帮助里却写着"显示每个目标的测量结果"。
	if p.verbose && !p.quiet {
		reporter := newCSVResultReporter(env.Stderr)
		opts.OnProbe = reporter.onProbe
		opts.OnTrace = reporter.onTrace
	}

	result, err := svc.RunCSVScan(ctx, opts)
	if err != nil {
		return err
	}
	printer.done()

	printCSVScanSummary(env.Stdout, result)
	return nil
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printCSVScanHeader 在扫描开始前说明将要做什么。
func printCSVScanHeader(w io.Writer, p scanParams) {
	fmt.Fprintf(w, "output:      %s", p.out)
	if p.appendOut {
		fmt.Fprintf(w, "（追加）")
	} else {
		fmt.Fprintf(w, "（覆盖）")
	}
	fmt.Fprintln(w)

	if p.limit > 0 {
		fmt.Fprintf(w, "limit:       %d 个目标\n", p.limit)
	}
	fmt.Fprintf(w, "workers:     %d\n", p.workers)
	if p.timeout.set {
		fmt.Fprintf(w, "timeout:     %s\n", p.timeout.d)
	}
	if p.trace {
		fmt.Fprintf(w, "trace:       enabled (mode %s) — 只跟踪 TCP 探测成功的目标\n", p.traceMode)
	} else {
		fmt.Fprintf(w, "trace:       disabled\n")
	}
	fmt.Fprintln(w)
}

// printCSVScanSummary 输出扫描结果摘要。
func printCSVScanSummary(w io.Writer, r *service.CSVScanResult) {
	fmt.Fprintf(w, "\noutput:      %s\n", r.OutputPath)
	fmt.Fprintf(w, "considered:  %d target(s)\n", r.Targets)
	fmt.Fprintf(w, "probed:      %d\n", r.Probed)
	fmt.Fprintf(w, "succeeded:   %d\n", r.Succeeded)
	fmt.Fprintf(w, "failed:      %d\n", r.Failed)
	if r.Traced > 0 {
		fmt.Fprintf(w, "traced:      %d (ok %d)\n", r.Traced, r.TracedOK)
	}
	fmt.Fprintf(w, "rows:        %d written\n", r.RowsWritten)
	fmt.Fprintf(w, "elapsed:     %s\n", r.Elapsed().Round(time.Millisecond))

	if r.Interrupted {
		fmt.Fprintf(w, "\n中断：已经测完的 %d 行都已写入文件，不会丢。\n", r.RowsWritten)
	}
	if r.TraceUnavailable != "" {
		fmt.Fprintf(w, "\ntrace:       SKIPPED\n%s\n", indentBlock(r.TraceUnavailable, "             "))
	}
	if r.Errors > 0 {
		fmt.Fprintf(w, "\nwarning:     %d 次写入失败（检查磁盘空间与权限）\n", r.Errors)
	}
}

// csvProgressPrinter 输出扫描进度。
type csvProgressPrinter struct {
	w     io.Writer
	quiet bool
	last  string
}

// newCSVProgressPrinter 创建进度输出。
func newCSVProgressPrinter(w io.Writer, quiet bool) *csvProgressPrinter {
	return &csvProgressPrinter{w: w, quiet: quiet}
}

// onEvent 处理一次进度事件。
//
// 只按阶段打印**完成**那一行，而不是每个目标一行：
// 一万多个目标会把终端刷爆，而"测到哪了"由百分比表达更清楚。
func (p *csvProgressPrinter) onEvent(event service.ProgressEvent) {
	if p == nil || p.quiet {
		return
	}
	if event.Total <= 0 || event.Completed != event.Total {
		return
	}

	label := "probe"
	if event.Phase == "trace" {
		label = "trace"
	}
	fmt.Fprintf(p.w, "%s completed: %d/%d\n", label, event.Completed, event.Total)
}

// done 结束进度输出（换行收尾）。
func (p *csvProgressPrinter) done() {
	if p == nil || p.quiet {
		return
	}
	// 进度行本身已带换行；这里只是保留一个收尾钩子，
	// 万一将来改成 \r 覆盖式输出，收尾点已经存在。
}

// ---------------------------------------------------------------------------
// --verbose 的逐条结果输出
// ---------------------------------------------------------------------------

// csvResultReporter 打印每个目标的测量结果。
//
// OnTrace 会被多个 worker **并发**调用，因此写入必须串行化：
// 否则两个 goroutine 的输出会交错在同一行里，日志变成乱码。
type csvResultReporter struct {
	mu sync.Mutex
	w  io.Writer
}

// newCSVResultReporter 创建逐条结果输出。
func newCSVResultReporter(w io.Writer) *csvResultReporter {
	return &csvResultReporter{w: w}
}

// onProbe 打印一个 IP 的连通性与延迟。
func (r *csvResultReporter) onProbe(outcome service.ProbeOutcome) {
	location := cliLandingSuffix(outcome.Landing)

	r.mu.Lock()
	defer r.mu.Unlock()

	if outcome.Success {
		fmt.Fprintf(r.w, "连通 %s  %.1f ms%s\n", outcome.Target, outcome.LatencyMS, location)
		return
	}
	reason := outcome.ErrorType
	if reason == "" {
		reason = "failed"
	}
	fmt.Fprintf(r.w, "不通 %s  (%s)%s\n", outcome.Target, reason, location)
}

// onTrace 打印一条线路：ASN 编号 + 线路名称 + 落地地区。
func (r *csvResultReporter) onTrace(outcome service.TraceOutcome) {
	location := cliLandingSuffix(outcome.Landing)

	r.mu.Lock()
	defer r.mu.Unlock()

	if !outcome.Success {
		reason := outcome.ErrorMessage
		if reason == "" {
			reason = outcome.ErrorType
		}
		fmt.Fprintf(r.w, "线路 %s 跟踪失败：%s%s\n", outcome.Target, reason, location)
		return
	}
	if outcome.Route == "" {
		fmt.Fprintf(r.w, "线路 %s：%d 跳（未识别出已知骨干线路）%s\n",
			outcome.Target, outcome.HopCount, location)
		return
	}
	fmt.Fprintf(r.w, "线路 %s：%d 跳，%s%s\n",
		outcome.Target, outcome.HopCount, outcome.Route, location)
}

// cliLandingSuffix 把落地地区格式化成行尾说明（无数据时不加）。
//
// 不加 "落地 -" 这种空标注：它只增加噪声，不提供信息。
func cliLandingSuffix(location string) string {
	trimmed := strings.TrimSpace(location)
	if trimmed == "" || trimmed == "-" {
		return ""
	}
	return "  落地 " + trimmed
}

// asnPrefixTTL 返回前缀缓存的有效期。
//
// --trace-refresh-asn-prefix 时返回一个极小的值，
// 等价于"立即视为过期"，从而强制重新抓取。
//
// 用"极小 TTL"而不是另加一个 force 参数：缓存层已经
// 完整支持过期重抓，多一个开关只会多一条分支要测。
func asnPrefixTTL(refresh bool) time.Duration {
	if refresh {
		return time.Nanosecond
	}
	return asnprefix.DefaultTTL
}

// probeTimeout 返回本次扫描的单目标超时。
func probeTimeout(p scanParams) time.Duration {
	if p.timeout.set {
		return p.timeout.d
	}
	return probe.DefaultTimeout
}

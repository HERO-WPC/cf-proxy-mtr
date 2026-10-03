package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

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
	}

	if !p.quiet {
		printCSVScanHeader(env.Stderr, p)
	}

	svc := service.New(service.Options{
		IdentityPath: p.identityPath,
		Source:       p.source.toConfig(),
	})

	opts := service.CSVScanOptions{
		OutputPath: p.out,
		Append:     p.appendOut,
		Workers:    p.workers,
		Limit:      p.limit,
		Trace:      p.trace,
		TraceConfig: service.TraceOptions{
			Binary: p.traceBinary,
			Mode:   p.traceMode,
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

func probeTimeout(p scanParams) time.Duration {
	if p.timeout.set {
		return p.timeout.d
	}
	return probe.DefaultTimeout
}

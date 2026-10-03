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
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// scanParams 是 `cf-route-tester scan` 的参数。
type scanParams struct {
	source sourceParams

	workers int
	timeout durationFlag
	limit   int

	db           string
	identityPath string

	// 会话控制：三者互斥（--resume / --new / --session）。
	resume     bool
	newSession bool
	sessionID  string

	trace bool

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
		Summary: "全量扫描：TCP Probe 并把结果写入本地数据库（支持 --resume 断点续测）",
		Usage:   ClientName + " scan [flags]",
		Run:     runScan,
		Flags: func(w io.Writer) {
			var p scanParams
			fs := scanFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nSessions:\n")
			fmt.Fprintf(w, "  每次 scan 都是一次测量会话（session）。判断某个目标\n")
			fmt.Fprintf(w, "  是否需要测量，依据的是 (目标, 采集者, 会话) 三元组，\n")
			fmt.Fprintf(w, "  而不是“数据库里有没有这个目标的历史结果”——后者会让\n")
			fmt.Fprintf(w, "  同一个节点永远无法重新测量全部目标。\n")
			fmt.Fprintf(w, "\n  --resume          继续上次未完成的会话，只测没测过的目标\n")
			fmt.Fprintf(w, "  --new             强制开始一个新会话（默认行为）\n")
			fmt.Fprintf(w, "  --session <id>    指定会话 ID（配合 --resume 使用）\n")
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

	fs.StringVar(&p.db, "db", storage.DefaultPath, "SQLite 数据库路径（扫描结果必须落库）")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径（collector_id）")

	fs.BoolVar(&p.resume, "resume", false, "继续上次未完成的会话，只测量尚未完成的目标")
	fs.BoolVar(&p.newSession, "new", false, "强制开始一个新会话（默认）")
	fs.StringVar(&p.sessionID, "session", "", "指定会话 ID（与 --resume 或 --new 配合）")

	fs.BoolVar(&p.trace, "trace", false, "对目标执行线路跟踪（需要 NextTrace，Phase 7 起可用）")
	fs.BoolVar(&p.trace, "trace-all", false, "同 --trace（全量跟踪；与 --trace 互为别名）")

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

	// 会话参数冲突必须明确报错：静默选一个会让用户以为自己在续测，
	// 实际却开了新会话（或相反）。
	if p.resume && p.newSession {
		return usageError("--resume and --new are mutually exclusive")
	}
	if p.limit < 0 {
		return usageError("--limit must not be negative")
	}
	if strings.TrimSpace(p.db) == "" {
		return usageError("--db must not be empty (scan stores its results)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 1) 目标列表（缓存优先，与 fetch/probe 一致）。
	res, err := loadTargets(ctx, p.source)
	if err != nil {
		return err
	}
	targets := res.Targets
	if p.limit > 0 && p.limit < len(targets) {
		targets = targets[:p.limit]
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets to scan (source %s returned an empty list)", res.Meta.URL)
	}

	// 2) 本地匿名标识（随机生成，非硬件指纹）。
	local, err := resolveIdentity(p.identityPath, p.collectorOverrides())
	if err != nil {
		return err
	}

	// 3) 数据库。扫描结果必须落库，因此这里是硬依赖。
	store, err := storage.Open(ctx, storage.Config{Path: p.db})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintln(env.Stderr, "Error: closing database: "+cerr.Error())
		}
	}()

	collectorPK, err := store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), version.Version)
	if err != nil {
		return err
	}

	// 4) 决定会话。
	//
	// --new 会生成新 ID；未指定时也生成新 ID（默认"每次 scan 都是新会话"）。
	// --resume 不指定 ID 时用本地记住的上一个会话。
	sessionID, resume, err := resolveScanSession(ctx, store, collectorPK, p, local)
	if err != nil {
		return err
	}

	// 5) 装配 scheduler。
	cfg := scheduler.Config{
		Probe:       probe.DefaultConfig(),
		CollectorPK: collectorPK,
		SessionID:   sessionID,
		Resume:      resume,
		Trace:       p.trace,
	}
	cfg.Probe.Workers = p.workers
	if p.timeout.set {
		cfg.Probe.Timeout = p.timeout.d
	}

	sched, err := scheduler.New(store, cfg)
	if err != nil {
		return err
	}

	// 目标元数据先入库：测量有外键指向 targets，
	// 而且 UPSERT 是幂等的，重复扫描不会产生重复目标。
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		return err
	}

	if !p.quiet {
		printScanHeader(env.Stderr, res, targets, sched, p, sessionID, resume, local)
	}

	printer := newScanProgressPrinter(env.Stderr, p.quiet)
	sched.SetProgress(printer.onEvent)

	result, err := sched.Run(ctx, targets, version.Version, nil)
	if err != nil {
		if resume && scheduler.LooksLikeResumeError(err) {
			return fmt.Errorf("%w\n提示：用 --new 开始一次新扫描，或查看 'db stats' 确认会话状态", err)
		}
		return err
	}
	printer.done()

	// 记住这次会话，下次 --resume 无需手工指定 ID。
	//
	// 写失败不影响扫描结果，但要提示：否则用户下次 --resume
	// 会莫名其妙地找不到会话。
	if err := identity.SetLastSession(p.identityPath, result.SessionID); err != nil {
		fmt.Fprintln(env.Stderr, "warning: could not record last session: "+err.Error())
	}

	printScanSummary(env.Stdout, result, p)
	return nil
}

// resolveScanSession 决定本次扫描使用的会话 ID。
func resolveScanSession(ctx context.Context, store *storage.Store, collectorPK int64, p scanParams, local *identity.Local) (sessionID string, resume bool, err error) {
	switch {
	case p.resume:
		id := strings.TrimSpace(p.sessionID)
		if id == "" {
			// 优先用本地记住的会话；没有就找数据库里最近一个未结束的。
			id = strings.TrimSpace(local.LastSessionID)
			if id == "" {
				latest, lerr := store.LatestOpenSession(ctx, collectorPK)
				if lerr != nil {
					return "", false, fmt.Errorf(
						"no session to resume (use --session <id>, or run a scan first): %w", lerr)
				}
				id = latest.ID
			}
		}
		return id, true, nil

	case p.sessionID != "":
		// 指定了 ID 但没加 --resume：理解为"续测这个会话"，
		// 因为若想新建，ID 由程序生成即可，没必要手写。
		return p.sessionID, true, nil

	default:
		id, gerr := model.NewSessionID(time.Now().UTC())
		if gerr != nil {
			return "", false, gerr
		}
		return id, false, nil
	}
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printScanHeader 在扫描开始前说明将要做什么。
func printScanHeader(w io.Writer, res *source.Result, targets []model.Target,
	sched *scheduler.Scheduler, p scanParams, sessionID string, resume bool, local *identity.Local) {

	origin := "network"
	switch {
	case res.FromCache && res.Stale:
		origin = "cache (stale)"
	case res.FromCache:
		origin = "cache"
	}

	mode := "new session"
	if resume {
		mode = "resume"
	}

	fmt.Fprintf(w, "source:     %s (%s, %s)\n", res.Meta.URL, origin, res.Meta.Format)
	fmt.Fprintf(w, "targets:    %d\n", len(targets))
	fmt.Fprintf(w, "database:   %s\n", p.db)
	fmt.Fprintf(w, "collector:  %s  %s\n", shortCollectorID(p.identityPath), local.Profile.ToModel().GroupKey())
	fmt.Fprintf(w, "session:    %s (%s)\n", sessionID, mode)
	fmt.Fprintf(w, "concurrency: %d workers, timeout %s\n", p.workers, probeTimeout(p))
	if p.trace {
		fmt.Fprintf(w, "trace:      requested (Phase 7 之前无法执行，会被明确跳过)\n")
	}
	if res.Stale {
		fmt.Fprintf(w, "warning:    using STALE target list\n")
	}
	fmt.Fprintln(w)
}

// probeTimeout 返回生效的单目标超时，用于日志展示。
func probeTimeout(p scanParams) time.Duration {
	if p.timeout.set {
		return p.timeout.d
	}
	return probe.DefaultTimeout
}

// scanProgressPrinter 按阶段输出进度，避免刷屏（需求第 54 条）。
type scanProgressPrinter struct {
	w        io.Writer
	quiet    bool
	lastLine string
}

func newScanProgressPrinter(w io.Writer, quiet bool) *scanProgressPrinter {
	return &scanProgressPrinter{w: w, quiet: quiet}
}

// onEvent 处理一次进度回调。
func (p *scanProgressPrinter) onEvent(ev scheduler.ProgressEvent) {
	if p == nil || p.quiet {
		return
	}

	label := "probe"
	if ev.Phase == scheduler.PhaseTrace {
		label = "trace"
	}

	// 只保留一行：用 \r 回到行首覆盖，而不是每次打印新行。
	// 这样长时间扫描不会把终端刷满，用户也能看到实时进度。
	line := fmt.Sprintf("\r%s completed=%d/%d success=%d failed=%d",
		label, ev.Completed, ev.Total, ev.Success, ev.Failed)
	if line == p.lastLine {
		return
	}
	p.lastLine = line
	fmt.Fprint(p.w, line)
}

// done 结束进度行。
func (p *scanProgressPrinter) done() {
	if p == nil || p.quiet || p.lastLine == "" {
		return
	}
	fmt.Fprintln(p.w)
	p.lastLine = ""
}

// printScanSummary 输出扫描汇总。
func printScanSummary(w io.Writer, r *scheduler.Result, p scanParams) {
	if r.Resumed {
		fmt.Fprintf(w, "mode:        resume (skipped %d already-measured target(s))\n", r.AlreadyDone)
	} else {
		fmt.Fprintf(w, "mode:        new session\n")
	}
	fmt.Fprintf(w, "session:     %s\n", r.SessionID)
	fmt.Fprintf(w, "targets:     %d total, %d to measure\n", r.TargetsTotal, r.TargetsPending)

	if r.TargetsPending > 0 {
		rate := r.Probe.SuccessRate() * 100
		fmt.Fprintf(w, "\nCompleted: %d / %d\n", r.Probe.Completed, r.TargetsPending)
		fmt.Fprintf(w, "Success:   %d\n", r.Probe.Success)
		fmt.Fprintf(w, "Failed:    %d\n", r.Probe.Failed)
		if elapsed := r.Elapsed(); elapsed > 0 {
			fmt.Fprintf(w, "Rate:      %.1f/s\n", float64(r.Probe.Completed)/elapsed.Seconds())
		}
		if r.Probe.Completed > 0 {
			fmt.Fprintf(w, "Success %%: %.1f%%\n", rate)
		}
	}

	if len(r.Probe.ErrorCounts) > 0 {
		fmt.Fprintf(w, "\nfailures by type:\n")
		for _, line := range formatErrorCounts(r.Probe.ErrorCounts, p.verbose) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	fmt.Fprintf(w, "\nstored:      %d measurement(s)\n", r.Stored)
	if r.Duplicates > 0 {
		fmt.Fprintf(w, "duplicates:  %d row(s) already present, skipped\n", r.Duplicates)
	}
	if r.StoreFailures > 0 {
		fmt.Fprintf(w, "warning:     %d storage failure(s); run '%s db stats' to verify\n",
			r.StoreFailures, ClientName)
	}

	fmt.Fprintf(w, "session progress: %d measured (%d ok, %d failed)\n",
		r.Progress.Measured, r.Progress.Success, r.Progress.Failed)

	if r.TraceSkipped {
		fmt.Fprintf(w, "\ntrace:       SKIPPED (--trace 需要 NextTrace，Phase 7 起可用)\n")
	} else if r.TraceAttempted > 0 {
		fmt.Fprintf(w, "trace:       %d target(s) attempted\n", r.TraceAttempted)
	}

	if r.Interrupted {
		fmt.Fprintf(w, "\nwarning: scan was interrupted; session is left open for --resume\n")
	} else if r.SessionFinished {
		fmt.Fprintf(w, "session:     finished\n")
	} else {
		fmt.Fprintf(w, "session:     left open (resume with --resume)\n")
	}
}

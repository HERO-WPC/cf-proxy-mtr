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
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// durationFlag 是一个"能区分是否被显式设置"的时间间隔参数。
//
// 为什么需要它：flag.Duration 的零值（0）与"用户写了 0s"无法区分，
// 而 0 在本项目里表示"使用默认值"。因此在 Set 里记录是否被设置过。
type durationFlag struct {
	d   time.Duration
	set bool
}

func (f *durationFlag) String() string {
	if f == nil || !f.set {
		return ""
	}
	return f.d.String()
}

func (f *durationFlag) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	f.d = d
	f.set = true
	return nil
}

// probeParams 是 `cf-route-tester probe` 的参数。
type probeParams struct {
	source sourceParams

	workers int
	timeout durationFlag
	limit   int

	// db 非空时把测量结果写入本地 SQLite。
	//
	// 空字符串表示只测量不入库（用于快速抽样观察网络状况）。
	db string

	// identityPath 是本地匿名标识文件路径（collector_id 的来源）。
	identityPath string

	// 采集者画像覆盖项：留空表示沿用本地标识文件中的值。
	//
	// 手动填写优先于自动检测，因为公网 ASN 不一定等于
	// 用户实际感知的接入线路。
	collectorCountry   string
	collectorProvince  string
	collectorCity      string
	collectorISP       string
	collectorASN       string
	collectorIPVersion string

	jsonOut bool
	verbose bool
	quiet   bool
}

// newProbeCommand 构造 probe 子命令。
func newProbeCommand() Command {
	return Command{
		Name:    "probe",
		Summary: "对全部 IP:Port 执行 TCP 连通性与延迟测量",
		Usage:   ClientName + " probe [flags]",
		Run:     runProbe,
		Flags: func(w io.Writer) {
			var p probeParams
			fs := probeFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			// 把错误分类的含义一并打印出来：
			// 否则用户看到 "connection_refused" 只能靠猜，
			// 而分类的含义正是这份数据的价值所在。
			fmt.Fprintf(w, "\nFailure classification:\n%s\n", describeErrorTypes())
		},
	}
}

// probeFlagSet 构造 probe 的 FlagSet，供解析与帮助共用。
func probeFlagSet(p *probeParams) *flag.FlagSet {
	fs := newFlagSet("probe")

	registerSourceFlags(fs, &p.source)

	fs.IntVar(&p.workers, "workers", probe.DefaultWorkers, "TCP 并发数（上限 "+itoa(probe.MaxWorkers)+"）")
	fs.Var(&p.timeout, "timeout", "单个 TCP 连接超时（默认 "+probe.DefaultTimeout.String()+"）")
	fs.IntVar(&p.limit, "limit", 0, "只测前 N 个目标（0 表示全部，用于快速抽样）")

	fs.BoolVar(&p.jsonOut, "json", false, "把每条结果作为一行 JSON 写到 stdout（便于管道处理）")
	fs.BoolVar(&p.verbose, "verbose", false, "显示每个目标的探测结果")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出汇总统计，不输出进度")

	// 落库相关参数。
	fs.StringVar(&p.db, "db", "", "把结果写入 SQLite（路径，留空表示不落库）")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径（collector_id）")

	// 采集者画像：留空表示沿用本地文件中的值。
	fs.StringVar(&p.collectorCountry, "country", "", "采集者国家代码（覆盖本地标识）")
	fs.StringVar(&p.collectorProvince, "province", "", "采集者省份（覆盖本地标识）")
	fs.StringVar(&p.collectorCity, "city", "", "采集者城市（覆盖本地标识）")
	fs.StringVar(&p.collectorISP, "isp", "", "采集者运营商（覆盖本地标识）")
	fs.StringVar(&p.collectorASN, "asn", "", "采集者 ASN（覆盖本地标识）")
	fs.StringVar(&p.collectorIPVersion, "ip-version", "", "采集者 IP 版本（覆盖本地标识）")

	return fs
}

// collectorOverrides 把命令行给出的画像整理成覆盖表。
//
// 只包含**非空**项：空值表示"沿用本地文件里的值"，
// 而不是"把它清空"。
func (p probeParams) collectorOverrides() map[string]string {
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

// runProbe 实现 `cf-route-tester probe`。
//
// 注意：本命令只做 TCP 探测，不写数据库、不做线路跟踪。
// 落库在 Phase 4/5（scan），线路跟踪在 Phase 7。
func runProbe(env *Env, args []string) error {
	var p probeParams
	fs := probeFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if p.limit < 0 {
		return usageError("--limit must not be negative")
	}

	// Ctrl+C / SIGTERM 时取消探测。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, err := loadTargets(ctx, p.source)
	if err != nil {
		return err
	}

	targets := res.Targets
	if p.limit > 0 && p.limit < len(targets) {
		targets = targets[:p.limit]
	}
	if len(targets) == 0 {
		return fmt.Errorf("no targets to probe (source %s returned an empty list)", res.Meta.URL)
	}

	cfg := probe.DefaultConfig()
	cfg.Workers = p.workers
	if p.timeout.set {
		cfg.Timeout = p.timeout.d
	}
	// 并发上限由 probe.New 统一截断，这里不重复实现。
	runner := probe.NewRunner(probe.RunnerConfig{Probe: cfg})

	// 落库准备：目标与采集者必须先入库，测量才能引用它们
	// （数据库用外键保证"不存在孤儿测量"）。
	var (
		store        *storage.Store
		collectorPK  int64
		targetsSaved int
	)
	if strings.TrimSpace(p.db) != "" {
		store, err = openProbeStore(ctx, p)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := store.Close(); cerr != nil {
				fmt.Fprintln(env.Stderr, "Error: closing database: "+cerr.Error())
			}
		}()

		local, err := resolveIdentity(p.identityPath, p.collectorOverrides())
		if err != nil {
			return err
		}

		// 目标元数据是维度表，可以重复写入（UPSERT）。
		targetsSaved, err = store.UpsertTargets(ctx, targets, time.Now().UTC())
		if err != nil {
			return err
		}

		collectorPK, err = store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), version.Version)
		if err != nil {
			return err
		}
	}

	if !p.quiet {
		printProbeHeader(env.Stderr, res, targets, runner.Prober().Config())
		if store != nil {
			fmt.Fprintf(env.Stderr, "database:  %s (collector %s, %d new target(s))\n\n",
				store.Path(), shortCollectorID(p.identityPath), targetsSaved)
		}
	}

	started := time.Now()
	handle := runner.Start(ctx, targets)

	var (
		latencies  []float64
		errorCount = make(map[probe.ErrorType]int)
		success    int
		failed     int
		completed  int

		// 落库批次：攒够 batchSize 条就写一次。
		//
		// 为什么不每条一个事务：SQLite 每次提交都要 fsync，
		// 逐条写入会把扫描速度拖到几百条/秒。批量事务能把
		// 同样的写入量提升一个数量级，而"整批一个事务"
		// 也保证了不会留下半批数据。
		batch         []storage.Measurement
		measureSaved  int
		measureDupes  int
		measureErrors int

		jsonWriter *bufio.Writer
	)
	const measurementBatchSize = 500

	if p.jsonOut {
		jsonWriter = bufio.NewWriter(env.Stdout)
	}
	progress := newProgressPrinter(env.Stderr, len(targets), p.quiet)

	// flushMeasurements 写入当前批次并清空缓冲。
	flushMeasurements := func() {
		if len(batch) == 0 {
			return
		}
		saved, skipped, err := store.SaveMeasurements(ctx, batch)
		measureSaved += saved
		measureDupes += skipped
		if err != nil {
			// 写入失败不能让整个扫描崩掉：已经测到的结果仍在，
			// 只是这一批没能落库。记录并继续，最后汇总里如实报告。
			measureErrors++
			fmt.Fprintln(env.Stderr, "Error: saving measurements: "+err.Error())
		}
		batch = batch[:0]
	}

	for result := range handle.Results() {
		completed++
		if result.Success {
			success++
			latencies = append(latencies, result.LatencyMS)
		} else {
			failed++
			errorCount[result.ErrorType]++
		}

		if store != nil {
			batch = append(batch, storage.NewMeasurement(collectorPK, "", result))
			if len(batch) >= measurementBatchSize {
				flushMeasurements()
			}
		}

		if jsonWriter != nil {
			if err := writeProbeJSONLine(jsonWriter, result); err != nil {
				// 输出失败（例如磁盘满、管道关闭）是全局错误，
				// 但已经测到的结果仍然要被汇总出来，
				// 因此这里记录后停止写 JSON，继续排空结果。
				fmt.Fprintln(env.Stderr, "Error: writing json output: "+err.Error())
				jsonWriter = nil
			}
		}

		if p.verbose {
			fmt.Fprintln(env.Stderr, formatProbeResult(result))
		}
		progress.update(completed, success, failed)
	}

	if jsonWriter != nil {
		if err := jsonWriter.Flush(); err != nil {
			fmt.Fprintln(env.Stderr, "Error: flushing json output: "+err.Error())
		}
	}
	progress.done()

	// 排空最后一批测量。必须用**未被取消的** context：
	// Ctrl+C 已经取消了探测 ctx，若沿用它写库会直接失败，
	// 而那正是最需要把已测结果保存下来的时刻。
	if store != nil {
		saved, skipped, err := store.SaveMeasurements(context.Background(), batch)
		measureSaved += saved
		measureDupes += skipped
		if err != nil {
			measureErrors++
			fmt.Fprintln(env.Stderr, "Error: saving final measurements: "+err.Error())
		}
		batch = nil
	}

	// Wait 必须在排空结果之后调用：它等待 pool goroutine 完全退出。
	// pool 侧统计用于汇报 Dropped（被丢弃的结果数）；
	// 其余汇总数字用消费者侧统计，因为它们如实反映
	// "调用方真正拿到多少条结果"。
	poolStats := handle.Wait()
	elapsed := time.Since(started)

	summary := probeSummary{
		Targets:       len(targets),
		Completed:     completed,
		Success:       success,
		Failed:        failed,
		ErrorCount:    errorCount,
		Elapsed:       elapsed,
		Interrupted:   completed < len(targets),
		Dropped:       poolStats.Dropped,
		Latency:       summarizeLatencies(latencies),
		Stored:        measureSaved,
		Duplicates:    measureDupes,
		StoreFailures: measureErrors,
		DBPath:        p.db,
	}

	if !p.jsonOut || !p.quiet {
		printProbeSummary(env.Stdout, summary, res, p)
	}
	return nil
}

// probeSummary 是本次探测的汇总。
type probeSummary struct {
	Targets     int
	Completed   int
	Success     int
	Failed      int
	Dropped     int
	ErrorCount  map[probe.ErrorType]int
	Elapsed     time.Duration
	Interrupted bool
	Latency     latencySummary

	// 落库统计（仅在 --db 非空时有意义）。
	Stored        int
	Duplicates    int
	StoreFailures int
	DBPath        string
}

// Rate 返回成功率（按已完成数计算）。
func (s probeSummary) Rate() float64 {
	if s.Completed == 0 {
		return 0
	}
	return float64(s.Success) / float64(s.Completed)
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printProbeHeader 在探测开始前打印将要做什么。
//
// 输出到 stderr：stdout 保留给结果与汇总，便于脚本管道使用。
func printProbeHeader(w io.Writer, res *source.Result, targets []model.Target, cfg probe.Config) {
	origin := "network"
	switch {
	case res.FromCache && res.Stale:
		origin = "cache (stale)"
	case res.FromCache:
		origin = "cache"
	}

	fmt.Fprintf(w, "source:    %s (%s, %s)\n", res.Meta.URL, origin, res.Meta.Format)
	if len(targets) != len(res.Targets) {
		fmt.Fprintf(w, "targets:   %d of %d\n", len(targets), len(res.Targets))
	} else {
		fmt.Fprintf(w, "targets:   %d\n", len(targets))
	}
	fmt.Fprintf(w, "concurrency: %d workers, timeout %s\n", cfg.Workers, cfg.Timeout)
	if res.Stale {
		fmt.Fprintf(w, "warning:   using STALE target list\n")
	}
	fmt.Fprintln(w)
}

// formatProbeResult 把一条结果格式化成一行日志。
func formatProbeResult(r probe.ProbeResult) string {
	if r.Success {
		return fmt.Sprintf("  %-24s ok   %.1f ms", r.TargetID, r.LatencyMS)
	}
	return fmt.Sprintf("  %-24s fail %s (%s)", r.TargetID, r.ErrorType, r.ErrorMessage)
}

// printProbeSummary 输出汇总统计。
func printProbeSummary(w io.Writer, s probeSummary, res *source.Result, p probeParams) {
	fmt.Fprintf(w, "probed:      %d / %d targets\n", s.Completed, s.Targets)
	fmt.Fprintf(w, "success:     %d (%.1f%%)\n", s.Success, s.Rate()*100)
	fmt.Fprintf(w, "failed:      %d (%.1f%%)\n", s.Failed, (1-s.Rate())*100)
	fmt.Fprintf(w, "elapsed:     %s", s.Elapsed.Round(time.Millisecond))
	if s.Completed > 0 {
		rate := float64(s.Completed) / s.Elapsed.Seconds()
		fmt.Fprintf(w, " (%.1f targets/s)", rate)
	}
	fmt.Fprintln(w)

	if s.Latency.Count > 0 {
		fmt.Fprintf(w, "\nlatency (ms, %d samples):\n", s.Latency.Count)
		fmt.Fprintf(w, "  min %.1f   p50 %.1f   p90 %.1f   p95 %.1f   max %.1f   avg %.1f\n",
			s.Latency.Min, s.Latency.P50, s.Latency.P90, s.Latency.P95, s.Latency.Max, s.Latency.Avg)
	}

	if len(s.ErrorCount) > 0 {
		fmt.Fprintf(w, "\nfailures by type:\n")
		for _, line := range formatErrorCounts(s.ErrorCount, p.verbose) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	if s.Interrupted {
		fmt.Fprintf(w, "\nwarning: probing was interrupted; results are incomplete\n")
	}
	if s.Dropped > 0 {
		// 丢结果意味着汇总里的样本数小于实际测到的数量，
		// 必须明确告知，避免用户把"少了的样本"当成真实情况。
		fmt.Fprintf(w, "warning: %d result(s) were dropped because the consumer could not keep up\n", s.Dropped)
	}
	if s.DBPath != "" {
		fmt.Fprintf(w, "\nstored:      %d measurement(s) in %s\n", s.Stored, s.DBPath)
		if s.Duplicates > 0 {
			// 重复说明这批结果之前已经写过（例如断点续测重跑），
			// 幂等跳过是正确行为，但必须让用户知道。
			fmt.Fprintf(w, "duplicates:  %d row(s) already present, skipped\n", s.Duplicates)
		}
		if s.StoreFailures > 0 {
			fmt.Fprintf(w, "warning: %d batch(es) failed to store; run 'db stats' to verify\n", s.StoreFailures)
		}
	}
	if res.Stale {
		fmt.Fprintf(w, "warning: target list came from a STALE cache\n")
	}
}

// formatErrorCounts 把错误分类计数格式化成"按数量降序"的列表。
func formatErrorCounts(counts map[probe.ErrorType]int, verbose bool) []string {
	type kv struct {
		kind probe.ErrorType
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
		out = append(out, fmt.Sprintf("%-22s %d", it.kind+":", it.n))
	}
	if rest > 0 {
		out = append(out, fmt.Sprintf("%-22s %d", "... others:", rest))
	}
	return out
}

// ---------------------------------------------------------------------------
// 延迟分位数
// ---------------------------------------------------------------------------

// latencySummary 是一组延迟样本的分位数摘要。
type latencySummary struct {
	Count int
	Min   float64
	P50   float64
	P90   float64
	P95   float64
	Max   float64
	Avg   float64
}

// summarizeLatencies 计算延迟摘要。
//
// 采用"线性插值"（与 numpy / pandas 默认的 R-7 方法一致）而不是
// 简单取第 k 个元素：样本量小时后者会明显偏乐观，
// 而项目最终要比较不同地区/运营商的分位数，算法必须一致且可解释。
//
// 输入切片会被排序（原地），调用方不应假定它保持原顺序。
func summarizeLatencies(latencies []float64) latencySummary {
	if len(latencies) == 0 {
		return latencySummary{}
	}

	sort.Float64s(latencies)

	sum := 0.0
	for _, v := range latencies {
		sum += v
	}

	return latencySummary{
		Count: len(latencies),
		Min:   latencies[0],
		P50:   percentile(latencies, 0.50),
		P90:   percentile(latencies, 0.90),
		P95:   percentile(latencies, 0.95),
		Max:   latencies[len(latencies)-1],
		Avg:   sum / float64(len(latencies)),
	}
}

// percentile 用线性插值计算分位数。
//
// sorted 必须已升序排列；q 在 [0,1] 区间。
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}

	// R-7：位置 = (n-1) * q
	pos := q * float64(len(sorted)-1)
	lower := int(pos)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}

	frac := pos - float64(lower)
	return sorted[lower] + frac*(sorted[upper]-sorted[lower])
}

// ---------------------------------------------------------------------------
// 进度
// ---------------------------------------------------------------------------

// progressPrinter 以固定间隔刷新一行进度，避免刷屏。
//
// 需求第 54 条：默认不要刷屏，只显示阶段性进度。
type progressPrinter struct {
	w        io.Writer
	total    int
	disabled bool
	last     time.Time
	interval time.Duration
}

// newProgressPrinter 创建进度打印器。quiet 为真时完全不输出。
func newProgressPrinter(w io.Writer, total int, quiet bool) *progressPrinter {
	return &progressPrinter{
		w:        w,
		total:    total,
		disabled: quiet,
		interval: 2 * time.Second,
		last:     time.Now(),
	}
}

// update 在达到刷新间隔时输出一次进度。
func (p *progressPrinter) update(completed, success, failed int) {
	if p == nil || p.disabled {
		return
	}
	if time.Since(p.last) < p.interval && completed < p.total {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(p.w, "progress completed=%d/%d success=%d failed=%d\n",
		completed, p.total, success, failed)
}

// done 输出结束标记（清掉"进行中"的观感）。
func (p *progressPrinter) done() {
	if p == nil || p.disabled {
		return
	}
	fmt.Fprintln(p.w)
}

// ---------------------------------------------------------------------------
// JSONL 输出
// ---------------------------------------------------------------------------

// probeJSONRecord 是单条测量结果在 JSONL 中的形状。
//
// 字段一旦发布就是公开契约，因此显式列出并带 schema_version。
// 注意这里**没有**任何采集者信息：collector 相关字段属于导出/上传层
// （Phase 9/10），本命令只输出客观测量事实。
type probeJSONRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ClientVersion string `json:"client_version"`

	TargetID string `json:"target_id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	Success   bool    `json:"success"`
	LatencyMS float64 `json:"latency_ms"`

	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	Timestamp string `json:"timestamp"`
}

// writeProbeJSONLine 把一条结果写成一行 JSON。
func writeProbeJSONLine(w io.Writer, r probe.ProbeResult) error {
	record := probeJSONRecord{
		SchemaVersion: version.SchemaVersion,
		ClientVersion: version.Version,
		TargetID:      r.TargetID,
		IP:            r.IP,
		Port:          r.Port,
		Success:       r.Success,
		LatencyMS:     r.LatencyMS,
		ErrorType:     string(r.ErrorType),
		ErrorMessage:  r.ErrorMessage,
		Timestamp:     r.Timestamp.UTC().Format(time.RFC3339Nano),
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

// itoa 是本文件内的短整数格式化。
func itoa(n int) string { return strconv.Itoa(n) }

// openProbeStore 按 probe 的 --db 参数打开数据库。
func openProbeStore(ctx context.Context, p probeParams) (*storage.Store, error) {
	cfg := storage.DefaultConfig()
	cfg.Path = p.db
	return storage.Open(ctx, cfg)
}

// shortCollectorID 读取本地标识里的 collector_id 摘要，用于日志展示。
//
// 只显示前 10 位：完整 ID 会在日志里反复出现，缩短后既够用于
// 人工核对"这是同一个节点"，又不至于让它散落在各处。
func shortCollectorID(identityPath string) string {
	local, err := identity.Load(identityPath)
	if err != nil {
		return "(unknown)"
	}
	id := local.CollectorID
	if len(id) > 10 {
		return id[:10] + "..."
	}
	return id
}

// describeErrorTypes 返回错误分类的中文说明（用于 --help）。
//
// 保留在代码里是为了让"分类含义"与实现放在一起，
// 避免文档与代码脱节。
func describeErrorTypes() string {
	descriptions := []struct {
		kind probe.ErrorType
		text string
	}{
		{probe.ErrorTypeTimeout, "超时（含整体取消前的等待超时）"},
		{probe.ErrorTypeConnectionRefused, "对端拒绝（IP 可达但该端口没有服务）"},
		{probe.ErrorTypeNetworkUnreachable, "网络/主机不可达"},
		{probe.ErrorTypeNoRoute, "本机没有可用路由"},
		{probe.ErrorTypeConnectionReset, "连接被重置"},
		{probe.ErrorTypePermissionDenied, "本机策略拒绝（防火墙/安全软件）"},
		{probe.ErrorTypeAddressNotAvailable, "本机缺少匹配地址族的地址"},
		{probe.ErrorTypeCanceled, "本次运行被中断（不计入丢包）"},
		{probe.ErrorTypeInvalidTarget, "目标数据非法（不计入丢包）"},
		{probe.ErrorTypeOther, "无法归类的连接错误"},
	}

	var b strings.Builder
	for _, d := range descriptions {
		b.WriteString(fmt.Sprintf("  %-22s %s\n", d.kind+":", d.text))
	}
	return strings.TrimRight(b.String(), "\n")
}

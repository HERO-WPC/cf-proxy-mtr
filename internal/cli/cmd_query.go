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

	"github.com/cf-route-tester/cf-route-tester/internal/aggregate"
	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/query"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// queryParams 是 `cf-route-tester query` 的参数。
type queryParams struct {
	// targets 是要查询的 IP:Port（可重复）。
	targets stringList

	db           string
	identityPath string

	// inputs 是 JSONL 数据源（与 --db 二选一）。
	inputs stringList

	session string
	since   string
	until   string

	limit int

	// topRegions 限制输出的分组数（0 表示全部）。
	topRegions int

	// showHops 是否输出逐跳表。
	showHops bool

	// showSeries 是否输出时间序列。
	showSeries bool

	// listTargets 列出数据里的目标。
	listTargets bool

	// statsOnly 只输出全局统计，不查具体目标。
	statsOnly bool

	format string
	out    string

	quiet bool
}

// 查询输出格式。
const (
	queryFormatText = "text"
	queryFormatJSON = "json"
)

// newQueryCommand 构造 query 子命令。
func newQueryCommand() Command {
	return Command{
		Name:    "query",
		Summary: "查询某个 IP:Port 的线路画像（跨地区 / 跨运营商）",
		Usage:   ClientName + " query IP:PORT [flags]",
		Run:     runQuery,
		Flags: func(w io.Writer) {
			var p queryParams
			fs := queryFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nData source (exactly one; --db and --input are mutually exclusive):\n")
			fmt.Fprintf(w, "  --db <path>        本地数据库（你自己的历史数据）\n")
			fmt.Fprintf(w, "  --input <path>     公开 JSONL 文件或目录（可重复）\n")

			fmt.Fprintf(w, "\nWhat it shows:\n")
			fmt.Fprintf(w, "  按 (地区 × 运营商) 分组的成功率、延迟分位数、失败分类\n")
			fmt.Fprintf(w, "  各分组的 AS 路径（不同运营商是否走不同出口）\n")
			fmt.Fprintf(w, "  逐跳延迟与超时情况（--hops）\n")
			fmt.Fprintf(w, "  延迟随时间的变化（--series）\n")

			fmt.Fprintf(w, "\nNote:\n")
			fmt.Fprintf(w, "  分位数由固定直方图估算（近似值，相对误差约 3.5%%），\n")
			fmt.Fprintf(w, "  极值与平均值是精确值。JSON 输出里带 percentiles_approx 标记。\n")

			fmt.Fprintf(w, "\nExamples:\n")
			fmt.Fprintf(w, "  %s query 1.1.1.1:443 --db data/results.db\n", ClientName)
			fmt.Fprintf(w, "  %s query 1.1.1.1:443 --input data/batches --hops\n", ClientName)
			fmt.Fprintf(w, "  %s query --list-targets --input data/batches\n", ClientName)
			fmt.Fprintf(w, "  %s query --stats --db data/results.db\n", ClientName)
		},
	}
}

// queryFlagSet 构造 query 的 FlagSet。
func queryFlagSet(p *queryParams) *flag.FlagSet {
	fs := newFlagSet("query")

	fs.Var(&p.targets, "target", "要查询的 IP:Port（也可作为位置参数给出）")
	fs.StringVar(&p.db, "db", "", "本地数据库路径")
	fs.Var(&p.inputs, "input", "公开 JSONL 文件或目录（可重复）")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径（--db 时用于定位采集者）")

	fs.StringVar(&p.session, "session", "", "只查指定会话")
	fs.StringVar(&p.since, "since", "", "起始时间（RFC3339 或 2006-01-02）")
	fs.StringVar(&p.until, "until", "", "截止时间（RFC3339 或 2006-01-02）")
	fs.IntVar(&p.limit, "limit", 0, "最多载入的行数（0 表示不限，用于大库抽样）")

	fs.IntVar(&p.topRegions, "top-regions", 0, "最多输出多少个地区分组（0 表示全部）")
	fs.BoolVar(&p.showHops, "hops", false, "输出逐跳延迟表")
	fs.BoolVar(&p.showSeries, "series", false, "输出延迟时间序列")
	fs.BoolVar(&p.listTargets, "list-targets", false, "列出数据里的目标，不查具体画像")
	fs.BoolVar(&p.statsOnly, "stats", false, "只输出全局统计")

	fs.StringVar(&p.format, "format", queryFormatText, "输出格式：text / json")
	fs.StringVar(&p.out, "out", "", "输出文件路径（默认写到 stdout）")
	fs.BoolVar(&p.quiet, "quiet", false, "不输出数据源信息")

	return fs
}

// runQuery 实现 `cf-route-tester query`。
func runQuery(env *Env, args []string) error {
	var p queryParams
	fs := queryFlagSet(&p)

	// 允许 `query 1.1.1.1:443 --db x.db` 与 `query --db x.db 1.1.1.1:443`
	// 两种写法：位置参数与选项可以任意交错。
	positionals, err := parseFlagsAllowInterspersed(fs, args)
	if err != nil {
		return err
	}
	p.targets = append(p.targets, positionals...)

	format := strings.ToLower(strings.TrimSpace(p.format))
	switch format {
	case queryFormatText, queryFormatJSON:
	default:
		return usageError("unknown --format %q (want %s or %s)", p.format, queryFormatText, queryFormatJSON)
	}

	// 数据源必须恰好一个：两个都给会让"这个数字从哪来"变得说不清。
	if strings.TrimSpace(p.db) != "" && len(p.inputs) > 0 {
		return usageError("--db and --input are mutually exclusive; pick one data source")
	}
	if strings.TrimSpace(p.db) == "" && len(p.inputs) == 0 {
		return usageError("a data source is required: pass --db <path> or --input <file-or-dir>")
	}

	// 没有目标时，只允许"列目标"或"看统计"。
	if len(p.targets) == 0 && !p.listTargets && !p.statsOnly {
		return usageError("no target given: pass IP:PORT, or use --list-targets / --stats")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 1) 载入数据。
	dataset, sourceLabel, err := loadQueryDataset(ctx, p, env)
	if err != nil {
		return err
	}
	if dataset.TargetCount() == 0 {
		fmt.Fprintf(env.Stderr, "warning: no rows matched in %s\n", sourceLabel)
	}

	// 2) 输出。
	out, closeFn, err := openQueryOutput(env, p.out)
	if err != nil {
		return err
	}
	if closeFn != nil {
		defer func() { _ = closeFn() }()
	}

	buffered := bufio.NewWriter(out)

	if !p.quiet && format == queryFormatText {
		printQuerySource(buffered, sourceLabel, dataset)
	}

	switch {
	case p.listTargets:
		err = writeTargetList(buffered, dataset, format)
	case p.statsOnly:
		err = writeDatasetStats(buffered, dataset, format)
	default:
		err = writeTargetProfiles(buffered, dataset, p, format, env)
	}
	if err != nil {
		return err
	}
	return buffered.Flush()
}

// loadQueryDataset 从选定数据源载入数据集。
func loadQueryDataset(ctx context.Context, p queryParams, env *Env) (*query.Dataset, string, error) {
	opts := query.IndexOptions{}

	if len(p.inputs) > 0 {
		dataset, stats, err := query.LoadFromJSONL(p.inputs, opts)
		if err != nil {
			return nil, "", err
		}
		label := fmt.Sprintf("%d JSONL file(s)", stats.Sources)
		if stats.BadLines > 0 {
			fmt.Fprintf(env.Stderr, "warning: %d unparsable line(s) skipped\n", stats.BadLines)
		}
		return dataset, label, nil
	}

	store, err := storage.Open(ctx, storage.Config{Path: p.db})
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintln(env.Stderr, "Error: closing database: "+cerr.Error())
		}
	}()

	local, err := identity.Load(p.identityPath)
	if err != nil {
		return nil, "", err
	}
	collectorPK, err := store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), version.Version)
	if err != nil {
		return nil, "", err
	}

	since, err := parseExportTime(p.since)
	if err != nil {
		return nil, "", usageError("--since: %v", err)
	}
	until, err := parseExportTimeEnd(p.until)
	if err != nil {
		return nil, "", usageError("--until: %v", err)
	}

	filter := storage.ExportFilter{
		CollectorPK: collectorPK,
		SessionID:   strings.TrimSpace(p.session),
		Since:       since,
		Until:       until,
		Limit:       p.limit,
	}

	dataset, err := query.LoadFromStore(ctx, store, filter, opts)
	if err != nil {
		return nil, "", err
	}
	return dataset, p.db, nil
}

// openQueryOutput 准备输出目标。
func openQueryOutput(env *Env, out string) (io.Writer, func() error, error) {
	path := strings.TrimSpace(out)
	if path == "" || path == "-" {
		return env.Stdout, nil, nil
	}

	// 与 export / aggregate 一致：不静默覆盖。
	if _, err := os.Stat(path); err == nil {
		return nil, nil, fmt.Errorf("output file %q already exists; remove it or choose another path with --out", path)
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("create output file %q: %w", path, err)
	}
	return file, file.Close, nil
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// printQuerySource 输出数据源概况。
func printQuerySource(w io.Writer, label string, d *query.Dataset) {
	fmt.Fprintf(w, "source:      %s\n", label)
	fmt.Fprintf(w, "rows:        %d\n", d.TotalRows)
	fmt.Fprintf(w, "targets:     %d\n", d.TargetCount())
	fmt.Fprintf(w, "collectors:  %d\n", len(d.Collectors))
	if !d.FirstTimestamp.IsZero() {
		fmt.Fprintf(w, "time range:  %s .. %s\n",
			d.FirstTimestamp.Format(time.RFC3339), d.LastTimestamp.Format(time.RFC3339))
	}
	fmt.Fprintln(w)
}

// writeTargetList 输出目标清单。
func writeTargetList(w io.Writer, d *query.Dataset, format string) error {
	targets := d.Targets()

	if format == queryFormatJSON {
		type entry struct {
			TargetID     string  `json:"target_id"`
			IP           string  `json:"ip"`
			Port         int     `json:"port"`
			Regions      int     `json:"regions"`
			ProbeTotal   int64   `json:"probe_total"`
			ProbeSuccess int64   `json:"probe_success"`
			SuccessRate  float64 `json:"success_rate"`
			TraceTotal   int64   `json:"trace_total"`
			P50MS        float64 `json:"p50_ms"`
		}
		out := make([]entry, 0, len(targets))
		for _, profile := range targets {
			item := entry{
				TargetID: profile.TargetID, IP: profile.IP, Port: profile.Port,
				Regions:    profile.RegionCount(),
				ProbeTotal: profile.ProbeTotal, ProbeSuccess: profile.ProbeSuccess,
				SuccessRate: profile.SuccessRate(),
				TraceTotal:  profile.TraceTotal,
			}
			if median, ok := profile.Latency.Quantile(0.50); ok {
				item.P50MS = median
			}
			out = append(out, item)
		}
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(out)
	}

	fmt.Fprintf(w, "%-28s %7s %8s %8s %8s %8s\n",
		"TARGET", "REGIONS", "PROBES", "OK", "OK%", "P50ms")
	for _, profile := range targets {
		p50 := 0.0
		if median, ok := profile.Latency.Quantile(0.50); ok {
			p50 = median
		}
		fmt.Fprintf(w, "%-28s %7d %8d %8d %7.1f%% %8.1f\n",
			truncateForDisplay(profile.TargetID, 28), profile.RegionCount(),
			profile.ProbeTotal, profile.ProbeSuccess,
			profile.SuccessRate()*100, p50)
	}
	fmt.Fprintf(w, "\n%d target(s)\n", len(targets))
	return nil
}

// datasetTotals 是全局统计。
type datasetTotals struct {
	Rows        int     `json:"rows"`
	Targets     int     `json:"targets"`
	Collectors  int     `json:"collectors"`
	ProbeTotal  int64   `json:"probe_total"`
	ProbeOK     int64   `json:"probe_success"`
	SuccessRate float64 `json:"success_rate"`
	TraceTotal  int64   `json:"trace_total"`
	TraceOK     int64   `json:"trace_success"`

	FirstTimestamp string `json:"first_timestamp,omitempty"`
	LastTimestamp  string `json:"last_timestamp,omitempty"`

	Latency aggregate.Histogram    `json:"latency"`
	Errors  []aggregate.ErrorCount `json:"errors,omitempty"`
}

// collectTotals 汇总全局统计。
func collectTotals(d *query.Dataset) datasetTotals {
	latency := aggregate.NewLatencyHistogram()
	errors := aggregate.ErrorCounts{}

	out := datasetTotals{
		Rows:       d.TotalRows,
		Targets:    d.TargetCount(),
		Collectors: len(d.Collectors),
	}
	if !d.FirstTimestamp.IsZero() {
		out.FirstTimestamp = d.FirstTimestamp.Format(time.RFC3339)
		out.LastTimestamp = d.LastTimestamp.Format(time.RFC3339)
	}

	for _, profile := range d.Targets() {
		out.ProbeTotal += profile.ProbeTotal
		out.ProbeOK += profile.ProbeSuccess
		out.TraceTotal += profile.TraceTotal
		out.TraceOK += profile.TraceSuccess
		latency.Merge(profile.Latency)
		for kind, count := range profile.Errors {
			errors[kind] += count
		}
	}
	if out.ProbeTotal > 0 {
		out.SuccessRate = float64(out.ProbeOK) / float64(out.ProbeTotal)
	}
	out.Latency = latency.Snapshot()
	out.Errors = errors.Sorted()
	return out
}

// writeDatasetStats 输出全局统计。
func writeDatasetStats(w io.Writer, d *query.Dataset, format string) error {
	totals := collectTotals(d)

	if format == queryFormatJSON {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(totals)
	}

	fmt.Fprintf(w, "rows:        %d\n", totals.Rows)
	fmt.Fprintf(w, "targets:     %d\n", totals.Targets)
	fmt.Fprintf(w, "collectors:  %d\n", totals.Collectors)
	if totals.FirstTimestamp != "" {
		fmt.Fprintf(w, "time range:  %s .. %s\n", totals.FirstTimestamp, totals.LastTimestamp)
	}

	fmt.Fprintf(w, "\nprobes:      %d total, %d ok (%.1f%%)\n",
		totals.ProbeTotal, totals.ProbeOK, totals.SuccessRate*100)
	if totals.TraceTotal > 0 {
		rate := 0.0
		if totals.TraceTotal > 0 {
			rate = float64(totals.TraceOK) / float64(totals.TraceTotal) * 100
		}
		fmt.Fprintf(w, "traces:      %d total, %d ok (%.1f%%)\n",
			totals.TraceTotal, totals.TraceOK, rate)
	}

	if totals.Latency.Count > 0 {
		fmt.Fprintf(w, "\nlatency (ms, %d samples, percentiles approximate):\n", totals.Latency.Count)
		fmt.Fprintf(w, "  min %.1f   p50 %.1f   p90 %.1f   p95 %.1f   p99 %.1f   max %.1f   avg %.1f\n",
			totals.Latency.MinMS, totals.Latency.P50MS, totals.Latency.P90MS,
			totals.Latency.P95MS, totals.Latency.P99MS, totals.Latency.MaxMS, totals.Latency.AvgMS)
	}

	if len(totals.Errors) > 0 {
		fmt.Fprintf(w, "\nfailures by type:\n")
		for _, item := range totals.Errors {
			fmt.Fprintf(w, "  %-24s %d\n", item.Type+":", item.Count)
		}
	}
	return nil
}

// profileJSON 是单个目标画像的 JSON 形状。
type profileJSON struct {
	TargetID string `json:"target_id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	Regions int `json:"regions"`

	ProbeTotal   int64   `json:"probe_total"`
	ProbeSuccess int64   `json:"probe_success"`
	SuccessRate  float64 `json:"success_rate"`

	TraceTotal   int64 `json:"trace_total"`
	TraceSuccess int64 `json:"trace_success"`

	LatencySpreadMS float64                `json:"latency_spread_ms"`
	Latency         aggregate.Histogram    `json:"latency"`
	Errors          []aggregate.ErrorCount `json:"errors,omitempty"`

	Sessions       int      `json:"sessions"`
	ClientVersions []string `json:"client_versions,omitempty"`

	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`

	RegionProfiles []regionJSON `json:"region_profiles"`
	Series         []seriesJSON `json:"series,omitempty"`
}

// regionJSON 是单个地区分组的 JSON 形状。
type regionJSON struct {
	Label    string `json:"label"`
	Country  string `json:"country,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	ISP      string `json:"isp,omitempty"`
	ASN      string `json:"asn,omitempty"`

	CollectorID string `json:"collector_id,omitempty"`

	ProbeTotal   int64   `json:"probe_total"`
	ProbeSuccess int64   `json:"probe_success"`
	SuccessRate  float64 `json:"success_rate"`

	Latency aggregate.Histogram    `json:"latency"`
	Errors  []aggregate.ErrorCount `json:"errors,omitempty"`

	TraceTotal   int64 `json:"trace_total"`
	TraceSuccess int64 `json:"trace_success"`

	ASPaths []query.ASPath `json:"as_paths,omitempty"`

	Hops      []hopJSON `json:"hops,omitempty"`
	HopCount  int       `json:"hop_count,omitempty"`
	Engine    string    `json:"engine,omitempty"`
	TraceMode string    `json:"trace_mode,omitempty"`

	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// hopJSON 是逐跳统计的 JSON 形状。
type hopJSON struct {
	TTL      int     `json:"ttl"`
	IP       string  `json:"ip,omitempty"`
	ASN      string  `json:"asn,omitempty"`
	ASOrg    string  `json:"as_organization,omitempty"`
	Country  string  `json:"country,omitempty"`
	City     string  `json:"city,omitempty"`
	Timeouts int     `json:"timeouts,omitempty"`
	Samples  int64   `json:"samples"`
	MinMS    float64 `json:"min_ms,omitempty"`
	P50MS    float64 `json:"p50_ms,omitempty"`
	MaxMS    float64 `json:"max_ms,omitempty"`
}

// seriesJSON 是时间序列的一个点。
type seriesJSON struct {
	At        string  `json:"at"`
	Success   bool    `json:"success"`
	LatencyMS float64 `json:"latency_ms"`
	ErrorType string  `json:"error_type,omitempty"`
}

// writeTargetProfiles 输出一个或多个目标的画像。
func writeTargetProfiles(w io.Writer, d *query.Dataset, p queryParams, format string, env *Env) error {
	// 收集画像；找不到的目标不静默跳过。
	profiles := make([]*query.TargetProfile, 0, len(p.targets))
	var missing []string

	for _, raw := range p.targets {
		profile, err := d.Lookup(raw)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (%v)", raw, err))
			continue
		}
		profiles = append(profiles, profile)
	}

	for _, text := range missing {
		fmt.Fprintf(env.Stderr, "warning: no data for %s\n", text)
	}

	if format == queryFormatJSON {
		out := make([]profileJSON, 0, len(profiles))
		for _, profile := range profiles {
			out = append(out, buildProfileJSON(profile, p))
		}
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(out); err != nil {
			return err
		}
	} else {
		for i, profile := range profiles {
			if i > 0 {
				fmt.Fprintln(w)
			}
			writeProfileText(w, profile, p)
		}
	}

	if len(profiles) == 0 {
		return fmt.Errorf("no matching target in the loaded data")
	}
	return nil
}

// buildProfileJSON 构造 JSON 画像。
func buildProfileJSON(profile *query.TargetProfile, p queryParams) profileJSON {
	out := profileJSON{
		TargetID:        profile.TargetID,
		IP:              profile.IP,
		Port:            profile.Port,
		Regions:         profile.RegionCount(),
		ProbeTotal:      profile.ProbeTotal,
		ProbeSuccess:    profile.ProbeSuccess,
		SuccessRate:     profile.SuccessRate(),
		TraceTotal:      profile.TraceTotal,
		TraceSuccess:    profile.TraceSuccess,
		LatencySpreadMS: profile.LatencySpreadMS(),
		Latency:         profile.Latency.Snapshot(),
		Errors:          profile.Errors.Sorted(),
		Sessions:        len(profile.Sessions),
	}
	if !profile.FirstSeen.IsZero() {
		out.FirstSeen = profile.FirstSeen.Format(time.RFC3339)
	}
	if !profile.LastSeen.IsZero() {
		out.LastSeen = profile.LastSeen.Format(time.RFC3339)
	}
	versions := make([]string, 0, len(profile.ClientVersions))
	for version := range profile.ClientVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	out.ClientVersions = versions

	regions := profile.RegionsSorted()
	if p.topRegions > 0 && len(regions) > p.topRegions {
		regions = regions[:p.topRegions]
	}

	for _, region := range regions {
		item := regionJSON{
			Label:        region.Label(),
			Country:      region.Country,
			Province:     region.Province,
			City:         region.City,
			ISP:          region.ISP,
			ASN:          region.ASN,
			CollectorID:  region.CollectorID,
			ProbeTotal:   region.ProbeTotal,
			ProbeSuccess: region.ProbeSuccess,
			SuccessRate:  region.SuccessRate(),
			Latency:      region.Latency.Snapshot(),
			Errors:       region.Errors.Sorted(),
			TraceTotal:   region.TraceTotal,
			TraceSuccess: region.TraceSuccess,
			ASPaths:      region.ASPathsSorted(),
			HopCount:     region.HopCount,
			Engine:       region.Engine,
			TraceMode:    region.TraceMode,
		}
		if !region.FirstSeen.IsZero() {
			item.FirstSeen = region.FirstSeen.Format(time.RFC3339)
		}
		if !region.LastSeen.IsZero() {
			item.LastSeen = region.LastSeen.Format(time.RFC3339)
		}

		if p.showHops {
			for _, stat := range region.HopStatsSorted() {
				hop := hopJSON{
					TTL: stat.TTL, IP: stat.IP, ASN: stat.ASN, ASOrg: stat.ASOrg,
					Country: stat.Country, City: stat.City,
					Timeouts: stat.Timeouts, Samples: stat.RTTs.Count,
				}
				if stat.RTTs.Count > 0 {
					hop.MinMS = roundTo(stat.RTTs.Min, 3)
					hop.MaxMS = roundTo(stat.RTTs.Max, 3)
					if median, ok := stat.RTTs.Quantile(0.50); ok {
						hop.P50MS = roundTo(median, 3)
					}
				}
				item.Hops = append(item.Hops, hop)
			}
		}

		out.RegionProfiles = append(out.RegionProfiles, item)
	}

	if p.showSeries {
		for _, point := range profile.Series {
			out.Series = append(out.Series, seriesJSON{
				At:        point.At.Format(time.RFC3339),
				Success:   point.Success,
				LatencyMS: point.LatencyMS,
				ErrorType: point.ErrorType,
			})
		}
	}

	return out
}

// roundTo 保留 n 位小数。
func roundTo(value float64, digits int) float64 {
	scale := 1.0
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	return float64(int64(value*scale+0.5)) / scale
}

// writeProfileText 输出人类可读的画像。
func writeProfileText(w io.Writer, profile *query.TargetProfile, p queryParams) {
	fmt.Fprintf(w, "%s\n", profile.TargetID)
	fmt.Fprintf(w, "  regions:     %d\n", profile.RegionCount())
	fmt.Fprintf(w, "  probes:      %d total, %d ok (%.1f%%)\n",
		profile.ProbeTotal, profile.ProbeSuccess, profile.SuccessRate()*100)
	if profile.TraceTotal > 0 {
		fmt.Fprintf(w, "  traces:      %d total, %d ok\n", profile.TraceTotal, profile.TraceSuccess)
	}
	if snapshot := profile.Latency.Snapshot(); snapshot.Count > 0 {
		fmt.Fprintf(w, "  latency:     min %.1f  p50 %.1f  p90 %.1f  p95 %.1f  max %.1f  avg %.1f ms\n",
			snapshot.MinMS, snapshot.P50MS, snapshot.P90MS, snapshot.P95MS, snapshot.MaxMS, snapshot.AvgMS)
	}
	if spread := profile.LatencySpreadMS(); spread > 0 {
		fmt.Fprintf(w, "  spread:      %.1f ms between the best and worst region (p50)\n", spread)
	}
	if !profile.FirstSeen.IsZero() {
		fmt.Fprintf(w, "  time range:  %s .. %s\n",
			profile.FirstSeen.Format(time.RFC3339), profile.LastSeen.Format(time.RFC3339))
	}

	if len(profile.Errors) > 0 {
		fmt.Fprintf(w, "\n  failures:\n")
		for _, item := range profile.Errors.Sorted() {
			fmt.Fprintf(w, "    %-22s %d\n", item.Type+":", item.Count)
		}
	}

	regions := profile.RegionsSorted()
	if p.topRegions > 0 && len(regions) > p.topRegions {
		regions = regions[:p.topRegions]
	}

	if len(regions) > 0 {
		fmt.Fprintf(w, "\n  by region / ISP:\n")
		fmt.Fprintf(w, "    %-46s %7s %7s %8s %8s\n", "REGION", "PROBES", "OK", "OK%", "P50ms")
		for _, region := range regions {
			p50 := 0.0
			if median, ok := region.Latency.Quantile(0.50); ok {
				p50 = median
			}
			fmt.Fprintf(w, "    %-46s %7d %7d %7.1f%% %8.1f\n",
				truncateForDisplay(region.Label(), 46),
				region.ProbeTotal, region.ProbeSuccess, region.SuccessRate()*100, p50)
		}
	}

	// AS 路径：不同运营商是否走不同出口。
	for _, region := range regions {
		paths := region.ASPathsSorted()
		if len(paths) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  AS path (%s):\n", region.Label())
		for _, path := range paths {
			fmt.Fprintf(w, "    %-60s %d\n", truncateForDisplay(path.Signature, 60), path.Count)
		}
	}

	if p.showHops {
		for _, region := range regions {
			stats := region.HopStatsSorted()
			if len(stats) == 0 {
				continue
			}
			fmt.Fprintf(w, "\n  hops (%s):\n", region.Label())
			fmt.Fprintf(w, "    %3s  %-30s %-9s %6s %9s %9s\n", "TTL", "IP", "ASN", "TO", "P50ms", "MAXms")
			for _, stat := range stats {
				p50, max := 0.0, 0.0
				if stat.RTTs.Count > 0 {
					max = stat.RTTs.Max
					if median, ok := stat.RTTs.Quantile(0.50); ok {
						p50 = median
					}
				}
				ip := stat.IP
				if ip == "" {
					ip = "*"
				}
				fmt.Fprintf(w, "    %3d  %-30s %-9s %6d %9.2f %9.2f\n",
					stat.TTL, truncateForDisplay(ip, 30), stat.ASN, stat.Timeouts, p50, max)
			}
		}
	}

	if p.showSeries && len(profile.Series) > 0 {
		fmt.Fprintf(w, "\n  series (%d points):\n", len(profile.Series))
		for _, point := range profile.Series {
			status := "ok"
			if !point.Success {
				status = point.ErrorType
				if status == "" {
					status = "failed"
				}
			}
			fmt.Fprintf(w, "    %s  %8.1f ms  %s\n",
				point.At.Format("15:04:05"), point.LatencyMS, status)
		}
	}
}

package aggregate

import (
	"sort"
	"strings"
	"time"
)

// Collector 累积聚合状态。
//
// 用法：
//
//	c := NewCollector(Options{MinSamples: 1})
//	c.Add(row)              // 对每一行调用
//	report := c.Report()    // 生成结果
//
// 内存占用与**分组数**成正比，与行数无关（延迟用直方图，不存样本）。
type Collector struct {
	opts Options

	// groups 按 (目标 × 地区 × 运营商) 聚合。
	groups map[string]*groupState

	// targets 按目标聚合（跨地区/运营商），用于回答
	// "同一个目标在不同节点看到的线路是否不同"。
	targets map[string]*targetState

	// collectors 是按 collector_id 的出现统计。
	collectors map[string]*collectorState

	// Load 是输入侧的统计。
	Load LoadStats

	// Skipped 是因为样本不足（MinSamples）而没被报告的目标数。
	Skipped int

	// Dropped 是因为分组数超过 MaxGroups 而没能进分组的行数。
	//
	// 必须计数并报出：静默丢弃会让聚合结果看起来"更干净"，
	// 而使用者无从知道数据并不完整。
	Dropped int64
}

// Options 是聚合配置。
type Options struct {
	// MinSamples 是**目标**进入结果所需的最少样本数（测量数 + 跟踪数）。
	//
	// 刻意作用在目标层面而不是分组层面：跨地区对比的常态是
	// "每个地区只有一两个样本"（一个节点一次扫描对同一目标只测一次），
	// 若按分组过滤，--min-samples 2 会把所有分组藏掉，
	// 跨地区对比直接失效。
	//
	// 默认 1（不过滤）。被过滤掉的目标数记录在 Collector.Skipped
	// 里并被报告出来。
	MinSamples int

	// MaxGroups 是分组数上限，防止内存被畸形输入撑爆。
	//
	// 超过时不再新建分组，并计数（结果里会体现），
	// 而不是静默丢弃——静默丢弃会让聚合结果看起来"更干净"。
	MaxGroups int
}

// DefaultMaxGroups 是默认的分组上限。
const DefaultMaxGroups = 200000

// NewCollector 创建聚合器。
func NewCollector(opts Options) *Collector {
	if opts.MaxGroups <= 0 {
		opts.MaxGroups = DefaultMaxGroups
	}
	return &Collector{
		opts:       opts,
		groups:     make(map[string]*groupState),
		targets:    make(map[string]*targetState),
		collectors: make(map[string]*collectorState),
	}
}

// groupState 是单个 (目标 × 地区 × 运营商) 分组的累积状态。
type groupState struct {
	key GroupKey

	// probe 是 TCP 测量侧的统计。
	probeTotal   int64
	probeSuccess int64
	latency      *LatencyHistogram
	errors       ErrorCounts

	// trace 是线路跟踪侧的统计。
	traceTotal     int64
	traceSuccess   int64
	hopCounts      []int
	respondedHops  []int
	traceErrors    ErrorCounts
	traceDurations *LatencyHistogram

	// first / last 是该分组数据覆盖的时间范围。
	first time.Time
	last  time.Time

	// sessions 是该分组出现过的会话集合（判断数据是否来自多批扫描）。
	sessions map[string]struct{}

	// clientVersions 是产生该分组数据的程序版本集合。
	//
	// 必须记录：测量逻辑演进后不同版本的样本不该被混在一起比较。
	clientVersions map[string]struct{}
}

// targetState 是按目标聚合的状态（跨地区/运营商）。
type targetState struct {
	targetID string
	ip       string
	port     int

	// regions 是该目标出现过的分组键，用于回答
	// "这个目标被多少个不同的地区/运营商测过"。
	regions map[string]GroupKey

	probeTotal   int64
	probeSuccess int64
	latency      *LatencyHistogram

	// asPaths 记录跟踪到的路径（用 AS 序列表示），
	// 用于回答"不同运营商是否走不同出口"。
	asPaths map[string]int64

	traceTotal   int64
	traceSuccess int64

	first time.Time
	last  time.Time
}

// collectorState 是单个采集者的出现统计。
type collectorState struct {
	id       string
	region   string
	rows     int64
	sessions map[string]struct{}
}

// Add 把一行纳入聚合。
//
// 只有**确实带来可聚合内容**的行才会记入采集者维度：
// 一个 kind 正常但缺 payload 的行（或 kind 未知的行）什么都没贡献，
// 把它算成"这个节点参与过"会让 CollectorCount 虚高。
// 这类行仍然被 Load 计入 Rows / UnknownKinds，由报告如实说明。
func (c *Collector) Add(row Row) {
	if c == nil {
		return
	}

	switch row.Kind {
	case "measurement":
		if row.Measurement == nil {
			return
		}
		c.noteCollector(row)
		c.addMeasurement(row)
	case "trace":
		if row.Trace == nil {
			return
		}
		c.noteCollector(row)
		c.addTrace(row)
	}
}

// noteCollector 记录采集者出现情况。
func (c *Collector) noteCollector(row Row) {
	if c.collectors == nil {
		c.collectors = make(map[string]*collectorState)
	}
	id := row.CollectorID
	if id == "" {
		id = "(unknown)"
	}

	state, ok := c.collectors[id]
	if !ok {
		country, province, city, isp, asn, _ := row.CollectorRegion()
		state = &collectorState{
			id:       id,
			region:   regionLabel(country, province, city, isp, asn),
			sessions: make(map[string]struct{}),
		}
		c.collectors[id] = state
	}
	state.rows++
	if row.SessionID != "" {
		state.sessions[row.SessionID] = struct{}{}
	}
}

// regionLabel 拼接地区标签。
func regionLabel(country, province, city, isp, asn string) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{country, province, city, isp, asn} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "/")
}

// groupKeyFor 由一行推导出分组键。
func groupKeyFor(row Row) GroupKey {
	country, province, city, isp, asn, _ := row.CollectorRegion()
	return GroupKey{
		TargetID: row.TargetID,
		Country:  country,
		Province: province,
		City:     city,
		ISP:      isp,
		ASN:      asn,
	}
}

// group 取出（或新建）分组状态。
//
// 超过 MaxGroups 时返回 nil：调用方据此放弃这一行的分组统计，
// 但**仍然把它计入 Load**，由报告如实说明"有行没能进分组"。
func (c *Collector) group(key GroupKey) *groupState {
	if state, ok := c.groups[key.String()]; ok {
		return state
	}
	if len(c.groups) >= c.opts.MaxGroups {
		return nil
	}
	state := &groupState{
		key:            key,
		latency:        NewLatencyHistogram(),
		errors:         ErrorCounts{},
		traceErrors:    ErrorCounts{},
		traceDurations: NewLatencyHistogram(),
		sessions:       make(map[string]struct{}),
		clientVersions: make(map[string]struct{}),
	}
	c.groups[key.String()] = state
	return state
}

// target 取出（或新建）目标状态。
func (c *Collector) target(row Row) *targetState {
	if state, ok := c.targets[row.TargetID]; ok {
		return state
	}
	if len(c.targets) >= c.opts.MaxGroups {
		return nil
	}
	state := &targetState{
		targetID: row.TargetID,
		ip:       row.IP,
		port:     row.Port,
		regions:  make(map[string]GroupKey),
		latency:  NewLatencyHistogram(),
		asPaths:  make(map[string]int64),
	}
	c.targets[row.TargetID] = state
	return state
}

// addMeasurement 累积一条测量。
func (c *Collector) addMeasurement(row Row) {
	at := row.Timestamp()

	key := groupKeyFor(row)
	if state := c.group(key); state != nil {
		state.probeTotal++
		if row.Measurement.Success {
			state.probeSuccess++
			state.latency.Observe(row.Measurement.LatencyMS)
		} else {
			state.errors.Add(row.Measurement.ErrorType)
		}
		if row.SessionID != "" {
			state.sessions[row.SessionID] = struct{}{}
		}
		if row.ClientVersion != "" {
			state.clientVersions[row.ClientVersion] = struct{}{}
		}
		state.noteTime(at)
	} else {
		c.Dropped++
	}

	if state := c.target(row); state != nil {
		state.probeTotal++
		if row.Measurement.Success {
			state.probeSuccess++
			state.latency.Observe(row.Measurement.LatencyMS)
		}
		state.regions[key.String()] = key
		state.noteTime(at)
	}
}

// addTrace 累积一条跟踪。
func (c *Collector) addTrace(row Row) {
	at := row.Timestamp()
	trace := row.Trace

	key := groupKeyFor(row)
	if state := c.group(key); state != nil {
		state.traceTotal++
		if trace.Success {
			state.traceSuccess++
			state.hopCounts = append(state.hopCounts, trace.HopCount)
			state.respondedHops = append(state.respondedHops, trace.RespondedHops)
			state.traceDurations.Observe(trace.DurationMS)
		} else {
			state.traceErrors.Add(trace.ErrorType)
		}
		if row.SessionID != "" {
			state.sessions[row.SessionID] = struct{}{}
		}
		if row.ClientVersion != "" {
			state.clientVersions[row.ClientVersion] = struct{}{}
		}
		state.noteTime(at)
	} else {
		c.Dropped++
	}

	if state := c.target(row); state != nil {
		state.traceTotal++
		if trace.Success {
			state.traceSuccess++
			// 路径签名：把跳上的 ASN 串起来。
			// 这是"不同运营商是否走不同出口"的直接证据。
			if signature := asPathSignature(trace.Hops); signature != "" {
				state.asPaths[signature]++
			}
		}
		state.regions[key.String()] = key
		state.noteTime(at)
	}
}

// asPathSignature 把路径上的 ASN 序列拼成签名。
//
// 只保留有 ASN 的跳，并用 "-" 连接：
//
//	AS9808-AS58453-AS13335
//
// 有了这个签名，"同一个目标在不同运营商下的路径是否不同"
// 就变成一个可以直接比较的字符串。
func asPathSignature(hops []HopView) string {
	parts := make([]string, 0, len(hops))
	for _, hop := range hops {
		if asn := strings.TrimSpace(hop.ASN); asn != "" {
			parts = append(parts, asn)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "-")
}

// noteTime 更新分组的时间范围。
func (s *groupState) noteTime(at time.Time) {
	if at.IsZero() {
		return
	}
	if s.first.IsZero() || at.Before(s.first) {
		s.first = at
	}
	if at.After(s.last) {
		s.last = at
	}
}

// noteTime 更新目标的时间范围。
func (s *targetState) noteTime(at time.Time) {
	if at.IsZero() {
		return
	}
	if s.first.IsZero() || at.Before(s.first) {
		s.first = at
	}
	if at.After(s.last) {
		s.last = at
	}
}

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

// Report 是聚合结果。
type Report struct {
	// SchemaVersion 是聚合输出的版本。
	//
	// 与公开数据的 schema_version 分开编号：聚合结果是本项目的
	// 分析产物，它的结构与原始数据的结构各自演进。
	SchemaVersion int `json:"schema_version"`

	GeneratedAt string `json:"generated_at"`

	Input InputSummary `json:"input"`

	Totals Totals `json:"totals"`

	// Regions 是按地区/运营商汇总的总体表现（"哪个运营商更好"）。
	Regions []RegionSummary `json:"regions"`

	// Targets 是按目标汇总的跨节点表现（"这个目标在全球范围内如何"）。
	Targets []TargetSummary `json:"targets"`

	// Groups 是 (目标 × 地区 × 运营商) 的明细。
	//
	// 可能非常大，因此默认**不**全部输出；
	// 由调用方的 TopN 控制取前多少条。
	Groups []GroupSummary `json:"groups,omitempty"`

	// Collectors 是参与节点数统计。
	CollectorCount int `json:"collector_count"`

	// MultiPathTargets 是"存在多条不同 AS 路径"的目标数。
	MultiPathTargets int `json:"multi_path_targets"`

	// Notes 是结果里需要提醒使用者注意的事项。
	Notes []string `json:"notes,omitempty"`
}

// InputSummary 描述输入数据的概况。
type InputSummary struct {
	Sources      int `json:"sources"`
	Lines        int `json:"lines"`
	Rows         int `json:"rows"`
	Measurements int `json:"measurements"`
	Traces       int `json:"traces"`
	BadLines     int `json:"bad_lines"`
	UnknownKinds int `json:"unknown_kinds"`

	FirstTimestamp string  `json:"first_timestamp,omitempty"`
	LastTimestamp  string  `json:"last_timestamp,omitempty"`
	SpanHours      float64 `json:"span_hours,omitempty"`
}

// Totals 是全局合计。
type Totals struct {
	Targets     int `json:"targets"`
	Groups      int `json:"groups"`
	GroupsShown int `json:"groups_shown"`

	ProbeTotal   int64 `json:"probe_total"`
	ProbeSuccess int64 `json:"probe_success"`
	TraceTotal   int64 `json:"trace_total"`
	TraceSuccess int64 `json:"trace_success"`

	// ProbeSuccessRate / TraceSuccessRate 是全局成功率。
	ProbeSuccessRate float64 `json:"probe_success_rate"`
	TraceSuccessRate float64 `json:"trace_success_rate"`

	// Latency 是全体成功测量的延迟分布（近似分位数）。
	Latency Histogram `json:"latency"`

	// Errors 是全局失败分类。
	Errors []ErrorCount `json:"errors,omitempty"`
}

// RegionSummary 是按地区/运营商汇总。
type RegionSummary struct {
	Country  string `json:"country,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	ISP      string `json:"isp,omitempty"`
	ASN      string `json:"asn,omitempty"`

	Label string `json:"label"`

	Targets int `json:"targets"`

	ProbeTotal   int64   `json:"probe_total"`
	ProbeSuccess int64   `json:"probe_success"`
	SuccessRate  float64 `json:"success_rate"`

	Latency Histogram `json:"latency"`

	Errors []ErrorCount `json:"errors,omitempty"`
}

// TargetSummary 是按目标汇总。
type TargetSummary struct {
	TargetID string `json:"target_id"`
	IP       string `json:"ip,omitempty"`
	Port     int    `json:"port,omitempty"`

	// Regions 是测过这个目标的地区/运营商数量。
	//
	// 这个数字直接体现"众测"的价值：1 表示只有一个节点测过，
	// 数字越大越能反映"不同地方看它是否不同"。
	Regions int `json:"regions"`

	ProbeTotal   int64   `json:"probe_total"`
	ProbeSuccess int64   `json:"probe_success"`
	SuccessRate  float64 `json:"success_rate"`

	Latency Histogram `json:"latency"`

	// LatencySpreadMS 是各分组延迟中位数的最大值与最小值之差。
	//
	// 这是"同一个目标在不同地区表现差异有多大"的**直接度量**：
	// 差异大说明这条线路对位置敏感，是很有价值的信号。
	// 只有 1 个分组时为 0。
	LatencySpreadMS float64 `json:"latency_spread_ms"`

	TraceTotal   int64 `json:"trace_total"`
	TraceSuccess int64 `json:"trace_success"`

	// DistinctASPaths 是该目标被跟踪到的不同 AS 路径数量。
	//
	// >1 表示"不同运营商确实走了不同出口"——这正是需求里
	// 想回答的核心问题之一。
	DistinctASPaths int `json:"distinct_as_paths,omitempty"`
}

// GroupSummary 是单个 (目标 × 地区 × 运营商) 分组。
type GroupSummary struct {
	TargetID string `json:"target_id"`
	IP       string `json:"ip,omitempty"`
	Port     int    `json:"port,omitempty"`

	Country  string `json:"country,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	ISP      string `json:"isp,omitempty"`
	ASN      string `json:"asn,omitempty"`

	ProbeTotal   int64   `json:"probe_total"`
	ProbeSuccess int64   `json:"probe_success"`
	SuccessRate  float64 `json:"success_rate"`

	Latency Histogram `json:"latency"`

	TraceTotal       int64   `json:"trace_total"`
	TraceSuccess     int64   `json:"trace_success"`
	AvgHopCount      float64 `json:"avg_hop_count,omitempty"`
	AvgRespondedHops float64 `json:"avg_responded_hops,omitempty"`

	Errors      []ErrorCount `json:"errors,omitempty"`
	TraceErrors []ErrorCount `json:"trace_errors,omitempty"`

	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`

	// Sessions 是该分组数据来自多少个不同的扫描会话。
	Sessions int `json:"sessions"`

	// ClientVersions 是产生这些数据的程序版本。
	ClientVersions []string `json:"client_versions,omitempty"`
}

// average 返回整数切片的平均值（空切片返回 0）。
func average(values []int) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0
	for _, v := range values {
		sum += v
	}
	return float64(sum) / float64(len(values))
}

// successRate 返回成功率（无样本返回 0）。
func successRate(success, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(success) / float64(total)
}

// sortGroupsByTargetAndRate 按目标 ID 与成功率排序，保证输出稳定。
func sortGroupsByTargetAndRate(groups []GroupSummary) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].TargetID != groups[j].TargetID {
			return groups[i].TargetID < groups[j].TargetID
		}
		return groups[i].ISP < groups[j].ISP
	})
}

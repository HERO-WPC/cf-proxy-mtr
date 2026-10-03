// Package query 提供面向分析的查询：给定一个 IP:Port，回答
// "它在不同地区/运营商下表现如何"。
//
// 与 storage 包的分工：
//
//	storage  负责"把事实从库里取出来"（原始行、按条件过滤）
//	query    负责"把事实组织成结论"（分组、分位数、时间序列）
//
// 与 aggregate 包的分工：
//
//	aggregate 回答"这一批数据整体如何"（全局、跨目标）
//	query     回答"这一个目标如何"（单目标、跨维度下钻）
//
// 数据源可以是数据库（本地历史）或 JSONL（公开数据），
// 因此这里定义自己的 Row 读取接口，两条路径共用同一套统计逻辑——
// 否则"本地查到的数字"与"聚合出来的数字"会不一致。
package query

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/aggregate"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// Row 是查询层接受的输入行。
//
// 刻意与 aggregate.Row 保持相同形状并直接复用其类型：
// 两份结构定义迟早会漂移，而"数据库里的行"与"JSONL 里的行"
// 描述的是同一件事。
type Row = aggregate.Row

// Dataset 是载入内存的数据集。
//
// 为什么载入内存：线路画像需要分位数（要直方图）、需要按地区分组
// （要多次遍历同一目标）、还需要时间序列。用 SQL 逐项查会让
// 一次画像变成十几次查询，而且无法支持 JSONL 数据源。
// 本地库的规模（单机众测）适合这个选择。
type Dataset struct {
	targets map[string]*TargetProfile

	// TotalRows / BadRows 是载入统计。
	TotalRows int
	BadRows   int

	// FirstTimestamp / LastTimestamp 是数据覆盖的时间范围。
	FirstTimestamp time.Time
	LastTimestamp  time.Time

	// Collectors 是出现过的匿名采集者标识。
	Collectors map[string]struct{}
}

// NewDataset 创建空数据集。
func NewDataset() *Dataset {
	return &Dataset{
		targets:    make(map[string]*TargetProfile),
		Collectors: make(map[string]struct{}),
	}
}

// TargetProfile 是单个目标的线路画像。
type TargetProfile struct {
	TargetID string
	IP       string
	Port     int

	// Location / Colo 是目标自身与 Cloudflare 接入点的信息
	// （来自公开的 all.json，随行带入）。
	Location aggregate.Row // 仅取 TargetMeta 相关字段

	ProbeTotal   int64
	ProbeSuccess int64

	// Latency 是全部成功测量的延迟分布。
	Latency *aggregate.LatencyHistogram

	// Errors 是失败分类计数。
	Errors aggregate.ErrorCounts

	TraceTotal   int64
	TraceSuccess int64

	// Regions 是按 (地区 × 运营商) 的分组画像。
	Regions map[string]*RegionProfile

	// Series 是时间序列样本（按时间升序），受 MaxSeries 限制。
	Series []SeriesPoint

	// FirstSeen / LastSeen 是该目标数据覆盖的时间范围。
	FirstSeen time.Time
	LastSeen  time.Time

	// Sessions 是测过该目标的会话集合。
	Sessions map[string]struct{}

	// ClientVersions 是产生这些数据的程序版本集合。
	ClientVersions map[string]struct{}
}

// RegionProfile 是单个 (地区 × 运营商) 分组下的画像。
type RegionProfile struct {
	Country  string
	Province string
	City     string
	ISP      string
	ASN      string

	CollectorID string

	ProbeTotal   int64
	ProbeSuccess int64

	Latency *aggregate.LatencyHistogram
	Errors  aggregate.ErrorCounts

	TraceTotal     int64
	TraceSuccess   int64
	TraceErrorCn   aggregate.ErrorCounts
	TraceDurations *aggregate.LatencyHistogram

	// Hops 是该分组下最后一次成功跟踪的路径
	// （多次跟踪时取最近一次，避免把不同时间的路径混在一起）。
	Hops      []aggregate.HopView
	HopCount  int
	TracedAt  time.Time
	Engine    string
	TraceMode string

	// ASPaths 是该分组下出现过的 AS 路径签名与次数。
	ASPaths map[string]int64

	// HopStats 是各跳的延迟统计（按 TTL 聚合）。
	HopStats map[int]*HopStat

	FirstSeen time.Time
	LastSeen  time.Time
}

// HopStat 是某一跳的累积统计。
type HopStat struct {
	TTL       int
	IP        string
	ASN       string
	ASOrg     string
	Country   string
	City      string
	Timeouts  int
	RTTs      *aggregate.LatencyHistogram
	FirstSeen time.Time
	LastSeen  time.Time
}

// SeriesPoint 是时间序列上的一个点。
type SeriesPoint struct {
	At        time.Time
	Success   bool
	LatencyMS float64
	ErrorType string
	Region    string
}

// MaxSeries 是每个目标保留的时间序列样本上限。
//
// 用途是"看趋势"而不是"精确统计"，因此只保留最近的一部分即可；
// 精确统计由直方图负责，它不受这个上限影响。
const MaxSeries = 2000

// IndexOptions 是数据集构建选项。
type IndexOptions struct {
	// MaxTargets 是索引的目标数上限（防止内存被畸形输入撑爆）。
	MaxTargets int

	// MaxSeries 是每个目标保留的时间序列点数（<=0 时使用 MaxSeries）。
	MaxSeries int
}

// DefaultMaxTargets 是默认的目标索引上限。
const DefaultMaxTargets = 500000

// IndexRows 把一批行载入数据集。
//
// 与 aggregate 一致：坏行由调用方（Load 的 stats）计数，
// 这里只处理"能解析出来的行"。
func (d *Dataset) IndexRows(rows []Row, opts IndexOptions) {
	if opts.MaxTargets <= 0 {
		opts.MaxTargets = DefaultMaxTargets
	}
	if opts.MaxSeries <= 0 {
		opts.MaxSeries = MaxSeries
	}

	for i := range rows {
		row := rows[i]
		d.TotalRows++

		d.noteCollector(row)
		d.noteTimeRange(row)

		if row.TargetID == "" {
			d.BadRows++
			continue
		}

		profile, ok := d.targets[row.TargetID]
		if !ok {
			if len(d.targets) >= opts.MaxTargets {
				d.BadRows++
				continue
			}
			profile = newTargetProfile(row)
			d.targets[row.TargetID] = profile
		}

		switch row.Kind {
		case "measurement":
			if row.Measurement == nil {
				continue
			}
			d.indexMeasurement(profile, row, opts.MaxSeries)
		case "trace":
			if row.Trace == nil {
				continue
			}
			d.indexTrace(profile, row)
		}
	}
}

// noteCollector 记录采集者出现。
func (d *Dataset) noteCollector(row Row) {
	if row.CollectorID != "" {
		d.Collectors[row.CollectorID] = struct{}{}
	}
}

// noteTimeRange 更新数据集的时间范围。
func (d *Dataset) noteTimeRange(row Row) {
	at := row.Timestamp()
	if at.IsZero() {
		return
	}
	if d.FirstTimestamp.IsZero() || at.Before(d.FirstTimestamp) {
		d.FirstTimestamp = at
	}
	if at.After(d.LastTimestamp) {
		d.LastTimestamp = at
	}
}

// newTargetProfile 创建目标画像。
func newTargetProfile(row Row) *TargetProfile {
	return &TargetProfile{
		TargetID:       row.TargetID,
		IP:             row.IP,
		Port:           row.Port,
		Location:       row,
		Latency:        aggregate.NewLatencyHistogram(),
		Errors:         aggregate.ErrorCounts{},
		Regions:        make(map[string]*RegionProfile),
		Sessions:       make(map[string]struct{}),
		ClientVersions: make(map[string]struct{}),
	}
}

// regionKeyOf 由一行推导分组键。
func regionKeyOf(row Row) string {
	country, province, city, isp, asn, _ := row.CollectorRegion()
	return strings.Join([]string{country, province, city, isp, asn}, "|")
}

// regionOf 取出（或新建）目标下的地区画像。
func (p *TargetProfile) regionOf(row Row) *RegionProfile {
	key := regionKeyOf(row)
	if existing, ok := p.Regions[key]; ok {
		return existing
	}
	country, province, city, isp, asn, _ := row.CollectorRegion()
	region := &RegionProfile{
		Country:        country,
		Province:       province,
		City:           city,
		ISP:            isp,
		ASN:            asn,
		CollectorID:    row.CollectorID,
		Latency:        aggregate.NewLatencyHistogram(),
		Errors:         aggregate.ErrorCounts{},
		TraceErrorCn:   aggregate.ErrorCounts{},
		TraceDurations: aggregate.NewLatencyHistogram(),
		ASPaths:        make(map[string]int64),
		HopStats:       make(map[int]*HopStat),
	}
	p.Regions[key] = region
	return region
}

// noteTime 更新地区画像的时间范围。
func (r *RegionProfile) noteTime(at time.Time) {
	if at.IsZero() {
		return
	}
	if r.FirstSeen.IsZero() || at.Before(r.FirstSeen) {
		r.FirstSeen = at
	}
	if at.After(r.LastSeen) {
		r.LastSeen = at
	}
}

// indexMeasurement 索引一条测量。
func (d *Dataset) indexMeasurement(profile *TargetProfile, row Row, maxSeries int) {
	at := row.Timestamp()
	m := row.Measurement

	profile.ProbeTotal++
	if m.Success {
		profile.ProbeSuccess++
		profile.Latency.Observe(m.LatencyMS)
	} else {
		profile.Errors.Add(m.ErrorType)
	}
	profile.noteTime(at)
	if row.SessionID != "" {
		profile.Sessions[row.SessionID] = struct{}{}
	}
	if row.ClientVersion != "" {
		profile.ClientVersions[row.ClientVersion] = struct{}{}
	}

	region := profile.regionOf(row)
	region.ProbeTotal++
	if m.Success {
		region.ProbeSuccess++
		region.Latency.Observe(m.LatencyMS)
	} else {
		region.Errors.Add(m.ErrorType)
	}
	region.noteTime(at)

	// 时间序列：只保留最近 maxSeries 个点。
	// 保留"最近"而不是"最早"：看趋势时最近的数据更有用。
	point := SeriesPoint{
		At:        at,
		Success:   m.Success,
		LatencyMS: m.LatencyMS,
		ErrorType: m.ErrorType,
		Region:    regionKeyOf(row),
	}
	if len(profile.Series) < maxSeries {
		profile.Series = append(profile.Series, point)
	} else {
		// 环形覆盖：丢掉最旧的。
		copy(profile.Series, profile.Series[1:])
		profile.Series[len(profile.Series)-1] = point
	}
}

// indexTrace 索引一条跟踪。
func (d *Dataset) indexTrace(profile *TargetProfile, row Row) {
	at := row.Timestamp()
	trace := row.Trace

	profile.TraceTotal++
	if trace.Success {
		profile.TraceSuccess++
	}
	profile.noteTime(at)

	region := profile.regionOf(row)
	region.TraceTotal++
	if trace.Success {
		region.TraceSuccess++
		region.TraceDurations.Observe(trace.DurationMS)
	} else {
		region.TraceErrorCn.Add(trace.ErrorType)
	}
	region.noteTime(at)

	// 该分组的最近一次成功路径。
	if trace.Success && (region.TracedAt.IsZero() || at.After(region.TracedAt)) {
		region.Hops = trace.Hops
		region.HopCount = trace.HopCount
		region.TracedAt = at
		region.Engine = trace.Engine
		region.TraceMode = trace.Mode
	}
	if trace.Success {
		if signature := asPathSignature(trace.Hops); signature != "" {
			region.ASPaths[signature]++
		}
		d.indexHops(region, trace.Hops, at)
	}
}

// indexHops 累积各跳的统计。
//
// 按 TTL 聚合而不是按 IP：同一个 TTL 在不同时间可能由不同的
// 等价路由器应答（负载分担），按 IP 会让同一跳出现多个条目。
func (d *Dataset) indexHops(region *RegionProfile, hops []aggregate.HopView, at time.Time) {
	for _, hop := range hops {
		stat, ok := region.HopStats[hop.TTL]
		if !ok {
			stat = &HopStat{TTL: hop.TTL, RTTs: aggregate.NewLatencyHistogram()}
			region.HopStats[hop.TTL] = stat
		}

		if hop.Timeout || hop.IP == "" {
			stat.Timeouts++
		} else {
			stat.IP = hop.IP
		}
		if hop.ASN != "" {
			stat.ASN = hop.ASN
		}
		if hop.ASOrganization != "" {
			stat.ASOrg = hop.ASOrganization
		}
		if hop.Country != "" {
			stat.Country = hop.Country
		}
		if hop.City != "" {
			stat.City = hop.City
		}
		for _, rtt := range hop.RTTMS {
			stat.RTTs.Observe(rtt)
		}
		if stat.FirstSeen.IsZero() || at.Before(stat.FirstSeen) {
			stat.FirstSeen = at
		}
		if at.After(stat.LastSeen) {
			stat.LastSeen = at
		}
	}
}

// asPathSignature 把 AS 序列拼成签名。
func asPathSignature(hops []aggregate.HopView) string {
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

// noteTime 更新目标画像的时间范围。
func (p *TargetProfile) noteTime(at time.Time) {
	if at.IsZero() {
		return
	}
	if p.FirstSeen.IsZero() || at.Before(p.FirstSeen) {
		p.FirstSeen = at
	}
	if at.After(p.LastSeen) {
		p.LastSeen = at
	}
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// ErrTargetNotFound 表示数据集中没有该目标。
var ErrTargetNotFound = fmt.Errorf("target not found in the loaded data")

// Lookup 返回单个目标的画像。
//
// target 接受 "IP:Port"，也接受裸 IP（此时取该 IP 的全部端口，
// 但只有恰好一个端口时才返回，避免悄悄给出错误的目标）。
func (d *Dataset) Lookup(target string) (*TargetProfile, error) {
	return d.LookupContext(context.Background(), target)
}

// LookupContext 是带 context 的 Lookup（为将来的流式实现留出位置）。
func (d *Dataset) LookupContext(_ context.Context, target string) (*TargetProfile, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil, fmt.Errorf("empty target")
	}

	// 直接命中。
	if profile, ok := d.targets[trimmed]; ok {
		return profile, nil
	}

	// 规范化后再试（例如 IPv6 的写法差异）。
	normalized, err := normalizeTargetID(trimmed)
	if err == nil {
		if profile, ok := d.targets[normalized]; ok {
			return profile, nil
		}
	}

	// 裸 IP：找出该 IP 的全部端口。
	matches := d.matchByIP(trimmed)
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrTargetNotFound, trimmed)
	case 1:
		return matches[0], nil
	default:
		ports := make([]string, 0, len(matches))
		for _, match := range matches {
			ports = append(ports, fmt.Sprintf("%d", match.Port))
		}
		sort.Strings(ports)
		return nil, fmt.Errorf(
			"%s matches %d ports (%s); specify the port, e.g. %s:%s",
			trimmed, len(matches), strings.Join(ports, ", "), trimmed, ports[0])
	}
}

// normalizeTargetID 把 "IP:Port" 规范成模型使用的形式。
func normalizeTargetID(raw string) (string, error) {
	host, port, err := splitHostPort(raw)
	if err != nil {
		return "", err
	}
	target, err := model.NewTargetFromStrings(host, port)
	if err != nil {
		return "", err
	}
	return target.ID, nil
}

// splitHostPort 把 "IP:Port" 拆成地址与端口。
//
// 支持 IPv6 的两种写法：
//
//	[2001:db8::1]:443   带方括号（模型与命令行都接受）
//	2001:db8::1         不带端口（调用方给默认端口）
//
// 用 net.SplitHostPort 处理方括号形式，用 model 的解析处理裸地址，
// 避免自己写正则去猜冒号属于谁。
func splitHostPort(raw string) (string, int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", 0, fmt.Errorf("empty target")
	}

	// 带方括号的 IPv6，或普通的 host:port。
	if strings.HasPrefix(trimmed, "[") || strings.Count(trimmed, ":") == 1 {
		host, portText, err := net.SplitHostPort(trimmed)
		if err != nil {
			return "", 0, fmt.Errorf("parse %q: %w (want IP:PORT)", raw, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return "", 0, fmt.Errorf("parse port %q: %w", portText, err)
		}
		return host, port, nil
	}

	// 裸 IPv6（多个冒号且没有方括号）：没有端口信息。
	if _, ok := model.ParseAddr(strings.Trim(trimmed, "[]")); ok {
		return "", 0, fmt.Errorf("%q is an IP without a port; write %s:443", raw, trimmed)
	}

	return "", 0, fmt.Errorf("parse %q: want IP:PORT", raw)
}

// matchByIP 返回 IP 相同的全部目标。
func (d *Dataset) matchByIP(ip string) []*TargetProfile {
	out := make([]*TargetProfile, 0, 4)
	for _, profile := range d.targets {
		if profile.IP == ip {
			out = append(out, profile)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// Targets 返回全部目标画像（按 TargetID 排序，保证输出稳定）。
func (d *Dataset) Targets() []*TargetProfile {
	out := make([]*TargetProfile, 0, len(d.targets))
	for _, profile := range d.targets {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}

// TargetCount 返回索引到的目标数。
func (d *Dataset) TargetCount() int { return len(d.targets) }

// Regions 返回某个目标的分组画像（按样本数降序）。
func (p *TargetProfile) RegionsSorted() []*RegionProfile {
	out := make([]*RegionProfile, 0, len(p.Regions))
	for _, region := range p.Regions {
		out = append(out, region)
	}
	sort.Slice(out, func(i, j int) bool {
		totalI := out[i].ProbeTotal + out[i].TraceTotal
		totalJ := out[j].ProbeTotal + out[j].TraceTotal
		if totalI != totalJ {
			return totalI > totalJ
		}
		return regionLabel(out[i]) < regionLabel(out[j])
	})
	return out
}

// regionLabel 拼接地区标签。
func regionLabel(r *RegionProfile) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{r.Country, r.Province, r.City, r.ISP, r.ASN} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "(unknown region)"
	}
	return strings.Join(parts, "/")
}

// Label 返回地区标签。
func (r *RegionProfile) Label() string { return regionLabel(r) }

// SuccessRate 返回成功率。
func (p *TargetProfile) SuccessRate() float64 {
	if p.ProbeTotal == 0 {
		return 0
	}
	return float64(p.ProbeSuccess) / float64(p.ProbeTotal)
}

// SuccessRate 返回该分组的成功率。
func (r *RegionProfile) SuccessRate() float64 {
	if r.ProbeTotal == 0 {
		return 0
	}
	return float64(r.ProbeSuccess) / float64(r.ProbeTotal)
}

// HopStatsSorted 返回按 TTL 排序的跳统计。
func (r *RegionProfile) HopStatsSorted() []*HopStat {
	out := make([]*HopStat, 0, len(r.HopStats))
	for _, stat := range r.HopStats {
		out = append(out, stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TTL < out[j].TTL })
	return out
}

// ASAPathsSorted 返回按出现次数降序的 AS 路径。
func (r *RegionProfile) ASPathsSorted() []ASPath {
	out := make([]ASPath, 0, len(r.ASPaths))
	for signature, count := range r.ASPaths {
		out = append(out, ASPath{Signature: signature, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	return out
}

// ASPath 是一条 AS 路径及其出现次数。
type ASPath struct {
	Signature string `json:"signature"`
	Count     int64  `json:"count"`
}

// LatencySpreadMS 返回各分组延迟中位数的极差。
//
// 这是"同一个目标在不同地区表现差异有多大"的直接度量。
func (p *TargetProfile) LatencySpreadMS() float64 {
	medians := make([]float64, 0, len(p.Regions))
	for _, region := range p.Regions {
		if median, ok := region.Latency.Quantile(0.50); ok {
			medians = append(medians, median)
		}
	}
	if len(medians) < 2 {
		return 0
	}
	low, high := medians[0], medians[0]
	for _, value := range medians[1:] {
		if value < low {
			low = value
		}
		if value > high {
			high = value
		}
	}
	return high - low
}

// RegionCount 返回测过该目标的分组数。
func (p *TargetProfile) RegionCount() int { return len(p.Regions) }

// Package aggregate 把公开 JSONL 数据聚合成统计结果。
//
// 它消费的是 internal/export 的产物（本地导出或从别处下载的批次），
// **不直接读数据库**。这个边界很重要：
//
//   - 聚合结果必须能从公开数据复现。如果它依赖本地库，
//     第三方就无法独立验证我们的数字；
//   - 聚合是"众测"的部分：把很多节点的导出批次放在一起看，
//     才能回答"不同地区/运营商到同一目标的线路是否不同"。
//
// 因此本包只认 JSONL，不认 SQLite。
package aggregate

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// CollectorRef 是行里采集者的地区/运营商信息。
//
// 用**具名类型**而不是内联匿名结构：匿名结构无法从包外构造，
// 于是"把数据库行转成公开行"这种事（见 internal/query）就做不到，
// 只能复制一份结构定义——那正是两条路径迟早漂移的原因。
type CollectorRef struct {
	Country   string `json:"country"`
	Province  string `json:"province"`
	City      string `json:"city"`
	ISP       string `json:"isp"`
	ASN       string `json:"asn"`
	IPVersion string `json:"ip_version"`
}

// TargetMetaRef 是行里目标的上游元数据。
type TargetMetaRef struct {
	Country string   `json:"country"`
	CCA2    string   `json:"cca2"`
	Region  string   `json:"region"`
	City    string   `json:"city"`
	Colo    *ColoRef `json:"colo"`
}

// ColoRef 是 Cloudflare 接入点信息。
type ColoRef struct {
	IATA string `json:"iata"`
	CCA2 string `json:"cca2"`
	City string `json:"city"`
}

// MeasurementRef 是行里的测量结果。
type MeasurementRef struct {
	Success      bool    `json:"success"`
	LatencyMS    float64 `json:"latency_ms"`
	ErrorType    string  `json:"error_type"`
	ErrorMessage string  `json:"error_message"`
}

// TraceRef 是行里的跟踪结果。
type TraceRef struct {
	Success       bool      `json:"success"`
	Engine        string    `json:"engine"`
	EngineVersion string    `json:"engine_version"`
	Mode          string    `json:"mode"`
	DurationMS    float64   `json:"duration_ms"`
	HopCount      int       `json:"hop_count"`
	RespondedHops int       `json:"responded_hops"`
	LocalFiltered bool      `json:"local_filtered"`
	ErrorType     string    `json:"error_type"`
	Hops          []HopView `json:"hops"`
}

// Row 是输入 JSONL 的一行（公开 Schema 的读取视图）。
//
// 只声明聚合需要的字段：多余的字段被忽略，缺失的字段按零值处理。
// 这样即使导出端加了新字段，聚合端也不需要跟着改。
type Row struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	ClientVersion string `json:"client_version"`

	TargetID string `json:"target_id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	TimestampUTC string `json:"timestamp_utc"`
	SessionID    string `json:"session_id"`
	CollectorID  string `json:"collector_id"`

	Collector   *CollectorRef   `json:"collector"`
	TargetMeta  *TargetMetaRef  `json:"target_meta"`
	Measurement *MeasurementRef `json:"measurement"`
	Trace       *TraceRef       `json:"trace"`
}

// HopView 是聚合侧对一跳的读取视图。
//
// 用具名类型而不是内联匿名结构：匿名结构无法作为参数类型传递，
// 而路径签名需要的正是"一串跳"。
type HopView struct {
	TTL            int       `json:"ttl"`
	IP             string    `json:"ip"`
	RTTMS          []float64 `json:"rtt_ms"`
	Timeout        bool      `json:"timeout"`
	ASN            string    `json:"asn"`
	ASOrganization string    `json:"as_organization"`
	Country        string    `json:"country"`
	City           string    `json:"city"`
}

// Timestamp 解析行内时间；失败时返回零值。
func (r Row) Timestamp() time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, r.TimestampUTC)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// CollectorRegion 返回采集者的地区/运营商标签（用于分组）。
func (r Row) CollectorRegion() (country, province, city, isp, asn, ipVersion string) {
	if r.Collector == nil {
		return "", "", "", "", "", ""
	}
	return r.Collector.Country, r.Collector.Province, r.Collector.City,
		r.Collector.ISP, r.Collector.ASN, r.Collector.IPVersion
}

// ---------------------------------------------------------------------------
// 输入
// ---------------------------------------------------------------------------

// LoadStats 是读取输入时的统计。
type LoadStats struct {
	// Lines 是读到的非空行数。
	Lines int

	// Rows 是成功解析的行数。
	Rows int

	// Measurements / Traces 是各类型的行数。
	Measurements int
	Traces       int

	// BadLines 是无法解析的行数。
	//
	// 必须计数并报出：一个静默跳过坏行的聚合器会给出
	// 看似正常但实际基于不完整数据的结论。
	BadLines int

	// UnknownKinds 是 kind 不是 measurement/trace 的行数。
	UnknownKinds int

	// Sources 是读过的文件数。
	Sources int

	// FirstTimestamp / LastTimestamp 是数据覆盖的时间范围。
	FirstTimestamp time.Time
	LastTimestamp  time.Time
}

// Load 从 reader 读取 JSONL 并逐行回调。
//
// 回调返回错误时立即停止（例如磁盘写满）。
func Load(r io.Reader, stats *LoadStats, fn func(Row) error) error {
	scanner := bufio.NewScanner(r)

	// 单行可能很长（一条跟踪含 30 跳的完整信息）。
	// 默认上限 64 KiB 会对大跳表报 "token too long"，
	// 因此把上限提到 8 MiB。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if stats != nil {
			stats.Lines++
		}

		var row Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			if stats != nil {
				stats.BadLines++
			}
			continue
		}

		if stats != nil {
			stats.Rows++
			switch row.Kind {
			case "measurement":
				stats.Measurements++
			case "trace":
				stats.Traces++
			default:
				stats.UnknownKinds++
			}

			if at := row.Timestamp(); !at.IsZero() {
				if stats.FirstTimestamp.IsZero() || at.Before(stats.FirstTimestamp) {
					stats.FirstTimestamp = at
				}
				if at.After(stats.LastTimestamp) {
					stats.LastTimestamp = at
				}
			}
		}

		if err := fn(row); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ---------------------------------------------------------------------------
// 延迟分位数
// ---------------------------------------------------------------------------

// LatencyHistogram 是一个内存有界、可合并的延迟分布。
//
// 为什么不用"把所有样本存进切片再排序"：聚合的输入是**多节点长期**
// 的公开数据，单个目标可能积累几十万个样本；把每个样本都留在内存里
// 会让聚合无法在普通机器上跑完。
//
// 做法是固定对数分桶（相对误差约 5%），内存占用与样本数无关。
// 代价是分位数是**近似值**，因此每个统计结果都带 approx 标记，
// 绝不把近似值当精确值报出去。
type LatencyHistogram struct {
	// Count 是样本总数。
	Count int64

	// Sum 是总和（用于精确平均值）。
	Sum float64

	// Min / Max 是精确极值（极值容易精确维护，不该被近似）。
	Min float64
	Max float64

	// buckets[i] 是第 i 个桶的计数。
	buckets []int64
}

// 分桶参数：从 0.1 ms 到约 130 秒，覆盖任何合理的网络延迟。
const (
	histogramMinMS = 0.1
	histogramMaxMS = 131072.0 // 2^17
	histogramScale = 20.0     // 每倍频程 20 个桶 -> 相对误差约 3.5%
)

// bucketCount 是总桶数。
var bucketCount = int(math.Ceil(math.Log(histogramMaxMS/histogramMinMS) * histogramScale))

// NewLatencyHistogram 创建空的直方图。
func NewLatencyHistogram() *LatencyHistogram {
	return &LatencyHistogram{buckets: make([]int64, bucketCount)}
}

// bucketIndex 把毫秒值映射到桶号。
func bucketIndex(value float64) int {
	if value < histogramMinMS {
		return 0
	}
	if value > histogramMaxMS {
		return bucketCount - 1
	}
	index := int(math.Log(value/histogramMinMS) * histogramScale)
	if index < 0 {
		return 0
	}
	if index >= bucketCount {
		return bucketCount - 1
	}
	return index
}

// bucketValue 返回某个桶的代表值（桶的几何中点）。
func bucketValue(index int) float64 {
	low := histogramMinMS * math.Exp(float64(index)/histogramScale)
	high := histogramMinMS * math.Exp(float64(index+1)/histogramScale)
	return math.Sqrt(low * high)
}

// Observe 记录一个样本。
//
// 非正值、NaN、Inf 都被忽略：
//
//   - 负值与 NaN 显然无效；
//   - **0 也必须丢掉**。0 在真实延迟里意味着"没有测到"（失败的探测、
//     超时跳），而不是"0 毫秒"。收进来会让分位数被一堆 0 拉垮——
//     实测表现是"只有一个 1.31ms 样本的跳，p50 却报成 0.00"。
//
// 代价是无法用直方图回答"有多少次没测到"，那由调用方单独计数
// （见 query 包里的 HopStat.Timeouts）。
func (h *LatencyHistogram) Observe(ms float64) {
	if h == nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 {
		return
	}
	if len(h.buckets) == 0 {
		h.buckets = make([]int64, bucketCount)
	}

	h.Count++
	h.Sum += ms
	if h.Count == 1 || ms < h.Min {
		h.Min = ms
	}
	if ms > h.Max {
		h.Max = ms
	}
	h.buckets[bucketIndex(ms)]++
}

// Merge 合并另一个直方图。
//
// 可合并是选直方图而不是"存样本"的另一个理由：
// 每个分组各维护一个直方图，最后再合并出全局分布，
// 不需要保留任何原始样本。
func (h *LatencyHistogram) Merge(other *LatencyHistogram) {
	if h == nil || other == nil || other.Count == 0 {
		return
	}
	if len(h.buckets) == 0 {
		h.buckets = make([]int64, bucketCount)
	}

	wasEmpty := h.Count == 0

	h.Count += other.Count
	h.Sum += other.Sum

	// 极值精确维护：合并时取两边更极端的值。
	if wasEmpty || other.Min < h.Min {
		h.Min = other.Min
	}
	if other.Max > h.Max {
		h.Max = other.Max
	}

	for i, count := range other.buckets {
		h.buckets[i] += count
	}
}

// Mean 返回平均值（精确，因为用了累加和）。
func (h *LatencyHistogram) Mean() float64 {
	if h == nil || h.Count == 0 {
		return 0
	}
	return h.Sum / float64(h.Count)
}

// Quantile 返回近似分位数（p 取 0~1）。
//
// 返回值可能受分桶误差影响，但**一定落在 [Min, Max] 之间**：
// 桶的代表值是几何中点，可能略微超出该桶内真实样本的范围，
// 于是出现"p90 = 305.6 而 max = 300.1"这种自相矛盾的输出
// （实测踩到过）。这里显式夹紧到精确极值，保证结果不自相矛盾。
//
// 返回的第二个值表示"有值"。对空直方图返回 0/false。
func (h *LatencyHistogram) Quantile(p float64) (float64, bool) {
	if h == nil || h.Count == 0 {
		return 0, false
	}
	if p <= 0 {
		return h.Min, true
	}
	if p >= 1 {
		return h.Max, true
	}

	// 极值精确可用时，直接用精确值回答 0 与 1 附近的请求。
	target := int64(math.Ceil(p * float64(h.Count)))
	var cumulative int64
	for i, count := range h.buckets {
		cumulative += count
		if cumulative >= target {
			return clamp(h.Min, bucketValue(i), h.Max), true
		}
	}
	return h.Max, true
}

// clamp 把 value 夹在 [low, high] 之间。
func clamp(low, value, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Histogram 是可序列化的分位数快照。
type Histogram struct {
	Count int64   `json:"count"`
	MinMS float64 `json:"min_ms"`
	MaxMS float64 `json:"max_ms"`
	AvgMS float64 `json:"avg_ms"`

	P50MS float64 `json:"p50_ms"`
	P90MS float64 `json:"p90_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`

	// PercentilesApprox 标记分位数是近似值。
	//
	// 永远为 true（除非样本极少且恰好精确）。
	// 显式标记而不是让读者猜：把近似值当精确值用会得出错误结论。
	PercentilesApprox bool `json:"percentiles_approx"`
}

// Snapshot 生成可序列化的分位数快照。
func (h *LatencyHistogram) Snapshot() Histogram {
	out := Histogram{PercentilesApprox: true}
	if h == nil || h.Count == 0 {
		return out
	}

	out.Count = h.Count
	out.MinMS = round(h.Min, 3)
	out.MaxMS = round(h.Max, 3)
	out.AvgMS = round(h.Mean(), 3)

	if v, ok := h.Quantile(0.50); ok {
		out.P50MS = round(v, 3)
	}
	if v, ok := h.Quantile(0.90); ok {
		out.P90MS = round(v, 3)
	}
	if v, ok := h.Quantile(0.95); ok {
		out.P95MS = round(v, 3)
	}
	if v, ok := h.Quantile(0.99); ok {
		out.P99MS = round(v, 3)
	}
	return out
}

// round 保留 n 位小数。
func round(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

// ---------------------------------------------------------------------------
// 错误分类计数
// ---------------------------------------------------------------------------

// ErrorCounts 是失败分类的计数。
type ErrorCounts map[string]int

// Add 累加。
func (e ErrorCounts) Add(kind string) {
	if e == nil || kind == "" {
		return
	}
	e[kind]++
}

// Sorted 返回按数量降序排列的分类。
//
// 排序是为了输出稳定：map 遍历顺序随机，不排序会让两次运行
// 的输出不同，无法比对。
func (e ErrorCounts) Sorted() []ErrorCount {
	out := make([]ErrorCount, 0, len(e))
	for kind, count := range e {
		out = append(out, ErrorCount{Type: kind, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// ErrorCount 是单个分类的计数。
type ErrorCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// ---------------------------------------------------------------------------
// 分组键
// ---------------------------------------------------------------------------

// GroupKey 是"目标 × 地区 × 运营商"的聚合键（需求第 3、21 条）。
//
// 为什么必须包含地区与运营商：同一个 IP:Port 从Sample Province移动和从德国
// 电信看过去是完全不同的线路。"1.1.1.1:443 的延迟是多少"这个问题
// 没有唯一答案，只有"从某地某运营商看是多少"。
type GroupKey struct {
	TargetID string

	Country  string
	Province string
	City     string
	ISP      string
	ASN      string
}

// String 返回稳定的分组标识。
//
// 用固定分隔符与固定字段顺序：相同的分组在任何时间、任何节点
// 都生成完全一致的键，便于比对与去重。
func (k GroupKey) String() string {
	return strings.Join([]string{
		k.TargetID, k.Country, k.Province, k.City, k.ISP, k.ASN,
	}, "|")
}

// TargetKeyOnly 返回只按目标聚合的键（用于跨节点对比）。
func (k GroupKey) TargetKeyOnly() string { return k.TargetID }

// describe 返回人类可读的分组描述。
func (k GroupKey) describe() string {
	parts := make([]string, 0, 5)
	for _, value := range []string{k.Country, k.Province, k.City, k.ISP, k.ASN} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "(unknown region)"
	}
	return strings.Join(parts, "/")
}

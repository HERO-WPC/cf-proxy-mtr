// Package export 把本地数据转换成**可公开的** JSONL。
//
// 这一层是整个项目里唯一允许产生对外数据的地方，因此它承担三件事：
//
//  1. **定型公开 Schema**：字段名、类型、缺失值表达方式一经确定，
//     下游（聚合、网站、第三方分析）就依赖它。schema_version 写在
//     每一行上，而不是只在文件头——文件可能被拆开、被打乱、被抽样。
//  2. **应用隐私过滤**：本地库保留内网地址供用户诊断，
//     导出时必须过滤（见 internal/privacy）。这是"最后一道闸门"：
//     过了这里就是公开数据，不再有机会补救。
//  3. **流式输出**：库可能有几十万行，导出不能把全部结果先读进内存。
//
// 明确**不做**的事：本包不做上传、不做聚合、不做打分。
// 上传（Phase 10）只负责把这里的产物搬走，不改变它的内容。
package export

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/privacy"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// Kind 区分导出行的种类。
//
// 两种行放在同一个 JSONL 流里（用 kind 区分）而不是两个文件：
// 上传的单位是一次会话，拆成多个文件会让"这批数据是否完整"
// 变成多文件一致性问题。
type Kind string

const (
	// KindMeasurement 是 TCP 测量结果。
	KindMeasurement Kind = "measurement"

	// KindTrace 是线路跟踪结果。
	KindTrace Kind = "trace"
)

// Row 是公开 JSONL 的一行。
//
// 字段顺序固定（Go 结构体字段顺序即 JSON 顺序），
// 便于人眼比对 diff。
type Row struct {
	// SchemaVersion 是公开 Schema 的版本。
	//
	// 每一行都带：文件可能被拆分、抽样、打乱，行级版本号
	// 是唯一可靠的自描述方式。
	SchemaVersion int `json:"schema_version"`

	// Kind 是行种类（measurement / trace）。
	Kind Kind `json:"kind"`

	// ClientVersion 是产生这条数据的程序版本。
	//
	// 必须保留：测量逻辑会演进，分析时需要能把样本按版本区分，
	// 否则不同口径的数据会被混在一起比较。
	ClientVersion string `json:"client_version"`

	// TargetID / IP / Port 是被测目标。
	//
	// 目标是公开数据（来自公开的 all.json），因此原样保留。
	TargetID string `json:"target_id"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`

	// TimestampUTC 是测量/跟踪发生的时间（RFC3339，UTC）。
	TimestampUTC string `json:"timestamp_utc"`

	// SessionID 是产生这条数据的测量会话。
	//
	// 保留它是为了"同一批扫描"能被识别出来：跨会话的样本
	// 时间跨度可能很大，混在一起算中位数会失真。
	SessionID string `json:"session_id,omitempty"`

	// CollectorID 是匿名采集者标识（随机生成，非硬件指纹）。
	//
	// 必须保留：没有它就无法回答"不同节点看到的线路是否不同"，
	// 而那是本项目的核心问题。它不含任何可定位到个人的信息。
	CollectorID string `json:"collector_id"`

	// Collector 是采集者的地区/运营商分组信息。
	Collector *Region `json:"collector,omitempty"`

	// TargetMeta 是目标的上游元数据（公开信息）。
	TargetMeta *TargetMeta `json:"target_meta,omitempty"`

	// Measurement 仅在 kind=measurement 时存在。
	Measurement *Measurement `json:"measurement,omitempty"`

	// Trace 仅在 kind=trace 时存在。
	Trace *Trace `json:"trace,omitempty"`
}

// Region 是地区/运营商分组信息（采集者与目标共用同一形状）。
type Region struct {
	Country  string `json:"country,omitempty"`
	Province string `json:"province,omitempty"`
	City     string `json:"city,omitempty"`
	ISP      string `json:"isp,omitempty"`
	ASN      string `json:"asn,omitempty"`

	// IPVersion 是出口 IP 版本（ipv4 / ipv6）。
	IPVersion string `json:"ip_version,omitempty"`
}

// TargetMeta 是目标的上游元数据。
//
// 这些字段来自公开的 all.json，本来就公开，因此原样带出。
// 注意**没有** ASN / ISP：上游的目标元数据里不存在这两个字段，
// 不在这里凭空生成（那会变成猜测，而不是测量）。
type TargetMeta struct {
	Country string `json:"country,omitempty"`
	CCA2    string `json:"cca2,omitempty"`
	Region  string `json:"region,omitempty"`
	City    string `json:"city,omitempty"`

	// CountryEN 仅供展示，不参与聚合判断。
	CountryEN string `json:"country_en,omitempty"`

	// Latitude / Longitude 缺失时**不出现**（omitempty 对指针有效）。
	//
	// 0,0 是合法坐标，因此不能用 0 表示"没有坐标"。
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`

	// Colo 是 Cloudflare 接入点信息。
	Colo *Colo `json:"colo,omitempty"`
}

// Colo 是 Cloudflare 接入点信息。
type Colo struct {
	IATA      string   `json:"iata,omitempty"`
	CCA2      string   `json:"cca2,omitempty"`
	City      string   `json:"city,omitempty"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
}

// Measurement 是 TCP 测量结果。
type Measurement struct {
	Success bool `json:"success"`

	// LatencyMS 只在成功时有意义。
	//
	// 失败时仍然保留"等待了多久"，但语义是"失败发生前等了多久"，
	// 因此用 error_type 区分，不要把失败样本算进延迟统计。
	LatencyMS float64 `json:"latency_ms"`

	// ErrorType 是失败分类；成功时省略。
	//
	// 分类必须保留（需求第 65 条）：超时 / 连接被拒 / 网络不可达
	// 是不同的线路现象，合并成一个"失败"会让数据失去价值。
	ErrorType string `json:"error_type,omitempty"`

	// ErrorMessage 是人类可读的简短原因。
	//
	// 它会经过清洗：去掉可能包含本机地址、路径、用户名的部分。
	ErrorMessage string `json:"error_message,omitempty"`
}

// Trace 是线路跟踪结果。
type Trace struct {
	Success bool `json:"success"`

	Engine        string `json:"engine,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Protocol      string `json:"protocol,omitempty"`

	DurationMS float64 `json:"duration_ms"`

	// HopCount 是路径总跳数（含未响应的跳）。
	HopCount int `json:"hop_count"`

	// RespondedHops 是有回复的跳数。
	//
	// 与 HopCount 分开：HopCount 大而 RespondedHops 小，
	// 说明路径上有大量不回 ICMP 的路由器，而不是路径很长。
	RespondedHops int `json:"responded_hops"`

	// Hops 是路径（内网地址已被替换成占位符）。
	Hops []Hop `json:"hops"`

	// LocalFiltered 表示这条记录经过了本地地址过滤。
	//
	// 显式标记而不是静默替换：分析者需要知道
	// "private-v4" 是我们替换的，而不是真的有个主机叫这个名字。
	LocalFiltered bool `json:"local_filtered"`

	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// Hop 是路径上的一跳。
type Hop struct {
	TTL int `json:"ttl"`

	// IP 是回复地址；内网地址已被替换为 "private-v4" / "private-v6"。
	//
	// 超时跳没有 IP，字段被省略。
	IP string `json:"ip,omitempty"`

	Hostname string `json:"hostname,omitempty"`

	// RTTMS 是往返时延样本（毫秒）。
	//
	// 保留全部样本而不是只留最小值：抖动（jitter）是线路质量的重要
	// 维度，只留最小值会把抖动信息全部丢掉。
	RTTMS []float64 `json:"rtt_ms,omitempty"`

	// Timeout 表示这一跳没有任何回复。
	Timeout bool `json:"timeout,omitempty"`

	ASN            string `json:"asn,omitempty"`
	ASOrganization string `json:"as_organization,omitempty"`
	Country        string `json:"country,omitempty"`
	Province       string `json:"province,omitempty"`
	City           string `json:"city,omitempty"`
}

// ---------------------------------------------------------------------------
// 写出
// ---------------------------------------------------------------------------

// Writer 把 Row 写成 JSONL。
//
// 刻意不缓冲：调用方（export 命令）负责套一层 bufio，
// 这样"压缩"与"缓冲"是两层独立的东西，可以各自替换。
type Writer struct {
	w       io.Writer
	written int
}

// NewWriter 创建 JSONL 写出器。
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write 写出一行。
//
// 用 json.Encoder 而不是 Marshal + Write：前者直接流式写到
// writer，不需要为每一行分配一个完整字节切片；对几十万行来说
// 这个差别是数量级的。
func (w *Writer) Write(row *Row) error {
	if row == nil {
		return nil
	}
	encoder := json.NewEncoder(w.w)

	// 关掉 HTML 转义：默认会把 & < > 变成 \u0026 等，
	// 让输出的可读性变差（运营商名里出现 & 很常见）。
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(row); err != nil {
		return fmt.Errorf("encode row: %w", err)
	}
	w.written++
	return nil
}

// Written 返回已写出的行数。
func (w *Writer) Written() int { return w.written }

// ---------------------------------------------------------------------------
// 行构造
// ---------------------------------------------------------------------------

// baseRow 是所有行的共同部分。
func baseRow(kind Kind, clientVersion string) Row {
	if clientVersion == "" {
		clientVersion = version.Version
	}
	return Row{
		SchemaVersion: version.SchemaVersion,
		Kind:          kind,
		ClientVersion: clientVersion,
	}
}

// timestampUTC 把时间格式化成 RFC3339（纳秒精度，UTC）。
//
// 用 UTC 而不是本地时间：公开数据跨时区，本地时间会让
// "什么时候变差"无法对齐。
func timestampUTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ---------------------------------------------------------------------------
// 隐私过滤的统计
// ---------------------------------------------------------------------------

// FilterStats 记录过滤过程中发生了什么。
//
// 存在的意义是**可核对**：导出报告里必须能看出
// "多少行因为隐私被丢掉了"，否则用户无法判断导出是否完整。
// 一个静默丢弃数据的导出是不可信的。
type FilterStats struct {
	// MeasurementsTotal / TracesTotal 是输入的原始行数。
	MeasurementsTotal int
	TracesTotal       int

	// MeasurementsExported / TracesExported 是实际写出的行数。
	MeasurementsExported int
	TracesExported       int

	// SkippedPrivateTarget 是因为目标本身是内网/保留地址而被丢弃的行数。
	SkippedPrivateTarget int

	// SkippedInvalidTarget 是因为目标地址无法解析而被丢弃的行数。
	SkippedInvalidTarget int

	// RedactedHops 是被替换成占位符的跳数。
	RedactedHops int

	// DroppedHops 是被整体丢弃的跳数（目前不丢，保留字段以便将来
	// 如果改成"直接删掉内网跳"时统计能跟上）。
	DroppedHops int

	// SanitizedMessages 是被清洗过的错误信息条数。
	SanitizedMessages int
}

// Add 合并另一份统计。
func (s *FilterStats) Add(other FilterStats) {
	s.MeasurementsTotal += other.MeasurementsTotal
	s.TracesTotal += other.TracesTotal
	s.MeasurementsExported += other.MeasurementsExported
	s.TracesExported += other.TracesExported
	s.SkippedPrivateTarget += other.SkippedPrivateTarget
	s.SkippedInvalidTarget += other.SkippedInvalidTarget
	s.RedactedHops += other.RedactedHops
	s.DroppedHops += other.DroppedHops
	s.SanitizedMessages += other.SanitizedMessages
}

// RowsExported 返回写出的总行数。
func (s FilterStats) RowsExported() int {
	return s.MeasurementsExported + s.TracesExported
}

// RowsSkipped 返回因为隐私原因被丢弃的总行数。
func (s FilterStats) RowsSkipped() int {
	return s.SkippedPrivateTarget + s.SkippedInvalidTarget
}

// PrivacyNote 返回一句话总结隐私过滤的结果，供导出报告展示。
func (s FilterStats) PrivacyNote() string {
	if s.RowsSkipped() == 0 && s.RedactedHops == 0 {
		return "no private data found (nothing was filtered or redacted)"
	}
	parts := make([]string, 0, 3)
	if s.SkippedPrivateTarget > 0 {
		parts = append(parts, fmt.Sprintf("%d row(s) with private/reserved target IP dropped", s.SkippedPrivateTarget))
	}
	if s.SkippedInvalidTarget > 0 {
		parts = append(parts, fmt.Sprintf("%d row(s) with unparseable target IP dropped", s.SkippedInvalidTarget))
	}
	if s.RedactedHops > 0 {
		parts = append(parts, fmt.Sprintf("%d private hop address(es) replaced with %s/%s",
			s.RedactedHops, privacy.RedactedIPv4, privacy.RedactedIPv6))
	}
	return joinWithSemicolon(parts)
}

// joinWithSemicolon 拼接说明片段。
func joinWithSemicolon(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += "; "
		}
		out += part
	}
	return out
}

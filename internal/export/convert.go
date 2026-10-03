package export

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/privacy"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// 本文件把 storage 的行转换成公开 Row，并在转换过程中应用隐私过滤。
//
// 关键设计：**过滤发生在转换时，而不是转换后**。
// 转换后过滤意味着"已经构造出了含内网地址的公开结构"，
// 任何一条遗漏返回它的代码路径都是泄露。在转换时过滤则
// 只有一条路径能产生 Row，且这条路径必然经过 Redact。

// MeasurementRow 把一条测量转换公开行。
//
// 返回 (nil, false) 表示该行因隐私原因**不应被导出**，
// 调用方应当计入 FilterStats 而不是静默跳过。
func MeasurementRow(item storage.ExportMeasurement, clientVersion string) (*Row, FilterStats, bool) {
	var stats FilterStats
	stats.MeasurementsTotal = 1

	// 目标地址必须可公开。私有地址的目标是用户自己的测试数据，
	// 不属于公开数据库。
	switch privacy.ClassifyTarget(item.IP) {
	case privacy.TargetPrivate:
		stats.SkippedPrivateTarget = 1
		return nil, stats, false
	case privacy.TargetInvalid:
		stats.SkippedInvalidTarget = 1
		return nil, stats, false
	}

	row := baseRow(KindMeasurement, clientVersion)
	row.TargetID = item.TargetID
	row.IP = item.IP
	row.Port = item.Port
	row.TimestampUTC = timestampUTC(item.Timestamp)
	row.SessionID = item.SessionID
	row.CollectorID = item.CollectorAID

	region := regionOf(item.CollectorCountry, item.CollectorProvince, item.CollectorCity,
		item.CollectorISP, item.CollectorASN, item.CollectorIPVersion)
	if region != nil {
		row.Collector = region
	}
	row.TargetMeta = targetMeta(item)

	measurement := &Measurement{
		Success:   item.Success,
		LatencyMS: item.LatencyMS,
	}
	if !item.Success {
		measurement.ErrorType = item.ErrorType
		message, sanitized := sanitizeErrorMessage(item.ErrorMessage)
		measurement.ErrorMessage = message
		if sanitized {
			stats.SanitizedMessages = 1
		}
	}
	row.Measurement = measurement

	stats.MeasurementsExported = 1
	return &row, stats, true
}

// regionOf 构造地区/运营商信息；全部字段为空时返回 nil。
//
// 为什么返回 nil 而不是空结构：JSON 里一个 "collector": {} 是纯噪声
// （所有字段都是 omitempty，空结构会序列化成空对象）。
// 采集者画像没跑过 detect 时就是这个状态，此时**省略**比
// 给一个空对象更诚实——"没有地区信息"和"地区信息是空的"是一回事，
// 而空对象看起来像"我们收集了但值是空"。
func regionOf(country, province, city, isp, asn, ipVersion string) *Region {
	region := Region{
		Country:   country,
		Province:  province,
		City:      city,
		ISP:       isp,
		ASN:       asn,
		IPVersion: ipVersion,
	}
	if region == (Region{}) {
		return nil
	}
	return &region
}

// targetMeta 构造目标元数据（采集者与跟踪共用同一形状）。
func targetMeta(item storage.ExportMeasurement) *TargetMeta {
	meta := &TargetMeta{
		Country:   item.Country,
		CCA2:      item.CCA2,
		Region:    item.Region,
		City:      item.City,
		CountryEN: item.CountryEN,
		Latitude:  item.Latitude,
		Longitude: item.Longitude,
	}
	if item.ColoIATA != "" || item.ColoCCA2 != "" || item.ColoCity != "" ||
		item.ColoLatitude != nil || item.ColoLongitude != nil {
		meta.Colo = &Colo{
			IATA:      item.ColoIATA,
			CCA2:      item.ColoCCA2,
			City:      item.ColoCity,
			Latitude:  item.ColoLatitude,
			Longitude: item.ColoLongitude,
		}
	}
	return meta
}

// TraceRow 把一条跟踪转换公开行。
//
// 与测量不同，跟踪的过滤更细：目标地址不可公开时整行丢弃，
// 但路径上的内网跳只做**替换**而不是丢弃——跳的位置与顺序
// 本身就是线路信息（"第 3 跳还在内网"说明流量尚未出局域网）。
func TraceRow(item storage.ExportTrace, clientVersion string) (*Row, FilterStats, bool) {
	var stats FilterStats
	stats.TracesTotal = 1

	switch privacy.ClassifyTarget(item.IP) {
	case privacy.TargetPrivate:
		stats.SkippedPrivateTarget = 1
		return nil, stats, false
	case privacy.TargetInvalid:
		stats.SkippedInvalidTarget = 1
		return nil, stats, false
	}

	row := baseRow(KindTrace, clientVersion)
	row.TargetID = item.TargetID
	row.IP = item.IP
	row.Port = item.Port
	row.TimestampUTC = timestampUTC(item.Timestamp)
	row.SessionID = item.SessionID
	row.CollectorID = item.CollectorAID

	region := regionOf(item.CollectorCountry, item.CollectorProvince, item.CollectorCity,
		item.CollectorISP, item.CollectorASN, item.CollectorIPVersion)
	if region != nil {
		row.Collector = region
	}

	// TargetMeta 与测量共用结构，因此这里构造一个临时测量行来复用逻辑。
	row.TargetMeta = targetMeta(storage.ExportMeasurement{
		Country: item.Country, CCA2: item.CCA2, Region: item.Region, City: item.City,
		CountryEN: item.CountryEN,
		Latitude:  item.Latitude, Longitude: item.Longitude,
		ColoIATA: item.ColoIATA, ColoCCA2: item.ColoCCA2, ColoCity: item.ColoCity,
		ColoLatitude: item.ColoLatitude, ColoLongitude: item.ColoLongitude,
	})

	trace := &Trace{
		Success:       item.Success,
		Engine:        item.Engine,
		EngineVersion: item.EngineVersion,
		Mode:          item.Mode,
		Protocol:      item.Protocol,
		DurationMS:    item.DurationMS,
		HopCount:      item.HopCount,
		LocalFiltered: true, // 下面必然经过 Redact
	}

	// 轨迹里也存在内网地址，同样必须过滤。
	hops, redacted, err := decodeHops(item.TraceJSON)
	if err != nil {
		// 解析失败不是隐私问题，而是数据问题：这一行的路径不可用，
		// 但测量本身（成功/失败、耗时）仍然有效，因此保留行、
		// 只把路径置空，并如实记下原因。
		trace.ErrorType = firstNonEmpty(trace.ErrorType, "trace_json_unparsable")
		hops = nil
	}
	trace.Hops = hops
	stats.RedactedHops = redacted

	// 统计有回复的跳数（超时跳不算）。
	for _, hop := range hops {
		if !hop.Timeout && hop.IP != "" {
			trace.RespondedHops++
		}
	}

	if !item.Success {
		trace.ErrorType = item.ErrorType
		message, sanitized := sanitizeErrorMessage(item.ErrorMessage)
		trace.ErrorMessage = message
		if sanitized {
			stats.SanitizedMessages++
		}
	}

	row.Trace = trace

	stats.TracesExported = 1
	return &row, stats, true
}

// internalHop 是数据库里 trace_json 的形状（与 internal/trace.Hop 对应）。
//
// 刻意在本包重新定义而不是直接 import internal/trace：
// 公开 Schema 的字段名必须由本包决定，不能被内部结构的演进牵动。
// 这里显式声明"我们从库里读哪些字段、公开成什么"，两层各自可以独立变化。
type internalHop struct {
	TTL            int       `json:"TTL"`
	IP             string    `json:"IP"`
	Hostname       string    `json:"Hostname"`
	RTTMS          []float64 `json:"RTTMS"`
	Timeout        bool      `json:"Timeout"`
	ASN            string    `json:"ASN"`
	ASOrganization string    `json:"ASOrganization"`
	Country        string    `json:"Country"`
	Province       string    `json:"Province"`
	City           string    `json:"City"`
}

// decodeHops 解析数据库里的跳列表并做隐私过滤。
//
// 返回 (跳列表, 被替换的跳数, 错误)。
func decodeHops(traceJSON string) ([]Hop, int, error) {
	trimmed := strings.TrimSpace(traceJSON)
	if trimmed == "" {
		// 没有路径不是错误：失败的跟踪、或者只记了跳数的情况。
		return nil, 0, nil
	}

	var internal []internalHop
	if err := json.Unmarshal([]byte(trimmed), &internal); err != nil {
		return nil, 0, fmt.Errorf("decode hops: %w", err)
	}

	hops := make([]Hop, 0, len(internal))
	redacted := 0
	for _, h := range internal {
		hop := Hop{
			TTL:            h.TTL,
			Hostname:       h.Hostname,
			RTTMS:          h.RTTMS,
			Timeout:        h.Timeout,
			ASN:            h.ASN,
			ASOrganization: h.ASOrganization,
			Country:        h.Country,
			Province:       h.Province,
			City:           h.City,
		}

		// 内网地址替换成占位符，而不是删掉这一跳。
		if h.IP != "" {
			redactedIP := privacy.Redact(h.IP)
			if redactedIP != h.IP {
				redacted++
			}
			hop.IP = redactedIP
		}

		hops = append(hops, hop)
	}
	return hops, redacted, nil
}

// ---------------------------------------------------------------------------
// 错误信息清洗
// ---------------------------------------------------------------------------

// 错误信息来自操作系统与第三方引擎，可能包含本机地址、文件路径、
// 用户名。公开之前必须把这类内容去掉。
//
// 策略是"按模式替换"而不是"整条丢弃"：错误信息对分析失败原因
// 有价值（"connection refused" vs "i/o timeout"），
// 整条丢掉会让公开数据没法解释失败。
var (
	// IPv4 地址。
	ipv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

	// IPv6 **候选**：一串十六进制与冒号，且至少两个冒号。
	//
	// 刻意不写成完整的 IPv6 正则：IPv6 的合法写法极多，
	// 手写正则几乎必然写漏（第一版就漏掉了 "2001:db8::1" 这种
	// 最常见的压缩形式，导致地址留在公开数据里）。
	//
	// 这里的做法是"宽匹配 + 严格校验"：先用宽松模式取出候选
	// （覆盖 ::、::1、%zone），再用 net.ParseIP 判定它到底是不是地址。
	// **由解析器决定，而不是由模式决定。**
	//
	// 注意 Go 的 regexp 不支持负向前瞻（RE2），因此这里不加
	// "(?![0-9.])" 之类的断言——纯 IPv4 已经在调用方先行替换掉了，
	// 剩下的误匹配由 net.ParseIP 挡掉。
	//
	// 第一条分支以 \b 开头（绝不能是 \b::，因为冒号不是"词字符"，
	// 它前面构不成词边界——第一版正是因此漏掉了 "::1"）；
	// 后两条覆盖以 :: 开头的压缩写法及其结尾边界。
	ipv6Candidate = regexp.MustCompile(
		`(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){2,7}(?:%[0-9a-z]+)?|` +
			`::[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){0,6}(?:%[0-9a-z]+)?|` +
			`::[0-9a-f]{0,4}(?:%[0-9a-z]+)?`)

	// 类 Unix 与 Windows 的绝对路径。
	pathPattern = regexp.MustCompile(
		`(?:[A-Za-z]:\\[^\s"']+)|(?:/(?:Users|home|root|tmp|var|opt|mnt)/[^\s"']+)`)

	// Windows 用户名形式的环境变量与常见用户名段。
	userSegmentPattern = regexp.MustCompile(`\bUsers\\[^\s\\"']+`)
)

// sanitizeErrorMessage 清洗错误信息，返回清洗后的文本与是否发生了修改。
//
// 同时做长度截断：错误信息可能非常长（整个 usage 输出），
// 而没有长度上限的公开字段是滥用面。
func sanitizeErrorMessage(message string) (string, bool) {
	original := message
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return "", false
	}

	// 顺序有讲究：先替换路径与用户名，再替换地址。
	// 路径里可能含地址（"C:\logs\10.0.0.1.txt"），先处理路径能把
	// 它整体吃掉，避免留下半截路径或半截地址。
	cleaned := pathPattern.ReplaceAllString(trimmed, "[path]")
	cleaned = userSegmentPattern.ReplaceAllString(cleaned, `Users\[user]`)
	cleaned = redactAddresses(cleaned)

	// 去掉控制字符（含 ANSI 颜色码的 ESC），它们会让 JSONL 难以处理。
	cleaned = stripControlChars(cleaned)
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	const maxLength = 300
	if len(cleaned) > maxLength {
		cleaned = cleaned[:maxLength] + "..."
	}

	return cleaned, cleaned != original
}

// redactAddresses 把消息里的 IP 地址替换成 [addr]。
//
// 与"一条大正则"相比，这里对每个候选 token 调用 net.ParseIP 判定。
// 这样做的理由是实测出来的：
//
//   - 纯模式匹配会**漏**掉 "::1"、"2001:db8::1" 这类压缩写法
//     （第一版就是这样漏的），地址直接留在了公开数据里；
//   - 放宽模式又会**误伤** "12:30:45"、"00:00:01" 这类时间字符串，
//     把诊断信息换成占位符，比不过滤更糟。
//
// 换成"由解析器判定"之后，两种情况都自动正确：net.ParseIP 明确
// 拒绝 "12:30:45"（段数不对），也明确接受 "::1"。
func redactAddresses(s string) string {
	if strings.Contains(s, ".") {
		s = ipv4Pattern.ReplaceAllString(s, "[addr]")
	}
	// 冒号少于两个时不可能是 IPv6（更可能是时间或端口），直接跳过，
	// 省掉一次正则扫描。
	if strings.Count(s, ":") < 2 {
		return s
	}

	return ipv6Candidate.ReplaceAllStringFunc(s, func(token string) string {
		// %zone 是接口名（fe80::1%eth0），net.ParseIP 不认，先去掉。
		host := token
		if index := strings.IndexByte(host, '%'); index >= 0 {
			host = host[:index]
		}
		if net.ParseIP(host) == nil {
			return token // 不是地址（例如时间），保持原样
		}
		return "[addr]"
	})
}

// stripControlChars 去掉 ASCII 控制字符。
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

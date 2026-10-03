package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// NextTrace JSON 的解析层。
//
// 这一层的存在理由（需求第 29、30 条）：
//
//	NextTrace JSON -> 本文件 -> TraceResult -> SQLite
//
// 上层与数据库都不直接依赖 NextTrace 的字段名。上游改格式时
// 只需要改这里，而不是迁移数据库。
//
// ===========================================================================
// 真实输出结构（NextTrace v1.7.3，2026-08 实测）
// ===========================================================================
//
//	{
//	  "Hops": [                      <- 注意大写 H
//	    [                            <- 每个 TTL 一个**数组**，含多次探测
//	      {
//	        "Success": true,
//	        "Address": {"IP": "203.0.113.4", "Zone": ""},
//	        "Hostname": "",
//	        "TTL": 4,
//	        "RTT": 4996800,          <- 纳秒！不是毫秒
//	        "Error": null,
//	        "Geo": {                 <- 可为 null
//	          "ip": "", "asnumber": "64500",
//	          "country": "中国", "country_en": "China",
//	          "prov": "示例省", "prov_en": "Zhejiang",
//	          "city": "示例市", "city_en": "Hangzhou",
//	          "owner": "example.net ", "isp": "移动",
//	          "whois": "RFC1918", "lat": 30.29, "lng": 120.16,
//	          "prefix": "", "router": {}, "source": ""
//	        },
//	        "Lang": "cn", "MPLS": null
//	      },
//	      {...}, {...}               <- 同一 TTL 的另外两次探测
//	    ]
//	  ],
//	  "StopReason": {"hop": 30, "reason": "max_hops"},
//	  "TraceMapUrl": "https://..."
//	}
//
// 三个必须特别注意的点（凭猜一定会错）：
//
//  1. **RTT 单位是纳秒**。4996800 表示 4.9968 ms，不是 4996.8 ms。
//     直接当毫秒用会把延迟放大 100 万倍，而"数字看起来很大"
//     很容易被当成"网络很差"而不是"单位错了"。
//  2. **Hops 是二维数组**：外层是 TTL，内层是同一 TTL 的多次探测。
//     同一跳的多个 RTT 必须聚合成一跳，而不是把 3 次探测算成 3 跳。
//  3. **Geo 只有 ..._en 字段是干净文本**。country / prov / city
//     是本地化（中文）文本，而**上游给的是乱码**（GBK 被当 UTF-8 解），
//     因此优先采用 country_en / prov_en / city_en。
//     这与 all.json 里 country_cn 乱码是同一类上游问题。

// 解析层参数。
const (
	// rttNanosecondsPerMillisecond 是 RTT 单位换算（纳秒 -> 毫秒）。
	rttNanosecondsPerMillisecond = 1e6

	// maxLookupDepth 是递归查找字段的最大深度。
	maxLookupDepth = 4
)

// ParsedTrace 是归一化后的解析结果。
//
// 导出是因为调用方（CLI、诊断工具）需要 StopReason 与 TraceMapURL：
// 前者区分"到达目标"与"跳数用尽"，后者便于人工核对路径。
type ParsedTrace struct {
	// Hops 是归一化后的跳列表（每个 TTL 一跳，按 TTL 升序）。
	Hops []Hop

	// Protocol 是引擎报告的协议（NextTrace 不总是提供，可能为空）。
	Protocol string

	// TargetIP 是引擎确认的目标地址，可能为空。
	TargetIP string

	// StopReason 是引擎停止的原因（例如 "max_hops"、"arrival"）。
	//
	// 它有价值：区分"到了目标"与"跳数用尽"对判断路径是否完整
	// 很重要，因此保存下来供诊断。
	StopReason string

	// StopHop 是引擎停止时所处的跳数（0 表示未提供）。
	StopHop int

	// TraceMapURL 是引擎提供的可视化链接（可能为空）。
	TraceMapURL string
}

// ParseNextTraceJSON 解析 nexttrace --json 的输出。
//
// 解析失败返回 error；解析成功但跳列表为空时返回**空跳列表**而不是错误——
// "跟踪到了 0 跳"是真实结果，由调用方决定怎么处理。
func ParseNextTraceJSON(data []byte) (*ParsedTrace, error) {
	data = trimJSONNoise(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("empty engine output")
	}

	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("engine output is not a json object: %w", err)
	}

	out := &ParsedTrace{
		Protocol: lookupString(document, "protocol", 0),
		TargetIP: firstNonEmpty(
			lookupString(document, "dest_ip", 0),
			lookupString(document, "destIp", 0),
		),
		TraceMapURL: lookupString(document, "TraceMapUrl", 0),
	}

	// StopReason 是嵌套对象 {"hop": 30, "reason": "max_hops"}，
	// 因此要取它的子字段而不是对象本身。
	// 它区分"到达目标"与"跳数用尽"，对判断路径是否完整有价值。
	if stop := document["StopReason"]; stop != nil {
		if stopMap, ok := stop.(map[string]any); ok {
			out.StopReason = firstNonEmpty(
				lookupString(stopMap, "reason", 0),
				lookupString(stopMap, "Reason", 0),
			)
			out.StopHop = lookupInt(stopMap, "hop", 0)
		} else if text, ok := scalarToString(stop); ok {
			// 容忍将来把它改成字符串。
			out.StopReason = strings.TrimSpace(text)
		}
	}

	rawHops, found := findHopsArray(document)
	if !found {
		return nil, fmt.Errorf("engine output has no hop list")
	}

	out.Hops = parseHops(rawHops)
	return out, nil
}

// findHopsArray 定位跳列表。
//
// 兼容大写与小写（真实版本用 "Hops"，不排除将来改成 "hops"）。
func findHopsArray(document map[string]any) ([]any, bool) {
	if document == nil {
		return nil, false
	}
	for _, key := range []string{"Hops", "hops", "Hop", "hop"} {
		value, ok := document[key]
		if !ok {
			continue
		}
		if array, ok := value.([]any); ok {
			return array, true
		}
	}
	return nil, false
}

// parseHops 把"每个 TTL 一组探测"的结构聚合成"每个 TTL 一跳"。
//
// 输入形态（真实）：
//
//	[
//	  [ {TTL:1, RTT:..., Address:{...}, Geo:{...}}, {...}, {...} ],   <- TTL 1
//	  [ {...}, {...}, {...} ],                                       <- TTL 2
//	]
//
// 也容忍扁平形态（每个元素直接是一个探测对象），
// 因为早期版本/其它模式可能不分组。
func parseHops(raw []any) []Hop {
	// 先按 TTL 归组：同一个 TTL 的所有探测合成一跳。
	grouped := make(map[int]*hopAccumulator)

	for _, item := range raw {
		switch value := item.(type) {
		case []any:
			// 真实形态：一个 TTL 的多次探测。
			for _, probe := range value {
				mergeProbe(grouped, probe)
			}
		case map[string]any:
			// 扁平形态：单个探测对象。
			mergeProbe(grouped, value)
		}
	}

	hops := make([]Hop, 0, len(grouped))
	for _, acc := range grouped {
		hops = append(hops, acc.build())
	}

	// 按 TTL 升序：路径必须按跳序理解。
	sort.SliceStable(hops, func(i, j int) bool { return hops[i].TTL < hops[j].TTL })
	return hops
}

// hopAccumulator 累积同一 TTL 的多次探测。
type hopAccumulator struct {
	ttl      int
	ip       string
	hostname string
	rtts     []float64

	// 元数据只取第一个有值的（同一 TTL 的多次探测通常来自同一路由器）。
	asn      string
	asOrg    string
	country  string
	province string
	city     string

	// noReply 表示所有探测都没有回复。
	noReply bool
}

// mergeProbe 把一个探测对象并入对应的 TTL 组。
func mergeProbe(grouped map[int]*hopAccumulator, item any) {
	probe, ok := item.(map[string]any)
	if !ok {
		return
	}

	ttl := lookupInt(probe, "TTL", 0)
	if ttl <= 0 {
		ttl = lookupInt(probe, "ttl", 0)
	}
	if ttl <= 0 {
		// 没有 TTL 的探测无法定位到路径上的哪一跳，丢弃。
		return
	}

	acc, exists := grouped[ttl]
	if !exists {
		acc = &hopAccumulator{ttl: ttl, noReply: true}
		grouped[ttl] = acc
	}

	// Address 可能是对象 {"IP": "...", "Zone": ""}，也可能是字符串。
	ip := ""
	if address, ok := probe["Address"].(map[string]any); ok {
		ip = firstNonEmpty(
			lookupString(address, "IP", 0),
			lookupString(address, "ip", 0),
		)
	} else if address, ok := probe["Address"].(string); ok {
		ip = strings.TrimSpace(address)
	}
	if ip == "" {
		ip = firstNonEmpty(lookupString(probe, "IP", 0), lookupString(probe, "ip", 0))
	}

	// Success 为 false 表示这次探测没有回复。
	success := true
	if value, ok := probe["Success"].(bool); ok {
		success = value
	}

	if ip != "" {
		acc.noReply = false
		if acc.ip == "" {
			acc.ip = ip
		}
	} else if success {
		// Success 为真但没有地址：数据异常，按"未回复"处理。
		acc.noReply = acc.noReply && true
	}

	// RTT：**纳秒**。只有成功的探测才有意义（失败时是 0）。
	if success {
		if rtt, ok := scalarToFloat(probe["RTT"]); ok && rtt > 0 {
			acc.rtts = append(acc.rtts, rtt/rttNanosecondsPerMillisecond)
		}
	}

	if acc.hostname == "" {
		acc.hostname = lookupString(probe, "Hostname", 0)
	}

	// Geo 可能为 null。
	if geo, ok := probe["Geo"].(map[string]any); ok {
		// 优先 ..._en：本地化字段在上游是乱码（GBK 当 UTF-8 解）。
		if acc.country == "" {
			acc.country = normalizeCountry(firstNonEmpty(
				lookupString(geo, "country_en", 0),
				lookupString(geo, "country", 0),
			))
		}
		if acc.province == "" {
			acc.province = firstNonEmpty(
				lookupString(geo, "prov_en", 0),
				lookupString(geo, "prov", 0),
			)
		}
		if acc.city == "" {
			acc.city = firstNonEmpty(
				lookupString(geo, "city_en", 0),
				lookupString(geo, "city", 0),
			)
		}
		if acc.asOrg == "" {
			// owner 通常是域名形式（example.net），isp 是运营商名（中国移动）。
			// 优先 owner：它是更具体的网络归属。
			acc.asOrg = firstNonEmpty(
				strings.TrimSpace(lookupString(geo, "owner", 0)),
				lookupString(geo, "isp", 0),
			)
		}
		if acc.asn == "" {
			// Geo.asnumber 是纯数字字符串（"64500"），需要补 AS 前缀。
			acc.asn = normalizeASN(firstNonEmpty(
				lookupString(geo, "asnumber", 0),
				lookupString(geo, "as_number", 0),
				lookupString(geo, "asn", 0),
			))
		}
	}

	// 有的模式把 ASN 放在探测对象顶层。
	if acc.asn == "" {
		acc.asn = normalizeASN(firstNonEmpty(
			lookupString(probe, "asnumber", 0),
			lookupString(probe, "asn", 0),
		))
	}
}

// build 生成归一化的一跳。
func (a *hopAccumulator) build() Hop {
	hop := Hop{
		TTL:      a.ttl,
		IP:       a.ip,
		Hostname: a.hostname,
		RTTMS:    a.rtts,

		ASN:            a.asn,
		ASOrganization: a.asOrg,
		Country:        a.country,
		Province:       a.province,
		City:           a.city,
	}

	// 超时的判定：没有任何一次探测拿到地址。
	hop.Timeout = a.noReply || a.ip == ""
	return hop
}

// ---------------------------------------------------------------------------
// 取值辅助
// ---------------------------------------------------------------------------

// lookupString 在文档里按键名找标量字符串（有限深度递归）。
func lookupString(document map[string]any, key string, depth int) string {
	if document == nil || depth > maxLookupDepth {
		return ""
	}
	if value, ok := document[key]; ok {
		if text, ok := scalarToString(value); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// lookupInt 在文档里按键名找整数。
func lookupInt(document map[string]any, key string, depth int) int {
	if document == nil || depth > maxLookupDepth {
		return 0
	}
	if value, ok := document[key]; ok {
		if n, ok := scalarToInt(value); ok {
			return n
		}
	}
	return 0
}

// scalarToString 把 JSON 标量转成字符串。
func scalarToString(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case json.Number:
		return v.String(), true
	default:
		return "", false
	}
}

// scalarToFloat 把 JSON 标量转成 float64。
func scalarToFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// scalarToInt 把 JSON 标量转成 int。
func scalarToInt(value any) (int, bool) {
	f, ok := scalarToFloat(value)
	if !ok {
		return 0, false
	}
	if f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// normalizeASN 把 ASN 归一化成 "AS<数字>"。
//
// NextTrace 的 Geo.asnumber 是纯数字字符串（"64500"），
// 而模型要求 "AS64500"。用 model.NormalizeASN 做统一处理，
// 但额外要求结果确实是 "AS<数字>"，否则返回空——
// 宁可没有 ASN，也不要写入 "AS中国移动" 这种值污染分组。
func normalizeASN(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	asn := model.NormalizeASN(s)
	if !looksLikeASNValue(asn) {
		return ""
	}
	return asn
}

// normalizeCountry 把国家代码归一化成 2 位大写。
//
// 真实数据里 country_en 是 "China"（全称），而不是 "CN"（代码）。
// 我们没有权威的全称->代码映射表，因此：
//   - 2 字母的当作代码，大写化；
//   - 更长的（全称）留空，而不是编一个代码。
//
// 留空是刻意的：编错的国家代码会让聚合分组出现错误的地区维度。
func normalizeCountry(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 2 {
		return strings.ToUpper(s)
	}
	return ""
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// looksLikeASNValue 报告值是否是 "AS<数字>"。
func looksLikeASNValue(s string) bool {
	if len(s) < 3 || !strings.HasPrefix(s, "AS") {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// trimJSONNoise 去掉输出前后的非 JSON 噪声。
//
// NextTrace 有时会在 JSON 之前打印更新提醒或警告行，
// 直接 json.Unmarshal 会因为前导字符失败。
// 这里定位第一个 '{' 并从那里开始解析。
func trimJSONNoise(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return trimmed
	}
	if index := bytes.IndexByte(trimmed, '{'); index >= 0 {
		return bytes.TrimSpace(trimmed[index:])
	}
	return trimmed
}

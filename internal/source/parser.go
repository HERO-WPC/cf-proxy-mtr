package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 原始 JSON 结构（wire types）
// ---------------------------------------------------------------------------
//
// 这些类型只描述我们**读取**的字段，且**不导出**。
// 目的：all.json 的字段一旦变化，只需要改动本文件。
//
// 关键设计：所有标量字段都可能是"数字或字符串"或"缺失"，因此不能用
// 强类型 struct 直接反序列化（类型不符会导致整条记录被丢弃，
// 而需求要求"IP 非法时跳过并记录"，不是"类型不符就整体失败"）。
// 因此这里解码为 map，再逐字段做宽松类型转换。

// rawRoot 是 all.json 的顶层结构。
//
// 真实结构（2026-10 实测）：
//
//	{
//	  "generated_at": "2026-10-03T03:52:35.771190",
//	  "list": { "country": {"US": 1388, ...}, "ips": 11610 },
//	  "data": [ { "ip": "...", "port": [443], "meta": {...} }, ... ]
//	}
//
// 其中 "list" 只是统计摘要，我们保存它作为 source metadata 但不依赖它。
type rawRoot struct {
	GeneratedAt flexTime         `json:"generated_at"`
	List        map[string]any   `json:"list"`
	Data        []map[string]any `json:"data"`
}

// ---------------------------------------------------------------------------
// 宽松类型转换工具
// ---------------------------------------------------------------------------

// asString 把任意 JSON 标量转成字符串。
//
// 数字会以不丢失精度的方式转换（json.Number -> 原始字面量），
// 因此 "lat": 56.946 与 "lat": "56.946" 得到一致结果。
// null、bool、对象、数组返回空字符串。
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return ""
	default:
		return ""
	}
}

// asFloat 把任意 JSON 数值/数字字符串转成 float64。
//
// 第二个返回值表示是否成功。空字符串与 null 视为失败。
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// asInt 把任意 JSON 数值/数字字符串转成 int。
func asInt(v any) (int, bool) {
	f, ok := asFloat(v)
	if !ok {
		return 0, false
	}
	// 端口、ASN 这类字段不应出现小数；出现小数说明数据异常，按失败处理。
	if f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

// getString 从 map 中取字符串字段。
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return asString(m[key])
}

// getMap 从 map 中取子对象字段。
func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

// asStringSlice 把 JSON 字符串数组转成 []string，忽略非字符串元素。
func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// flexTime
// ---------------------------------------------------------------------------

// flexTime 解析 all.json 的 generated_at。
//
// 生产数据的格式是 "2026-10-03T03:52:35.771190"：没有时区后缀。
// 这里把它当作 UTC 处理（而不是本地时区），因为源数据是以 UTC 生成的；
// 这样不同时区的采集者解析出同一个时间点。
//
// 解析失败不返回错误：generated_at 只是元数据，
// 缺失时由调用方回退到抓取时间，不应该导致整个解析失败。
type flexTime struct {
	Time  time.Time
	Valid bool
}

// flexTimeLayouts 是按优先级尝试的布局。
//
// 带时区偏移的布局排在前面，避免把 "2026-10-03T03:52:35+08:00" 误判成 UTC。
var flexTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *flexTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(bytes.Trim(b, `"`)))
	*f = parseFlexTime(s)
	return nil
}

// parseFlexTime 解析时间字符串，失败时返回 Valid=false。
func parseFlexTime(s string) flexTime {
	s = strings.TrimSpace(s)
	if s == "" {
		return flexTime{}
	}
	for _, layout := range flexTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return flexTime{Time: t, Valid: true}
		}
	}
	return flexTime{}
}

// ---------------------------------------------------------------------------
// ParseResult
// ---------------------------------------------------------------------------

// ParseResult 是一次解析的完整结果：目标列表 + 源元数据 + 统计与警告。
//
// 注意 Parse* 函数**不会**因为"个别记录非法"而返回 error：
// 非法记录会被跳过、计数并记录原因，其余记录照常返回。
// 只有"整体不是可识别的结构"（例如没有 data 数组也不是文本列表）
// 才返回 error。
type ParseResult struct {
	// Targets 是去重后的目标列表。
	Targets []model.Target

	// Meta 是数据源级元数据。
	Meta SourceMeta

	// Stats 是解析统计。
	Stats ParseStats

	// Warnings 是人类可读的解析告警（已去重、限量）。
	Warnings []string
}

// SourceMeta 描述数据源本身，作为"保留 source metadata"的载体。
//
// 它只包含来源信息，不含任何采集者信息。
type SourceMeta struct {
	// URL 是实际使用的数据源地址。
	URL string

	// Format 是识别出的格式：json 或 text。
	Format string

	// GeneratorGeneratedAt 是源数据自称的生成时间（可能为零值）。
	GeneratorGeneratedAt time.Time

	// HasGeneratorTime 表示 GeneratorGeneratedAt 是否可用。
	HasGeneratorTime bool

	// ReportedCount 是源数据自称的目标总数（list.ips），0 表示未提供。
	ReportedCount int

	// CountryCounts 是源数据提供的按国家统计（可能为 nil）。
	//
	// 它属于"来源自称的摘要"，我们保存但不作为事实依据：
	// 真实目标数以实际解析结果为准。
	CountryCounts map[string]int
}

// GeneratedAtLocation 返回 generated_at 的时区。
//
// 生产数据不带时区后缀，我们统一按 UTC 解释，
// 因此这里正常情况总是返回 time.UTC。
// 保留该方法是为了让"时区假设"可被测试显式验证，
// 而不是散落在断言里的隐式假设。
func (m SourceMeta) GeneratedAtLocation() *time.Location {
	return m.GeneratorGeneratedAt.Location()
}

// ParseStats 是解析统计，用于"跳过并记录"以及命令行汇报。
//
// 不变式（可被测试验证）：
//
//	RawItems    = TargetCandidates + SkippedItems
//	RawCombos   = len(Targets) + Duplicates + InvalidPorts + Unknown
//	TargetCandidates + Unknown = RawCombos
//	len(Targets) = RawCombos - Duplicates - InvalidPorts - Unknown
type ParseStats struct {
	// RawItems 是源记录条数（JSON 的 data 数组长度，或文本行数）。
	RawItems int

	// TargetCandidates 是成功取出 IP 的记录条数（此时还没展开端口）。
	TargetCandidates int

	// SkippedItems 是因为 ip 字段缺失/非法而整条跳过的记录数。
	SkippedItems int

	// RawCombos 是展开端口后得到的 IP:Port 组合总数（去重前）。
	RawCombos int

	// Duplicates 是因为 (IP, Port) 已出现过而被丢弃的组合数。
	Duplicates int

	// InvalidPorts 是因为端口非法（非整数 / 超出 1-65535）而丢弃的组合数。
	InvalidPorts int

	// Unknown 是因为记录缺少端口信息（无 port 字段或空数组）而无法生成组合的数量。
	Unknown int

	// Reasons 是各类跳过原因的计数，例如 "invalid ip" -> 3。
	Reasons map[string]int
}

// addReason 累加一个跳过原因。
func (s *ParseStats) addReason(format string, args ...any) {
	if s.Reasons == nil {
		s.Reasons = make(map[string]int)
	}
	s.Reasons[fmt.Sprintf(format, args...)]++
}

// ---------------------------------------------------------------------------
// 解析入口
// ---------------------------------------------------------------------------

// maxWarnings 限制单次解析保留的告警条数，避免畸形数据刷屏。
const maxWarnings = 20

// ParseJSON 解析 all.json 内容。
//
// 返回值约定：
//   - 结构无法识别（没有 data 数组且顶层不是对象）-> error；
//   - 其它情况一律返回结果，非法记录进入 Stats/Warnings；
//   - 未知字段被完全忽略，不会导致失败。
func ParseJSON(data []byte) (*ParseResult, error) {
	// 去掉 UTF-8 BOM：ParseJSON 是导出函数，调用方可能直接传入带 BOM 的内容
	// （Windows 上常见），BOM 会让 json 解析在第一字节就失败。
	data = stripBOM(data)

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var root rawRoot
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	res := &ParseResult{
		Meta: SourceMeta{
			Format: "json",
		},
		Stats: ParseStats{
			Reasons: make(map[string]int),
		},
	}

	if root.GeneratedAt.Valid {
		res.Meta.GeneratorGeneratedAt = root.GeneratedAt.Time
		res.Meta.HasGeneratorTime = true
	}
	res.Meta.ReportedCount, res.Meta.CountryCounts = parseSummary(root.List)

	if root.Data == nil {
		return nil, fmt.Errorf("parse json: missing or null \"data\" array")
	}

	dedup := newDeduper()
	warnings := newWarningList(maxWarnings)

	for i, item := range root.Data {
		res.Stats.RawItems++

		ipRaw := getString(item, "ip")
		addr, err := parseHost(ipRaw)
		if err != nil {
			res.Stats.SkippedItems++
			res.Stats.addReason("invalid ip")
			warnings.add("record #%d: invalid ip %q: %v", i, truncate(ipRaw, 64), err)
			continue
		}

		ports, badPorts, ok := normalizePorts(item["port"])
		if !ok {
			res.Stats.Unknown++
			res.Stats.addReason("missing port")
			warnings.add("record #%d (%s): no usable port field", i, ipRaw)
			continue
		}

		res.Stats.TargetCandidates++
		res.Stats.InvalidPorts += badPorts
		if badPorts > 0 {
			res.Stats.addReason("invalid port")
			warnings.add("record #%d (%s): dropped %d invalid port value(s)", i, ipRaw, badPorts)
		}

		loc := parseLocation(getMap(item, "meta"))

		for _, port := range ports {
			res.Stats.RawCombos++
			target, ok := model.NewTarget(addr, port)
			if !ok {
				res.Stats.InvalidPorts++
				res.Stats.addReason("invalid port")
				warnings.add("record #%d (%s): invalid port %d", i, ipRaw, port)
				continue
			}
			target.Location = loc

			if !dedup.add(target) {
				res.Stats.Duplicates++
				res.Stats.addReason("duplicate ip:port")
			}
		}
	}

	res.Targets = dedup.targets
	res.Warnings = warnings.items()
	return res, nil
}

// ParseText 解析 all.txt 备用数据源。
//
// 生产数据的每行格式为：
//
//	101.32.169.108:443#SG
//
// 其中 "#" 之后是国家/地区代码。文本源不提供城市、经纬度、IATA 等信息，
// 因此 Location 只填 CCA2/Country；这不是缺陷，而是源本身的限制，
// 调用方（fetch）在 JSON 源可用时不会使用文本源。
//
// 该函数对格式非常宽容：
//   - 空行、以 "#" 或 ";" 或 "//" 开头的注释行被跳过（不计入 RawItems）；
//   - 缺少 "#CC" 的行仍然有效，只是没有国家信息；
//   - 同一行重复出现时按重复计数。
//
// 返回解析结果与 error。若一行都无法解析且没有任何有效目标，
// 返回 error（避免把"下错文件"当成空结果）。
func ParseText(data []byte) (*ParseResult, error) {
	// 去掉 UTF-8 BOM，否则第一行会变成 "\ufeff1.2.3.4:443" 而解析失败。
	data = stripBOM(data)

	res := &ParseResult{
		Meta:  SourceMeta{Format: "text"},
		Stats: ParseStats{Reasons: make(map[string]int)},
	}

	dedup := newDeduper()
	warnings := newWarningList(maxWarnings)

	lines := bytes.Split(data, []byte("\n"))
	for i, rawLine := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(string(rawLine), "\r"))
		if line == "" {
			continue
		}
		// 注释行：以 # ; // 开头。注意 "#SG" 出现在行尾，不能按位置判断。
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}

		res.Stats.RawItems++

		hostPort, country, _ := strings.Cut(line, "#")
		hostPort = strings.TrimSpace(hostPort)
		country = strings.ToUpper(strings.TrimSpace(country))

		host, portStr, err := splitHostPort(hostPort)
		if err != nil {
			res.Stats.SkippedItems++
			res.Stats.addReason("malformed line")
			warnings.add("line %d: %v", i+1, err)
			continue
		}

		addr, err := parseHost(host)
		if err != nil {
			res.Stats.SkippedItems++
			res.Stats.addReason("invalid ip")
			warnings.add("line %d: invalid ip %q: %v", i+1, truncate(host, 64), err)
			continue
		}

		port, err := strconv.Atoi(portStr)
		if err != nil || !model.ValidPort(port) {
			res.Stats.SkippedItems++
			res.Stats.InvalidPorts++
			res.Stats.addReason("invalid port")
			warnings.add("line %d: invalid port %q", i+1, truncate(portStr, 16))
			continue
		}

		res.Stats.TargetCandidates++
		res.Stats.RawCombos++

		target, ok := model.NewTarget(addr, port)
		if !ok {
			res.Stats.InvalidPorts++
			res.Stats.addReason("invalid port")
			continue
		}
		if country != "" {
			target.Location.CCA2 = country
			target.Location.Country = country
		}

		if !dedup.add(target) {
			res.Stats.Duplicates++
			res.Stats.addReason("duplicate ip:port")
		}
	}

	if res.Stats.RawItems == 0 {
		return nil, fmt.Errorf("parse text: no data lines found")
	}
	if len(dedup.targets) == 0 {
		return nil, fmt.Errorf("parse text: no valid ip:port found in %d line(s)", res.Stats.RawItems)
	}

	res.Targets = dedup.targets
	res.Warnings = warnings.items()
	return res, nil
}

// parseSummary 解析 list 摘要字段（国家统计与自称的总数）。
func parseSummary(list map[string]any) (int, map[string]int) {
	if list == nil {
		return 0, nil
	}

	total, _ := asInt(list["ips"])

	rawCountries := getMap(list, "country")
	if len(rawCountries) == 0 {
		return total, nil
	}

	counts := make(map[string]int, len(rawCountries))
	for code, v := range rawCountries {
		n, ok := asInt(v)
		if !ok {
			continue
		}
		counts[strings.ToUpper(strings.TrimSpace(code))] = n
	}
	if len(counts) == 0 {
		return total, nil
	}
	return total, counts
}

// ---------------------------------------------------------------------------
// 字段级解析
// ---------------------------------------------------------------------------

// parseHost 解析并规范化一个主机地址字符串。
//
// 规则：
//   - 接受 IPv4 与 IPv6；IPv6 允许带方括号（文本源可能出现）；
//   - 去掉 zone（例如 fe80::1%eth0），因为 zone 是本机信息；
//   - 拒绝未指定地址（0.0.0.0 / ::）与组播地址：
//     它们不可能是可测量的公网目标，属于数据异常；
//   - 返回的 Addr 一定是 Unmap 过的（IPv4-mapped IPv6 会被还原成 IPv4），
//     避免同一个地址出现两种字符串形式导致去重失效。
func parseHost(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, fmt.Errorf("empty address")
	}

	// 容忍 "[2001:db8::1]" 形式。
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}

	// 去掉 zone。
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}

	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("not an ip address")
	}
	addr = addr.Unmap().WithZone("")

	if addr.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("unspecified address")
	}
	if addr.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("multicast address")
	}

	return addr, nil
}

// splitHostPort 把 "host:port" 拆开，支持 IPv6 的 "[::1]:443" 形式。
func splitHostPort(s string) (host, port string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("empty host:port")
	}

	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", fmt.Errorf("malformed address %q: missing ]", truncate(s, 48))
		}
		host = s[1:end]
		rest := s[end+1:]
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("malformed address %q: missing port", truncate(s, 48))
		}
		return host, rest[1:], nil
	}

	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("malformed address %q: missing port", truncate(s, 48))
	}
	return s[:i], s[i+1:], nil
}

// normalizePorts 把 JSON 中的 port 字段规范化为端口列表。
//
// 真实数据里 port 是数组；这里同时容忍单个数字（防御上游改动）。
// 返回值：
//
//	ports    有效的端口列表（已去重、升序）
//	bad      无法使用的端口值个数
//	ok       false 表示该记录完全没有端口信息（应整条跳过并计入 Unknown）
func normalizePorts(v any) (ports []int, bad int, ok bool) {
	switch t := v.(type) {
	case nil:
		return nil, 0, false

	case []any:
		if len(t) == 0 {
			return nil, 0, false
		}
		seen := make(map[int]struct{}, len(t))
		for _, e := range t {
			p, valid := asInt(e)
			if !valid || !model.ValidPort(p) {
				bad++
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			ports = append(ports, p)
		}
		sortInts(ports)
		return ports, bad, len(ports) > 0

	default:
		// 容忍 "port": 443 这种单个数字写法。
		if p, valid := asInt(v); valid {
			if model.ValidPort(p) {
				return []int{p}, 0, true
			}
			return nil, 1, false
		}
		return nil, 1, false
	}
}

// sortInts 对端口做插入排序。
//
// 端口数组非常小（实测最多 6 个），插入排序比引入 sort 更直观，
// 也保证输出顺序稳定（便于测试与 diff）。
func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// parseLocation 把 meta（含 colo）转换为 model.Location。
//
// 该函数只做字段映射与容错，不做任何"猜测"以外的加工：
// 所有字段都原样来自源数据，缺失就留空。
func parseLocation(meta map[string]any) model.Location {
	loc := model.Location{}
	if meta == nil {
		return loc
	}

	loc.Country = upperTrim(getString(meta, "country"))
	loc.Region = getString(meta, "region")
	loc.City = getString(meta, "city")
	loc.CountryEN = getString(meta, "country_en")

	if colo := getMap(meta, "colo"); colo != nil {
		loc.CCA2 = upperTrim(getString(colo, "cca2"))
		loc.IATA = upperTrim(getString(colo, "iata"))
		// colo.region / colo.city 是"接入点位置"，与目标地理位置不同，
		// 只有在 meta 自身没有 region/city 时才作为降级填充。
		if loc.Region == "" {
			loc.Region = getString(colo, "region")
		}
		if loc.City == "" {
			loc.City = getString(colo, "city")
		}
	}

	// CCA2 缺失时回退到 meta.country：两者含义不同，
	// 但在"目标 IP 属于哪个国家"这一点上，国家字段比空值更有用。
	if loc.CCA2 == "" {
		loc.CCA2 = loc.Country
	}

	// 经纬度优先级：meta.latitude/longitude（目标 IP 地理位置）
	// 优先于 colo.lat/lon（Cloudflare 接入点）。回退时显式标记。
	if lat, ok := asFloat(meta["latitude"]); ok {
		if lon, ok := asFloat(meta["longitude"]); ok {
			loc.Latitude, loc.Longitude = lat, lon
			loc.HasCoordinates = true
			return loc
		}
	}

	// 坐标不完整：尝试 colo 坐标（仅当两个值都有效时）。
	if colo := getMap(meta, "colo"); colo != nil {
		if lat, ok := asFloat(colo["lat"]); ok {
			if lon, ok := asFloat(colo["lon"]); ok {
				loc.Latitude, loc.Longitude = lat, lon
				loc.HasCoordinates = true
				loc.FromColoFallback = true
			}
		}
	}

	return loc
}

// upperTrim 统一处理国家/地区代码的大小写与空白。
func upperTrim(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// truncate 截断过长的值，避免把整条畸形 JSON 写进警告。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// 去重与告警辅助
// ---------------------------------------------------------------------------

// deduper 按 (IP, Port) 去重，并保持首次出现的顺序。
//
// 顺序稳定很重要：扫描进度、断点续测、导出 diff 都依赖目标顺序一致。
// 重复时保留**首次出现**的记录，保证同一份输入每次解析结果完全相同。
type deduper struct {
	seen    map[string]struct{}
	targets []model.Target
}

func newDeduper() *deduper {
	return &deduper{seen: make(map[string]struct{})}
}

// add 尝试加入一个目标。返回 false 表示是重复项。
func (d *deduper) add(t model.Target) bool {
	if _, dup := d.seen[t.ID]; dup {
		return false
	}
	d.seen[t.ID] = struct{}{}
	d.targets = append(d.targets, t)
	return true
}

// warningList 收集有限条数的告警，并做去重。
//
// 目的：数据源出现大量同类问题时，输出"前 N 条 + 总数"，
// 而不是把整个列表刷到终端或日志里。
type warningList struct {
	max    int
	items_ []string
	seen   map[string]struct{}
	total  int
}

func newWarningList(max int) *warningList {
	return &warningList{max: max, seen: make(map[string]struct{})}
}

// add 记录一条告警（按格式化内容去重）。
func (w *warningList) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.total++
	if _, dup := w.seen[msg]; dup {
		return
	}
	w.seen[msg] = struct{}{}
	if len(w.items_) < w.max {
		w.items_ = append(w.items_, msg)
	}
}

// items 返回告警列表，必要时追加一条"还有 N 条被省略"。
func (w *warningList) items() []string {
	if w.total <= len(w.items_) {
		return w.items_
	}
	out := make([]string, 0, len(w.items_)+1)
	out = append(out, w.items_...)
	out = append(out, fmt.Sprintf("... and %d more similar warnings", w.total-len(w.items_)))
	return out
}

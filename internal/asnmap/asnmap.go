// Package asnmap 把 ASN 翻译成运营商内部的**线路名称**。
//
// 为什么需要它：`AS4134`、`AS4809`、`AS4837` 这些数字对绝大多数人
// 没有意义，而它们实际代表的是中国三大运营商各自的骨干网档次——
// 这正是"这条线路好不好"的关键信息：
//
//	AS4134  -> 163      中国电信普通出口
//	AS4809  -> CN2      中国电信优质出口
//	AS4837  -> 169      中国联通普通出口
//	AS9929  -> 9929/CUII 中国联通精品网
//	AS9808  -> CMNET    中国移动普通出口
//	AS58453 -> CMI      中国移动国际
//	AS58807 -> CMIN2    中国移动优质出口
//	AS10099 -> CUG      中国联通国际
//
// 本包**只做名称翻译**，不给出任何好坏评分：163 在空闲时段
// 可能比 CN2 还快，评价取决于用途与时段，程序不替使用者下结论。
//
// == 表的维护原则 ==
//
// 国内骨干线路那几项是公认的（运维与玩家圈子长期通用），列在下面。
// 其余只收录**广为人知、不会认错**的运营商/云厂商（Cloudflare、
// AWS、Google 等）。刻意不往里塞"大概是哪家"的 ASN：
// 猜错的名字比没有名字更糟——使用者会据此做判断。
package asnmap

import (
	"sort"
	"strconv"
	"strings"
)

// Route 描述一个 ASN 对应的线路。
type Route struct {
	// ASN 是规范的 AS 编号，例如 "AS4134"。
	ASN string

	// Name 是线路的通用名称，例如 "163"、"CN2"。
	Name string

	// Operator 是所属运营商，便于按家分组阅读。
	Operator string

	// Note 是一句补充说明（可为空）。
	//
	// 只描述事实（这条线路的定位/别称），不写"好/差"这类判断。
	Note string
}

// routes 是已知线路表。
//
// 键是规范化的 "AS<数字>"。
var routes = map[string]Route{
	// --- 中国电信 ---
	"AS4134": {ASN: "AS4134", Name: "163", Operator: "中国电信",
		Note: "ChinaNet 普通出口"},
	"AS4809": {ASN: "AS4809", Name: "CN2", Operator: "中国电信",
		Note: "下一代承载网（优质出口）"},

	// --- 中国联通 ---
	"AS4837": {ASN: "AS4837", Name: "169", Operator: "中国联通",
		Note: "China169 普通出口"},
	"AS9929": {ASN: "AS9929", Name: "9929/CUII", Operator: "中国联通",
		Note: "工业互联网骨干，俗称 9929"},
	"AS10099": {ASN: "AS10099", Name: "CUG", Operator: "中国联通",
		Note: "China Unicom Global"},

	// --- 中国移动 ---
	"AS9808": {ASN: "AS9808", Name: "CMNET", Operator: "中国移动",
		Note: "移动普通出口"},
	// 省网出口：它们跑的还是 CMNET，归到同一个名字，
	// 于是路径里连续的省网跳不会重复显示成一长串。
	"AS56046": {ASN: "AS56046", Name: "CMNET", Operator: "中国移动",
		Note: "移动省网出口"},
	"AS56048": {ASN: "AS56048", Name: "CMNET", Operator: "中国移动",
		Note: "移动省网出口"},
	"AS58453": {ASN: "AS58453", Name: "CMI", Operator: "中国移动",
		Note: "China Mobile International"},
	"AS58807": {ASN: "AS58807", Name: "CMIN2", Operator: "中国移动",
		Note: "移动优质出口"},

	// --- 国际网络与云厂商（广为人知，不会认错） ---
	"AS13335":  {ASN: "AS13335", Name: "Cloudflare", Operator: "Cloudflare"},
	"AS16509":  {ASN: "AS16509", Name: "AWS", Operator: "Amazon"},
	"AS15169":  {ASN: "AS15169", Name: "Google", Operator: "Google"},
	"AS8075":   {ASN: "AS8075", Name: "Microsoft", Operator: "Microsoft"},
	"AS6939":   {ASN: "AS6939", Name: "HE", Operator: "Hurricane Electric"},
	"AS2914":   {ASN: "AS2914", Name: "NTT", Operator: "NTT", Note: "日本"},
	"AS2497":   {ASN: "AS2497", Name: "IIJ", Operator: "IIJ", Note: "日本"},
	"AS3491":   {ASN: "AS3491", Name: "PCCW", Operator: "PCCW", Note: "香港"},
	"AS174":    {ASN: "AS174", Name: "Cogent", Operator: "Cogent"},
	"AS3356":   {ASN: "AS3356", Name: "Lumen", Operator: "Lumen"},
	"AS1299":   {ASN: "AS1299", Name: "Arelion", Operator: "Arelion"},
	"AS3257":   {ASN: "AS3257", Name: "GTT", Operator: "GTT"},
	"AS9009":   {ASN: "AS9009", Name: "M247", Operator: "M247"},
	"AS14061":  {ASN: "AS14061", Name: "DigitalOcean", Operator: "DigitalOcean"},
	"AS20473":  {ASN: "AS20473", Name: "Vultr", Operator: "Vultr"},
	"AS63949":  {ASN: "AS63949", Name: "Akamai", Operator: "Akamai"},
	"AS45102":  {ASN: "AS45102", Name: "Alibaba", Operator: "阿里云"},
	"AS37963":  {ASN: "AS37963", Name: "Alibaba", Operator: "阿里云"},
	"AS45090":  {ASN: "AS45090", Name: "Tencent", Operator: "腾讯云"},
	"AS132203": {ASN: "AS132203", Name: "Tencent", Operator: "腾讯云"},
}

// Normalize 把各种写法的 ASN 归一化成 "AS<数字>" 形式。
//
// 输入可能来自 NextTrace 的 "56046"、用户写的 "as56046"、
// 或已经是规范的 "AS56046"。无法识别时返回空字符串。
func Normalize(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	digits := trimmed
	if len(digits) >= 2 && strings.EqualFold(digits[:2], "as") {
		digits = digits[2:]
	}
	digits = strings.TrimSpace(digits)
	if digits == "" {
		return ""
	}

	// ASN 是 32 位无符号整数；只接受纯数字，避免把
	// "AS9808-AS58453" 这类路径串当成单个 ASN。
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "AS" + digits
}

// Lookup 返回某个 ASN 对应的线路信息。
//
// 第二个返回值表示是否命中已知表：**未命中不是错误**，
// 国际 transit 与小型运营商本来就没有通用名称。
func Lookup(asn string) (Route, bool) {
	normalized := Normalize(asn)
	if normalized == "" {
		return Route{}, false
	}
	route, found := routes[normalized]
	if !found {
		return Route{ASN: normalized}, false
	}
	return route, true
}

// Name 返回 ASN 的线路名称；未知时返回空字符串。
//
// 未知返回空是刻意的：调用方应当据此显示 ASN 编号本身，
// 而不是显示一个编造的名字。
func Name(asn string) string {
	route, ok := Lookup(asn)
	if !ok {
		return ""
	}
	return route.Name
}

// Label 返回用于显示的一行文本。
//
//	已知：  "AS4134 (163)"
//	未知：  "AS12345"
//	空输入：""
//
// 刻意保留 ASN 编号：名称只是便于阅读，排查问题时仍需要编号。
func Label(asn string) string {
	normalized := Normalize(asn)
	if normalized == "" {
		return ""
	}
	if name := Name(normalized); name != "" {
		return normalized + " (" + name + ")"
	}
	return normalized
}

// ShortLabel 返回更短的形式，用于表格列。
//
//	已知："163"；未知："AS12345"；空：""
//
// 未知时显示编号而不是留空：留空会让人以为"没有数据"。
func ShortLabel(asn string) string {
	normalized := Normalize(asn)
	if normalized == "" {
		return ""
	}
	if name := Name(normalized); name != "" {
		return name
	}
	return normalized
}

// Operator 返回 ASN 所属运营商；未知时返回空字符串。
func Operator(asn string) string {
	route, ok := Lookup(asn)
	if !ok {
		return ""
	}
	return route.Operator
}

// ShortPath 把一串 ASN 压成"线路名称"串，用于一行日志或一个表格单元格。
//
//	"163 > CN2 > Cloudflare"
//
// 规则：
//
//   - 只保留**有名字**的 ASN（国际 transit 与小型运营商略去，
//     它们出现在一行里只会挤掉有用信息）；
//   - **相邻**重复合并，但不相邻的重复保留：
//     "163 > CN2 > 163" 表示流量出去又绕回来，这是重要信息。
//
// 未知项被全部略去时返回空字符串——调用方应当据此显示
// "未识别出已知骨干线路"，而不是显示一个空白标签。
func ShortPath(asns []string) string {
	var (
		parts []string
		prev  string
	)

	for _, raw := range asns {
		asn := Normalize(raw)
		if asn == "" {
			continue
		}
		name := Name(asn)
		if name == "" {
			continue
		}
		if name == prev {
			continue
		}
		prev = name
		parts = append(parts, name)
	}

	return strings.Join(parts, " > ")
}

// FullPath 与 ShortPath 类似，但保留 ASN 编号且不略去未知项：
//
//	"163(AS4134) > CN2(AS4809) > AS13335"
//
// 用于需要核对编号的场合（命令行 --hops 输出）。
func FullPath(asns []string) string {
	var (
		parts []string
		prev  string
	)

	for _, raw := range asns {
		asn := Normalize(raw)
		if asn == "" {
			continue
		}
		if asn == prev {
			continue
		}
		prev = asn

		if name := Name(asn); name != "" {
			parts = append(parts, name+"("+asn+")")
		} else {
			parts = append(parts, asn)
		}
	}

	return strings.Join(parts, " > ")
}

// Known 返回已知线路表（按 ASN 数字升序）。
func Known() []Route {
	out := make([]Route, 0, len(routes))
	for _, route := range routes {
		out = append(out, route)
	}
	sort.Slice(out, func(i, j int) bool {
		return asnNumber(out[i].ASN) < asnNumber(out[j].ASN)
	})
	return out
}

// asnNumber 把 "AS4134" 解析成 4134（解析失败返回最大整数，
// 让未知项排在最后而不是乱序）。
func asnNumber(asn string) int {
	digits := strings.TrimPrefix(asn, "AS")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return n
}

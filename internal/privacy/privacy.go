// Package privacy 实现公开数据前的隐私过滤。
//
// 为什么单独一个包：需求第 55~57、63 条把"什么东西绝不能出现在公开数据里"
// 定成了硬约束，而这类约束如果散落在导出、上传、聚合三处，迟早会有一处漏掉。
// 集中在这里之后，**只有一条路径**能产生对外数据，并且这条路径有测试守着。
//
// 边界（写清楚，因为"隐私过滤"很容易变成含糊的口号）：
//
//   - 本包**不负责**采集者身份：collector_id 本来就是随机生成的匿名标识，
//     它必须保留（否则无法区分不同节点的数据），不需要过滤。
//   - 本包**不负责**目标 IP：目标来自公开的 all.json，它本身就是公开数据。
//   - 本包负责的是**测量过程中暴露出来的本机与会话内网信息**：
//     路径上的内网地址、本地网关、以及任何指向"这个采集者在哪张内网里"
//     的东西。这类信息既无公开价值，又能定位到个人网络。
package privacy

import (
	"net"
	"strings"
)

// RedactedIPv4 / RedactedIPv6 是内网地址被替换后的占位符。
//
// 为什么要占位而不是直接删掉那一跳：跳的位置与顺序是线路信息的一部分
// （"第 3 跳是内网私有地址" 说明流量还没出局域网），
// 抹掉整跳会让后续跳的编号失去参照。
const (
	RedactedIPv4 = "private-v4"
	RedactedIPv6 = "private-v6"
)

// IsPrivateAddress 报告 IP 是否属于不可公开的地址范围。
//
// 覆盖的类别比 net.IP.IsPrivate 更宽，因为**能定位到个人网络**的不止
// RFC1918：运营商级 NAT（100.64/10）能定位到运营商内网，
// 链路本地（169.254/16、fe80::/10）能暴露本地拓扑，
// 而环回与未指定地址出现在路径里说明数据本身有问题。
//
// 判定基于解析后的地址，而不是字符串前缀——"192.168.1.1"
// 与 "::ffff:192.168.1.1" 必须得到同一个结论。
func IsPrivateAddress(ip net.IP) bool {
	if ip == nil {
		return true // 解析失败：宁可当作不可公开
	}

	// 环回、未指定、链路本地组播等：一律不可公开。
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}

	if ip.IsPrivate() {
		return true
	}

	if v4 := ip.To4(); v4 != nil {
		return isPrivateIPv4(v4)
	}
	return isPrivateIPv6(ip)
}

// isPrivateIPv4 覆盖 IPv4 里各种"不可公开"的范围。
func isPrivateIPv4(ip net.IP) bool {
	switch {
	// 0.0.0.0/8：本网络。
	case ip[0] == 0:
		return true
	// 100.64.0.0/10：运营商级 NAT（RFC 6598）。
	case ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127:
		return true
	// 192.0.0.0/24：IETF 协议分配。
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 0:
		return true
	// 192.0.2.0/24、198.51.100.0/24、203.0.113.0/24：文档用途。
	// 这些地址出现在真实测量里意味着数据被伪造或来自测试环境，
	// 公开它们会污染数据库。
	case ip[0] == 192 && ip[1] == 0 && ip[2] == 2:
		return true
	case ip[0] == 198 && ip[1] == 51 && ip[2] == 100:
		return true
	case ip[0] == 203 && ip[1] == 0 && ip[2] == 113:
		return true
	// 198.18.0.0/15：基准测试。
	case ip[0] == 198 && (ip[1] == 18 || ip[1] == 19):
		return true
	// 240.0.0.0/4：保留（含 255.255.255.255 广播）。
	case ip[0] >= 240:
		return true
	default:
		return false
	}
}

// isPrivateIPv6 覆盖 IPv6 里的特殊范围。
func isPrivateIPv6(ip net.IP) bool {
	// 唯一本地地址 fc00::/7（RFC 4193）。
	if len(ip) >= 1 && ip[0]&0xfe == 0xfc {
		return true
	}
	// 文档用途 2001:db8::/32。
	if len(ip) >= 4 && ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}
	// IPv4 映射与 IPv4 兼容地址：按内嵌的 IPv4 判定。
	if v4 := ip.To4(); v4 != nil {
		return isPrivateIPv4(v4)
	}
	return false
}

// IsPrivateString 是 IsPrivateAddress 的字符串入口。
//
// 无法解析的字符串返回 true：解析不了就没法证明它可公开。
// 空字符串返回 false（表示"没有地址"，例如超时跳），
// 由调用方决定怎么处理——空地址本身不泄露任何东西。
func IsPrivateString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// 去掉可能是 "IP%zone" 形式的接口区域后缀（IPv6 链路本地会用）。
	if index := strings.IndexByte(s, '%'); index >= 0 {
		s = s[:index]
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return true
	}
	return IsPrivateAddress(ip)
}

// Redact 把一个不可公开的地址替换成占位符；可公开的地址原样返回。
//
// 空字符串原样返回（"这一跳没有回复"不是隐私问题）。
func Redact(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return ""
	}
	if !IsPrivateString(trimmed) {
		return trimmed
	}
	// 用 To4 判断族别，保证 "::ffff:10.0.0.1" 也得到 private-v4。
	if ip := net.ParseIP(stripZone(trimmed)); ip != nil {
		if ip.To4() != nil {
			return RedactedIPv4
		}
		return RedactedIPv6
	}
	return RedactedIPv4
}

// stripZone 去掉 IPv6 的 %zone 后缀。
func stripZone(s string) string {
	if index := strings.IndexByte(s, '%'); index >= 0 {
		return s[:index]
	}
	return s
}

// ---------------------------------------------------------------------------
// 坐标
// ---------------------------------------------------------------------------

// Coordinate 是一对（纬度, 经度）。
//
// 用指针语义的 HasValue 而不是 0 表示"未知"：0,0 是几内亚湾的合法坐标，
// 用它表示"没有坐标"会把一批目标误判到同一个点上（Phase 4 已定的约定）。
type Coordinate struct {
	Lat float64
	Lng float64
	Set bool
}

// CoordinatesForTarget 决定目标坐标能否公开。
//
// 规则：
//   - 没有坐标 -> 不公开（Set=false）；
//   - 坐标是 (0,0) 且不是"真的在几内亚湾"的证据时 -> 视为缺失。
//     这里无法区分两者，因此**保留**原始值：0,0 是合法的，
//     而把它当作缺失会丢掉真实的几内亚湾节点。判断依据是 lat/lng
//     字段是否真的存在（由上游解析层决定），本函数只负责
//     "存在就公开、不存在就不公开"。
//
// 换句话说：本函数不做猜测，只做搬运。
func CoordinatesForTarget(lat, lng *float64) Coordinate {
	if lat == nil || lng == nil {
		return Coordinate{}
	}
	return Coordinate{Lat: *lat, Lng: *lng, Set: true}
}

// ---------------------------------------------------------------------------
// 目标地址
// ---------------------------------------------------------------------------

// TargetKind 描述一个目标地址为什么（不）可以公开。
type TargetKind int

const (
	// TargetPublic 是正常的公网目标，可以直接公开。
	TargetPublic TargetKind = iota

	// TargetPrivate 是内网/保留地址目标。
	//
	// 这类目标通常来自用户自己的测试数据（例如用本机监听做验证），
	// 它们**不属于**公开数据库的内容：既没有普遍价值，
	// 又会暴露采集者所在内网的地址规划。
	TargetPrivate

	// TargetInvalid 是无法解析的地址：数据有问题，不公开。
	TargetInvalid
)

// String 实现 fmt.Stringer，用于导出报告。
func (k TargetKind) String() string {
	switch k {
	case TargetPublic:
		return "public"
	case TargetPrivate:
		return "private"
	case TargetInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// ClassifyTarget 判定目标地址可否公开。
func ClassifyTarget(ip string) TargetKind {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return TargetInvalid
	}
	parsed := net.ParseIP(stripZone(trimmed))
	if parsed == nil {
		return TargetInvalid
	}
	if IsPrivateAddress(parsed) {
		return TargetPrivate
	}
	return TargetPublic
}

// PublicTarget 报告目标是否应当出现在公开数据里。
//
// 只有 TargetPublic 会被公开。这是需求第 56 条的直接落点：
// 私有 IP 必须在生成公开数据**之前**被过滤掉，
// 而不是"上传时再过滤"——上传路径一多，漏掉一处就发出去了。
func PublicTarget(ip string) bool {
	return ClassifyTarget(ip) == TargetPublic
}

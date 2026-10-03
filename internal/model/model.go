// Package model 定义 cf-route-tester 的核心业务数据模型。
//
// 这一层刻意与任何外部依赖解耦：
//
//   - 不依赖 all.json 的原始 JSON 结构；
//   - 不依赖 NextTrace 的原始 JSON 结构；
//   - 不依赖 SQLite 的表结构。
//
// 外部数据格式变化时，修改的是解析层（internal/source、internal/trace），
// 而不是这里，也不是数据库结构。
//
// 命名上必须严格区分两个"位置"：
//
//	Target.Location      目标 IP 自身的地理信息（来自 all.json 的 meta）
//	CollectorProfile     测量者所在的地理 / 运营商信息（来自本机检测或手动配置）
//
// 两者含义不同，任何情况下都不能混用。
package model

import (
	"net/netip"
	"strconv"
	"strings"
)

// IPVersion 表示测量所使用的 IP 协议版本。
type IPVersion string

const (
	// IPVersionIPv4 表示 IPv4。
	IPVersionIPv4 IPVersion = "ipv4"

	// IPVersionIPv6 表示 IPv6。
	IPVersionIPv6 IPVersion = "ipv6"

	// IPVersionUnknown 表示尚不确定（例如尚未探测）。
	IPVersionUnknown IPVersion = "unknown"
)

// IPVersionOf 根据地址推断 IP 版本。
//
// 参数必须是已经过 netip.ParseAddr 校验的地址。
func IPVersionOf(addr netip.Addr) IPVersion {
	if !addr.IsValid() {
		return IPVersionUnknown
	}
	if addr.Is4() {
		return IPVersionIPv4
	}
	return IPVersionIPv6
}

// Location 描述**目标 IP 自身**的地理信息。
//
// 它来自 all.json 中该目标的 meta 字段（不是测量者的位置）。
//
// 字段语义：
//
//	CCA2      国家/地区代码，来自 meta.colo.cca2（2 位大写）
//	IATA      Cloudflare colo 的 IATA 机场代码，来自 meta.colo.iata（3 位大写）
//	Region    meta.region（一级行政区，例如 Illinois）
//	City      meta.city
//	Latitude  目标 IP 地理位置纬度（不是 colo 纬度）
//	Longitude 目标 IP 地理位置经度（不是 colo 经度）
//	Country   meta.country（2 位大写），与 CCA2 可能不同
//	CountryEN meta.country_en，便于展示，不参与任何判断
//
// 关于经纬度的一个刻意取舍：
// meta.latitude/longitude 是 IPv4 地理定位结果，meta.colo.lat/lon 是 Cloudflare
// 接入点坐标。生产数据中两者在约 21% 的记录上属于不同国家。
// 这里保存的是 meta.latitude/longitude（目标 IP 的地理位置），
// 并在两者都缺失时才回退到 colo 坐标；回退情况由 FromColoFallback 标记，
// 避免下游把两种来源的坐标当成同一件事。
type Location struct {
	CCA2      string
	IATA      string
	Region    string
	City      string
	Latitude  float64
	Longitude float64
	Country   string
	CountryEN string

	// HasCoordinates 表示 Latitude/Longitude 是否来自有效数据。
	//
	// 0,0 是一个合法坐标，因此不能用零值判断"没有坐标"。
	HasCoordinates bool

	// FromColoFallback 表示坐标回退使用了 colo 坐标
	// （即 meta 自身经纬度缺失或非法）。
	FromColoFallback bool
}

// IsZero 报告该 Location 是否完全没有可用信息。
func (l Location) IsZero() bool {
	return l.CCA2 == "" && l.IATA == "" && l.Region == "" && l.City == "" &&
		l.Country == "" && l.CountryEN == "" && !l.HasCoordinates
}

// String 返回便于日志展示的简短形式，例如 "US/ORD/Chicago"。
func (l Location) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{l.Country, l.IATA, l.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "/")
}

// Target 是一个测量目标，唯一标识为 (IP, Port)。
//
// 一个 Target 等价于一行 "IP:Port"，例如 "1.2.3.4:443"。
// 同一个 IP 的多个端口会产生多个独立 Target，因为最终数据库的主键维度是
// IP × Port，而不是 IP。
type Target struct {
	// ID 是稳定且人类可读的标识：IPv4 为 "1.2.3.4:443"，
	// IPv6 为规范化后的 "[2001:db8::1]:443"。
	//
	// 它由 IP 与 Port 决定，不含任何时间或随机成分，
	// 因此在多次扫描、多次上传之间保持一致。
	ID string

	// IP 是规范化后的 IP 字符串（IPv6 使用压缩形式，无 zone）。
	IP string

	// Port 是 TCP 端口，范围 1-65535。
	Port int

	// IPVersion 由 IP 推断，供数据库与聚合分组使用。
	IPVersion IPVersion

	// Location 是**目标 IP 自身**的地理信息。
	Location Location
}

// NewTarget 构造一个 Target，并生成规范化的 ID。
//
// addr 必须已经过校验（netip.ParseAddr 成功且无 zone）。
// 端口非法时返回空 Target 与 false。
func NewTarget(addr netip.Addr, port int) (Target, bool) {
	if !addr.IsValid() {
		return Target{}, false
	}
	if !ValidPort(port) {
		return Target{}, false
	}

	// 统一去掉 IPv6 zone（例如 fe80::1%eth0），
	// 因为 zone 是本机信息，不应进入数据库或公开数据。
	addr = addr.WithZone("")
	ip := addr.String()

	return Target{
		ID:        TargetID(ip, port),
		IP:        ip,
		Port:      port,
		IPVersion: IPVersionOf(addr),
	}, true
}

// TargetID 根据 IP 与端口生成规范 ID。
//
// IPv6 使用 "[addr]:port" 形式，避免与端口分隔符产生歧义。
func TargetID(ip string, port int) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]:" + strconv.Itoa(port)
	}
	return ip + ":" + strconv.Itoa(port)
}

// Addr 返回 Target 的地址。
func (t Target) Addr() (netip.Addr, bool) {
	addr, err := netip.ParseAddr(t.IP)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.WithZone(""), true
}

// ParseAddr 解析并规范化一个 IP 字符串。
//
// 与 source/parser.go 中的 parseHost 相比，这里不做"公网可达性"判断，
// 只做规范化：去掉 zone、还原 IPv4-mapped IPv6。
//
// 用途：从缓存、数据库、命令行等已受信任的来源读取 IP 时使用。
func ParseAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// AddrPort 返回 Target 的 IP:Port。
func (t Target) AddrPort() (netip.AddrPort, bool) {
	addr, ok := t.Addr()
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr, uint16(t.Port)), true
}

// String 返回规范 ID，便于日志与测试中使用。
func (t Target) String() string {
	if t.ID != "" {
		return t.ID
	}
	return TargetID(t.IP, t.Port)
}

// AddrMustString 返回规范化后的 IP 字符串。
//
// 用于测试与日志：无法解析时返回原始字符串，
// 而不是 panic 或返回空串（避免掩盖数据问题）。
func (t Target) AddrMustString() string {
	if addr, ok := t.Addr(); ok {
		return addr.String()
	}
	return t.IP
}

// ValidPort 报告端口是否在合法的 TCP 端口范围内。
//
// 0 被排除：它是"任意端口"的占位值，不能作为测量目标。
func ValidPort(port int) bool {
	return port >= 1 && port <= 65535
}

// CollectorProfile 描述**测量者**所在的位置与运营商。
//
// 它与 Target.Location 完全无关：
//
//	Target.Location      测的是"这个目标 IP 在哪"
//	CollectorProfile     记的是"我在哪、我用哪家运营商"
//
// 该结构被刻意限制为"粗粒度、不可定位到个人"的信息：
// 国家、省份、城市、运营商、ASN、IP 版本。
//
// 禁止在此结构中增加：
//
//	公网 IP、内网 IP、MAC、主机名、设备唯一标识、精确经纬度、住址
type CollectorProfile struct {
	// Country 是国家/地区代码或名称，例如 "CN"。
	Country string

	// Province 是一级行政区，例如 "Zhejiang"。
	Province string

	// City 是城市，例如 "Hangzhou"。
	City string

	// ISP 是运营商名称，例如 "China Mobile"。
	ISP string

	// ASN 是运营商的自治系统号，例如 "AS9808"。
	ASN string

	// IPVersion 是本次测量使用的 IP 版本。
	IPVersion IPVersion
}

// IsZero 报告该 Profile 是否完全没有填写。
func (c CollectorProfile) IsZero() bool {
	return c.Country == "" && c.Province == "" && c.City == "" &&
		c.ISP == "" && c.ASN == "" && c.IPVersion == ""
}

// GroupKey 返回用于聚合分组的稳定键，例如 "CN|Zhejiang|Hangzhou|China Mobile|AS9808|ipv4"。
//
// 使用固定分隔符与固定字段顺序，保证不同采集者、不同时间的相同分组
// 生成完全一致的键，便于聚合与去重。
func (c CollectorProfile) GroupKey() string {
	parts := []string{
		c.Country,
		c.Province,
		c.City,
		c.ISP,
		c.ASN,
		string(c.IPVersion),
	}
	return strings.Join(parts, "|")
}

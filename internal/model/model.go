// Package model 定义 cf-route-tester 的核心业务数据模型。
//
// 这一层刻意与任何外部依赖解耦：
//
//   - 不依赖 all.json 的原始 JSON 结构（解析层负责映射）；
//   - 不依赖 NextTrace 的原始 JSON 结构（解析层负责映射）；
//   - 不依赖 SQLite 的表结构（存储层负责映射）；
//   - 不带任何 JSON/YAML 标签：序列化格式由导出层显式决定，
//     避免"改一个字段名就悄悄改变了公开数据格式"。
//
// 命名上必须严格区分三类位置信息，任何情况下都不能混用：
//
//	Target.Location      目标 IP 自身的地理位置（来自 all.json 的 meta）
//	Target.Colo          处理该目标的 Cloudflare 接入点（colo）信息
//	CollectorProfile     测量者所在的地理 / 运营商信息（本机检测或手动配置）
//
// 之所以把 Colo 从 Location 中拆出来：生产数据里 meta.country 与
// colo.cca2 有约 21% 的记录不一致，两者回答的是不同问题
// （"这个 IP 在哪" vs "这个 IP 从哪里接入 Cloudflare"）。
// 混在一个结构里，下游迟早会把它们当成同一件事。
package model

import (
	"errors"
	"fmt"
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

// Valid 报告该版本值是否是已定义的取值。
func (v IPVersion) Valid() bool {
	switch v {
	case IPVersionIPv4, IPVersionIPv6, IPVersionUnknown:
		return true
	default:
		return false
	}
}

// IPVersionOf 根据地址推断 IP 版本。
//
// 参数必须是已经过 netip.ParseAddr 校验、且已 Unmap 的地址。
func IPVersionOf(addr netip.Addr) IPVersion {
	if !addr.IsValid() {
		return IPVersionUnknown
	}
	if addr.Is4() {
		return IPVersionIPv4
	}
	return IPVersionIPv6
}

// ---------------------------------------------------------------------------
// Colo
// ---------------------------------------------------------------------------

// ColoInfo 描述处理某个目标的 Cloudflare 接入点（colo）。
//
// 它**不是**目标 IP 的地理位置：目标可能在德国，却从阿姆斯特丹接入。
//
// 字段来自 all.json 的 meta.colo：
//
//	IATA     机场代码（3 位大写），例如 ORD / AMS / NRT
//	CCA2     接入点所在国家/地区代码（2 位大写）
//	Region   接入点大区（例如 "North America" / "Europe"）
//	City     接入点城市
//	Latitude/Longitude 接入点坐标
type ColoInfo struct {
	IATA      string
	CCA2      string
	Region    string
	City      string
	Latitude  float64
	Longitude float64

	// HasCoordinates 表示 Latitude/Longitude 是否来自有效数据。
	HasCoordinates bool
}

// IsZero 报告该 ColoInfo 是否完全没有信息。
func (c ColoInfo) IsZero() bool {
	return c == ColoInfo{}
}

// Normalize 归一化字段（去空白、统一大写），并返回是否发生了修改。
func (c *ColoInfo) Normalize() bool {
	if c == nil {
		return false
	}
	before := *c

	c.IATA = normalizeCode(c.IATA)
	c.CCA2 = normalizeCode(c.CCA2)
	c.Region = strings.TrimSpace(c.Region)
	c.City = strings.TrimSpace(c.City)
	c.HasCoordinates = coordinatesInRange(c.Latitude, c.Longitude) && c.HasCoordinates

	return *c != before
}

// String 返回便于日志展示的简短形式，例如 "AMS/NL/Amsterdam"。
func (c ColoInfo) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{c.IATA, c.CCA2, c.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// Location
// ---------------------------------------------------------------------------

// Location 描述**目标 IP 自身**的地理位置。
//
// 字段来自 all.json 中该目标的 meta（不是测量者的位置，也不是 colo 位置）。
//
//	Country   meta.country（2 位大写）
//	CCA2      目标所在的 2 位国家/地区代码（缺失时回退 meta.country）
//	Region    meta.region（一级行政区，例如 Illinois）
//	City      meta.city
//	Latitude  目标地理位置纬度（不是 colo 纬度）
//	Longitude 目标地理位置经度（不是 colo 经度）
//	CountryEN meta.country_en，便于展示，不参与任何判断
//
// 注意 Country 与 CCA2 可能不同（上游 colo 国家与 IP 归属国家不一致时），
// 两者都保留：CCA2 用于聚合分组的"更精确归属"，
// Country 保留上游原始字段以便追溯。
//
// 不带 JSON 标签是刻意的：公开数据的字段名由导出层决定，
// 避免模型结构与公开 Schema 隐式绑定。
type Location struct {
	Country   string
	CCA2      string
	Region    string
	City      string
	Latitude  float64
	Longitude float64
	CountryEN string

	// HasCoordinates 表示 Latitude/Longitude 是否来自有效数据。
	//
	// 0,0 是一个合法坐标（几内亚湾），因此不能用零值判断"没有坐标"。
	HasCoordinates bool
}

// IsZero 报告该 Location 是否完全没有可用信息。
func (l Location) IsZero() bool {
	return l.Country == "" && l.CCA2 == "" && l.Region == "" && l.City == "" &&
		l.CountryEN == "" && !l.HasCoordinates
}

// Normalize 归一化字段（去空白、统一大写），并返回是否发生了修改。
//
// 归一化的意义：同一份数据无论来自解析、缓存还是数据库，
// 只要经过 Normalize，字符串比较与聚合分组都得到一致结果。
//
// 不做静默数据修正（例如把 "USA" 补成 "US"）：只做大小写与空白处理，
// 越界坐标只会把 HasCoordinates 置为 false 而不是改写数值。
func (l *Location) Normalize() bool {
	if l == nil {
		return false
	}
	before := *l

	l.Country = normalizeCode(l.Country)
	l.CCA2 = normalizeCode(l.CCA2)
	l.Region = strings.TrimSpace(l.Region)
	l.City = strings.TrimSpace(l.City)
	l.CountryEN = strings.TrimSpace(l.CountryEN)
	l.HasCoordinates = coordinatesInRange(l.Latitude, l.Longitude) && l.HasCoordinates

	return *l != before
}

// EffectiveCountry 返回用于分组/展示的国家代码。
//
// 优先 CCA2（来自 colo，覆盖率更高），为空时回退 Country。
func (l Location) EffectiveCountry() string {
	if l.CCA2 != "" {
		return l.CCA2
	}
	return l.Country
}

// ResolveCoordinates 返回可用的坐标及其来源。
//
// 返回顺序：目标自身坐标 -> colo 坐标 -> 无坐标。
// 第二个返回值 fromColo 为真表示坐标来自 colo（即"接入点位置"），
// 调用方可以选择不把它当作"目标位置"使用。
//
// 之所以提供这个函数而不是在模型里做回退赋值：
// 坐标一旦被回退填充，就再也分不清它描述的是目标还是接入点；
// 因此回退必须由调用方显式发起，并且知道自己在做什么。
func (l Location) ResolveCoordinates(colo ColoInfo) (lat, lon float64, ok, fromColo bool) {
	if l.HasCoordinates {
		return l.Latitude, l.Longitude, true, false
	}
	if colo.HasCoordinates {
		return colo.Latitude, colo.Longitude, true, true
	}
	return 0, 0, false, false
}

// String 返回便于日志展示的简短形式，例如 "US/Chicago"。
func (l Location) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{l.EffectiveCountry(), l.Region, l.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// Target
// ---------------------------------------------------------------------------

// Target 是一个测量目标，唯一标识为 (IP, Port)。
//
// 一个 Target 等价于一行 "IP:Port"，例如 "1.2.3.4:443"。
// 同一个 IP 的多个端口会产生多个独立 Target，因为最终数据库的主键维度是
// IP × Port，而不是 IP。
//
// 不变式（由 Validate 检查）：
//
//	ID == TargetID(IP, Port)
//	IP 可被解析为无 zone 的地址
//	1 <= Port <= 65535
//	IPVersion == IPVersionOf(IP)
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

	// Location 是**目标 IP 自身**的地理位置。
	Location Location

	// Colo 是处理该目标的 Cloudflare 接入点信息。
	Colo ColoInfo
}

// NewTarget 构造一个 Target，并生成规范化的 ID。
//
// addr 必须已经过校验（netip.ParseAddr 成功）。
// 端口非法时返回空 Target 与 false。
// IPv6 zone 会被去掉；IPv4-mapped IPv6 会被还原成 IPv4，
// 避免同一地址出现两种字符串形式导致去重失效。
func NewTarget(addr netip.Addr, port int) (Target, bool) {
	addr = CanonicalAddr(addr)
	if !addr.IsValid() {
		return Target{}, false
	}
	if !ValidPort(port) {
		return Target{}, false
	}

	ip := addr.String()

	return Target{
		ID:        TargetID(ip, port),
		IP:        ip,
		Port:      port,
		IPVersion: IPVersionOf(addr),
	}, true
}

// NewTargetFromStrings 从字符串形式的 IP 与端口构造 Target。
//
// 这是给已受信任来源（缓存、数据库、配置文件、命令行）使用的入口：
// 它不做"公网可达性"判断（那是解析层的职责），只做规范化与校验。
func NewTargetFromStrings(ip string, port int) (Target, error) {
	addr, ok := ParseAddr(ip)
	if !ok {
		return Target{}, fmt.Errorf("%w: ip %q", ErrInvalidTarget, ip)
	}
	target, ok := NewTarget(addr, port)
	if !ok {
		return Target{}, fmt.Errorf("%w: port %d", ErrInvalidTarget, port)
	}
	return target, nil
}

// ErrInvalidTarget 表示目标不满足 Target 的不变式。
var ErrInvalidTarget = errors.New("invalid target")

// ParseAddr 解析并规范化一个 IP 字符串。
//
// 与解析层的 IP 校验相比，这里不做"公网可达性"判断，
// 只做规范化：去掉 zone、还原 IPv4-mapped IPv6。
func ParseAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return CanonicalAddr(addr), true
}

// CanonicalAddr 把地址转换为本项目的规范形式：
// 去掉 zone，并把 IPv4-mapped IPv6 还原为 IPv4。
//
// 无效地址原样返回（IsValid 为 false），由调用方判断。
func CanonicalAddr(addr netip.Addr) netip.Addr {
	if !addr.IsValid() {
		return addr
	}
	return addr.Unmap().WithZone("")
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
	return ParseAddr(t.IP)
}

// AddrPort 返回 Target 的 IP:Port。
func (t Target) AddrPort() (netip.AddrPort, bool) {
	addr, ok := t.Addr()
	if !ok {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr, uint16(t.Port)), true
}

// Network 返回拨号用的网络类型（"tcp4" / "tcp6"）。
//
// 供测量层使用：显式区分 tcp4 / tcp6 可以避免 IPv6 目标被
// 系统解析成 IPv4，也让"IPv4 与 IPv6 分开测量"成为可能。
func (t Target) Network() string {
	switch t.ipVersionByAddr() {
	case IPVersionIPv4:
		return "tcp4"
	case IPVersionIPv6:
		return "tcp6"
	default:
		return "tcp"
	}
}

// Address 返回可直接传给 net.Dial 的 "host:port" 字符串。
//
// IPv6 会被加上方括号，这是 net 包要求的写法。
func (t Target) Address() string {
	addr, ok := t.Addr()
	if !ok {
		return TargetID(t.IP, t.Port)
	}
	return netip.AddrPortFrom(addr, uint16(t.Port)).String()
}

// String 返回规范 ID，便于日志与测试中使用。
func (t Target) String() string {
	if t.ID != "" {
		return t.ID
	}
	return TargetID(t.IP, t.Port)
}

// Normalize 归一化 Target 的所有字段，并返回是否发生了修改。
//
// 归一化内容：
//   - IP 规范形式（zone / IPv4-mapped）
//   - IPVersion 与 IP 保持一致
//   - ID 由 (IP, Port) 重新生成
//   - Location / Colo 字段的空白与大小写
//
// 返回 true 表示该值不是规范形式。调用方可用它做"数据需要修复"的检测，
// 但不必因为 true 就丢弃数据——Normalize 之后的值一定可用。
func (t *Target) Normalize() bool {
	if t == nil {
		return false
	}
	before := *t

	if addr, ok := ParseAddr(t.IP); ok {
		t.IP = addr.String()
		t.IPVersion = IPVersionOf(addr)
		t.ID = TargetID(t.IP, t.Port)
	} else if t.ID == "" {
		t.ID = TargetID(t.IP, t.Port)
	}
	if !t.IPVersion.Valid() {
		t.IPVersion = t.ipVersionByAddr()
	}

	t.Location.Normalize()
	t.Colo.Normalize()

	return *t != before
}

// Validate 检查 Target 是否满足不变式。
//
// 用于"数据可信"的边界处：解析后、入库前、上传前。
// 返回 error 时用 errors.Is(err, ErrInvalidTarget) 判断。
func (t Target) Validate() error {
	addr, ok := ParseAddr(t.IP)
	if !ok {
		return fmt.Errorf("%w: ip %q is not a valid address", ErrInvalidTarget, t.IP)
	}
	if !ValidPort(t.Port) {
		return fmt.Errorf("%w: port %d out of range 1-65535", ErrInvalidTarget, t.Port)
	}
	if want := TargetID(t.IP, t.Port); t.ID != want {
		return fmt.Errorf("%w: id %q does not match ip:port %q", ErrInvalidTarget, t.ID, want)
	}
	if want := IPVersionOf(addr); t.IPVersion != want {
		return fmt.Errorf("%w: ip_version %q does not match ip %q (%s)", ErrInvalidTarget, t.IPVersion, t.IP, want)
	}
	if err := t.Location.validate(); err != nil {
		return err
	}
	return t.Colo.validate()
}

// Equal 报告两个 Target 是否完全相同（含 Location 与 Colo）。
func (t Target) Equal(other Target) bool {
	return t == other
}

// SameEndpoint 报告两个 Target 是否指向同一个 IP:Port。
//
// 与 Equal 的区别：只比较端点，忽略地理位置等元数据。
// 用于判断"同一目标的元数据是否发生了变化"。
func (t Target) SameEndpoint(other Target) bool {
	return t.ID == other.ID && t.IP == other.IP && t.Port == other.Port
}

// Merge 用 other 补齐 t 中缺失的元数据字段（t 自身已有的字段优先）。
//
// 典型用途：同一个 IP:Port 在两次抓取之间元数据一个有一个没有，
// 或用更完整的来源补齐缓存中较旧的记录。
//
// 只补齐空值，不覆盖已有值，因此不会让数据"变差"。
func (t Target) Merge(other Target) Target {
	out := t

	if out.Location.CCA2 == "" {
		out.Location.CCA2 = other.Location.CCA2
	}
	if out.Location.Country == "" {
		out.Location.Country = other.Location.Country
	}
	if out.Location.Region == "" {
		out.Location.Region = other.Location.Region
	}
	if out.Location.City == "" {
		out.Location.City = other.Location.City
	}
	if out.Location.CountryEN == "" {
		out.Location.CountryEN = other.Location.CountryEN
	}
	if !out.Location.HasCoordinates && other.Location.HasCoordinates {
		out.Location.Latitude = other.Location.Latitude
		out.Location.Longitude = other.Location.Longitude
		out.Location.HasCoordinates = true
	}

	if out.Colo.IATA == "" {
		out.Colo.IATA = other.Colo.IATA
	}
	if out.Colo.CCA2 == "" {
		out.Colo.CCA2 = other.Colo.CCA2
	}
	if out.Colo.Region == "" {
		out.Colo.Region = other.Colo.Region
	}
	if out.Colo.City == "" {
		out.Colo.City = other.Colo.City
	}
	if !out.Colo.HasCoordinates && other.Colo.HasCoordinates {
		out.Colo.Latitude = other.Colo.Latitude
		out.Colo.Longitude = other.Colo.Longitude
		out.Colo.HasCoordinates = true
	}

	return out
}

// ipVersionByAddr 根据 IP 字段推断版本，无法解析时返回 IPVersionUnknown。
func (t Target) ipVersionByAddr() IPVersion {
	addr, ok := ParseAddr(t.IP)
	if !ok {
		return IPVersionUnknown
	}
	return IPVersionOf(addr)
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

// ---------------------------------------------------------------------------
// 字段校验辅助
// ---------------------------------------------------------------------------

// validate 检查 Location 的字段格式。
func (l Location) validate() error {
	if err := validateCountryCode("location.country", l.Country); err != nil {
		return err
	}
	if err := validateCountryCode("location.cca2", l.CCA2); err != nil {
		return err
	}
	if !l.HasCoordinates && (l.Latitude != 0 || l.Longitude != 0) {
		return fmt.Errorf("%w: location has coordinates but has_coordinates is false", ErrInvalidTarget)
	}
	if l.HasCoordinates && !coordinatesInRange(l.Latitude, l.Longitude) {
		return fmt.Errorf("%w: location coordinates out of range (lat=%v lon=%v)",
			ErrInvalidTarget, l.Latitude, l.Longitude)
	}
	return nil
}

// validate 检查 ColoInfo 的字段格式。
func (c ColoInfo) validate() error {
	if err := validateCountryCode("colo.cca2", c.CCA2); err != nil {
		return err
	}
	if c.IATA != "" && (len(c.IATA) != 3 || !isUpperAlpha(c.IATA)) {
		return fmt.Errorf("%w: colo.iata %q must be 3 uppercase letters", ErrInvalidTarget, c.IATA)
	}
	if !c.HasCoordinates && (c.Latitude != 0 || c.Longitude != 0) {
		return fmt.Errorf("%w: colo has coordinates but has_coordinates is false", ErrInvalidTarget)
	}
	if c.HasCoordinates && !coordinatesInRange(c.Latitude, c.Longitude) {
		return fmt.Errorf("%w: colo coordinates out of range (lat=%v lon=%v)",
			ErrInvalidTarget, c.Latitude, c.Longitude)
	}
	return nil
}

// validateCountryCode 校验国家/地区代码：允许为空，否则必须是 2 位大写字母。
//
// 刻意不做"是否真实存在的国家代码"校验：上游可能使用非 ISO 取值
// （例如 EU 这类特殊代码），宁可接受也不误杀数据。
func validateCountryCode(field, code string) error {
	if code == "" {
		return nil
	}
	if len(code) != 2 || !isUpperAlpha(code) {
		return fmt.Errorf("%w: %s %q must be 2 uppercase letters", ErrInvalidTarget, field, code)
	}
	return nil
}

// isUpperAlpha 报告字符串是否全部由大写 ASCII 字母组成。
func isUpperAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return len(s) > 0
}

// normalizeCode 归一化代码字段：去空白 + 转大写。
func normalizeCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// coordinatesInRange 报告坐标是否都在合法范围内。
func coordinatesInRange(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CollectorProfile 描述**测量者**所在的位置与运营商。
//
// 它与 Target.Location / Target.Colo 完全无关：
//
//	Target.Location   测的是"这个目标 IP 在哪"
//	Target.Colo       测的是"这个目标从哪个接入点进来"
//	CollectorProfile  记的是"我在哪、我用哪家运营商"
//
// 该结构被刻意限制为"粗粒度、不可定位到个人"的信息：
// 国家、省份、城市、运营商、ASN、IP 版本。
//
// 禁止在此结构中增加（见需求第 10 / 15 条）：
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

// Normalize 归一化字段并返回是否发生了修改。
//
// 归一化规则：
//   - Country：去空白 + 转大写（ISO 3166-1 alpha-2 习惯写法）；
//   - ASN：统一成 "AS<数字>" 形式（用户可能写 "9808" 或 "as9808"）；
//   - Province / City / ISP：只去首尾空白（保留大小写，因为上游与用户
//     的写法不统一，强行改大小写反而会破坏可读性）；
//   - IPVersion：去空白 + 转小写。
func (c *CollectorProfile) Normalize() bool {
	if c == nil {
		return false
	}
	before := *c

	c.Country = normalizeCode(c.Country)
	c.Province = strings.TrimSpace(c.Province)
	c.City = strings.TrimSpace(c.City)
	c.ISP = strings.TrimSpace(c.ISP)
	c.ASN = NormalizeASN(c.ASN)
	c.IPVersion = IPVersion(strings.ToLower(strings.TrimSpace(string(c.IPVersion))))

	return *c != before
}

// Validate 检查 Profile 是否可用。
//
// 只校验"格式"，不校验"是否填全"：
// Phase 6 允许只填部分字段（自动检测 + 手动覆盖的混合场景），
// 因此空字段是合法的，由收集者自己承担分组精度的后果。
func (c CollectorProfile) Validate() error {
	if c.Country != "" {
		if len(c.Country) != 2 || !isUpperAlpha(c.Country) {
			return fmt.Errorf("%w: collector.country %q must be 2 uppercase letters", ErrInvalidProfile, c.Country)
		}
	}
	if c.ASN != "" {
		if !isASN(c.ASN) {
			return fmt.Errorf("%w: collector.asn %q must look like AS9808 or 9808", ErrInvalidProfile, c.ASN)
		}
	}
	if c.IPVersion != "" && !c.IPVersion.Valid() {
		return fmt.Errorf("%w: collector.ip_version %q must be one of ipv4/ipv6/unknown", ErrInvalidProfile, c.IPVersion)
	}
	return nil
}

// ErrInvalidProfile 表示采集者信息不满足格式要求。
var ErrInvalidProfile = errors.New("invalid collector profile")

// GroupKey 返回用于聚合分组的稳定键。
//
// 形如 "CN|Zhejiang|Hangzhou|China Mobile|AS9808|ipv4"：
// 固定分隔符 + 固定字段顺序，保证相同分组在任何时间、任何采集者
// 都生成完全一致的键，便于聚合与去重。
//
// 注意：调用方应当先 Normalize，否则 "CN" 与 "cn" 会形成两个分组。
func (c CollectorProfile) GroupKey() string {
	return strings.Join([]string{
		c.Country,
		c.Province,
		c.City,
		c.ISP,
		c.ASN,
		string(c.IPVersion),
	}, "|")
}

// IsAnonymous 报告该 Profile 是否不含任何可定位到个人的信息。
//
// 永远返回 true：CollectorProfile 的字段类型决定了它无法承载
// 公网 IP、MAC、主机名等隐私数据。这个函数存在的意义是把
// "隐私约束"写成可测试的断言，而不是只写在注释里
// （见 model_test.go 中对应的测试）。
func (c CollectorProfile) IsAnonymous() bool {
	return true
}

// NormalizeASN 把 ASN 归一化成 "AS<数字>" 形式。
//
// 接受 "9808" / "as9808" / "AS9808" / " as9808 "，统一输出 "AS9808"。
// 无法识别的内容只做去空白处理并保留原样，
// 因为"看不懂"不等于"要丢掉用户填的信息"。
func NormalizeASN(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	digits := s
	upper := strings.ToUpper(s)
	if strings.HasPrefix(upper, "AS") {
		digits = s[2:]
	}
	digits = strings.TrimSpace(digits)

	if !isAllDigits(digits) {
		return s
	}
	// 去掉前导零（"AS09808" -> "AS9808"），但保留单个 0（原样返回）。
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "AS0"
	}
	return "AS" + trimmed
}

// isASN 报告字符串是否是归一化后的 ASN 形式（"AS<数字>"）。
func isASN(s string) bool {
	if len(s) < 3 || !strings.HasPrefix(s, "AS") {
		return false
	}
	return isAllDigits(s[2:])
}

// isAllDigits 报告字符串是否非空且全部由 ASCII 数字组成。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// collector_id
// ---------------------------------------------------------------------------

// collectorIDBytes 是随机 collector_id 的字节数（16 字节 = 128 位）。
const collectorIDBytes = 16

// collectorIDPrefix 是 collector_id 的前缀，便于在数据里一眼识别。
const collectorIDPrefix = "c-"

// NewCollectorID 生成一个匿名 collector_id。
//
// 关键约束（需求第 10 条）：
//
//	collector_id 必须随机生成，**不能**由硬件信息（MAC、CPU、磁盘序列号）、
//	公网 IP、主机名等可识别信息推导出来，否则它就变成了设备指纹。
//
// 因此这里使用 crypto/rand 生成 128 位随机值并做十六进制编码。
// 它的唯一用途是"把同一个匿名节点的历史数据关联起来"：
// 换机器、重装系统、删除本地数据库后都会得到新的 ID，
// 这是可接受的代价，因为我们不追求跨设备追踪。
//
// 读取随机源失败时返回 error（不降级到可预测的伪随机），
// 由调用方决定是否终止——宁可失败也不要生成弱 ID。
func NewCollectorID() (string, error) {
	buf := make([]byte, collectorIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate collector_id: %w", err)
	}
	return collectorIDPrefix + hex.EncodeToString(buf), nil
}

// ValidCollectorID 报告 collector_id 是否符合本项目的格式。
func ValidCollectorID(id string) bool {
	if !strings.HasPrefix(id, collectorIDPrefix) {
		return false
	}
	body := id[len(collectorIDPrefix):]
	if len(body) != collectorIDBytes*2 {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

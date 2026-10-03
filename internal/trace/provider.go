package trace

import (
	"fmt"
	"sort"
	"strings"
)

// 本文件定义两个"源"选项：
//
//	DataProvider —— 每一跳的 ASN / 运营商 / 地理位置从哪儿来
//	PowProvider  —— NextTrace 自家 API 的 PoW 令牌从哪儿取
//
// 为什么把它们做成受校验的类型而不是直接透传字符串：
// 数据源名字写错时，NextTrace 的行为是**换一个源或忽略**，
// 而不是报错。那意味着"我以为在用 IPInfo，其实一直在用别的"
// ——这种错误从结果上看不出来，只能靠构造时校验挡住。
//
// 可选值的清单来自本地 nexttrace 的 `--help`（v1.7.3）。

// DataProvider 是 GeoIP 数据源。
type DataProvider string

const (
	// ProviderNextTraceAPI 是 NextTrace 自建 API（默认）。
	//
	// 它对每跳做 ASN / 运营商 / 地理查询，是**唯一**一个
	// 与 PoW 令牌机制绑定的源：令牌拿不到就整体失败。
	ProviderNextTraceAPI DataProvider = "NextTrace-API"

	// ProviderIPInfo 使用 ipinfo.io。
	ProviderIPInfo DataProvider = "IPInfo"

	// ProviderIPSB 使用 ip.sb。
	ProviderIPSB DataProvider = "IP.SB"

	// ProviderIPInsight 使用 ipinsight。
	ProviderIPInsight DataProvider = "IPInsight"

	// ProviderIPAPI 使用 ip-api.com。
	ProviderIPAPI DataProvider = "IP-API.com"

	// ProviderIPInfoLocal 使用本地 IP 库（不走网络）。
	ProviderIPInfoLocal DataProvider = "IPInfoLocal"

	// ProviderIPDBOne 使用 ipdb.one。
	ProviderIPDBOne DataProvider = "ipdb.one"

	// ProviderChunzhen 使用纯真 IP 库（国内常用）。
	ProviderChunzhen DataProvider = "chunzhen"

	// ProviderDisabled 不做 GeoIP 查询。
	//
	// 用途：只要路径不要 ASN/地区时，用它避开所有外部依赖，
	// 跟踪速度也最快。代价是 `as_path` 与落地地区会是空的。
	ProviderDisabled DataProvider = "disable-geoip"
)

// DefaultDataProvider 是默认数据源。
//
// 与 NextTrace 的默认一致（NextTrace-API），因为它在 ASN 覆盖
// 与线路命名上最完整。想避开 PoW 限流时改用 IPInfo 等。
const DefaultDataProvider = ProviderNextTraceAPI

// dataProviderAliases 把常见写法映射到规范值。
//
// 大小写不敏感已经由 Normalize 处理，这里处理的是**名字本身**
// 的几种叫法：使用者会照着讲"leomoe"或"ntapi"，而不一定记得
// 帮助里那串带点的写法。
var dataProviderAliases = map[string]DataProvider{
	"nexttrace-api": ProviderNextTraceAPI,
	"nexttraceapi":  ProviderNextTraceAPI,
	"leomoeapi":     ProviderNextTraceAPI,
	"leomoe":        ProviderNextTraceAPI,
	"ntapi":         ProviderNextTraceAPI,
	"ip.sb":         ProviderIPSB,
	"ipsb":          ProviderIPSB,
	"ipinfo":        ProviderIPInfo,
	"ipinfo.io":     ProviderIPInfo,
	"ipinsight":     ProviderIPInsight,
	"ip-api.com":    ProviderIPAPI,
	"ipapi":         ProviderIPAPI,
	"ipapicom":      ProviderIPAPI,
	"ipinfolocal":   ProviderIPInfoLocal,
	"local":         ProviderIPInfoLocal,
	"ipdb.one":      ProviderIPDBOne,
	"ipdbone":       ProviderIPDBOne,
	"chunzhen":      ProviderChunzhen,
	"disable-geoip": ProviderDisabled,
	"disable":       ProviderDisabled,
	"none":          ProviderDisabled,
	"off":           ProviderDisabled,
}

// DataProviders 返回全部可选数据源（按 nexttrace --help 的顺序）。
//
// 给 --help 与图形界面下拉框用：手工维护一份列表会与这里的
// 常量漂移，而"选项在帮助里但不可用"是最烦人的那类问题。
func DataProviders() []DataProvider {
	return []DataProvider{
		ProviderNextTraceAPI,
		ProviderIPInfo,
		ProviderIPSB,
		ProviderIPInsight,
		ProviderIPAPI,
		ProviderIPInfoLocal,
		ProviderIPDBOne,
		ProviderChunzhen,
		ProviderDisabled,
	}
}

// Valid 报告数据源是否受支持。
func (p DataProvider) Valid() bool {
	normalized, err := p.Normalize()
	return err == nil && normalized == p
}

// Normalize 把用户输入规范化成受支持的数据源。
//
// 空字符串表示"用默认值"。
func (p DataProvider) Normalize() (DataProvider, error) {
	raw := strings.ToLower(strings.TrimSpace(string(p)))
	if raw == "" {
		return DefaultDataProvider, nil
	}
	if provider, ok := dataProviderAliases[raw]; ok {
		return provider, nil
	}
	return "", fmt.Errorf("unknown data provider %q (choose one of %s)",
		string(p), strings.Join(providerNames(), ", "))
}

// IsZero 报告是否未指定（用默认值）。
func (p DataProvider) IsZero() bool {
	return strings.TrimSpace(string(p)) == ""
}

// NeedsGeoIP 报告该数据源是否会做外部 GeoIP 查询。
//
// 用途：disable-geoip 时 `as_path` 与落地地区必然为空，
// 调用方可以据此给出解释，而不是让人以为"跟踪坏了"。
func (p DataProvider) NeedsGeoIP() bool {
	normalized, err := p.Normalize()
	if err != nil {
		return true
	}
	return normalized != ProviderDisabled
}

// providerNames 返回规范名的有序列表（用于错误信息）。
func providerNames() []string {
	providers := DataProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, string(provider))
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// PoW 令牌来源
// ---------------------------------------------------------------------------

// PowProvider 是 NextTrace API v3 的 PoW 令牌来源。
//
// 为什么需要可选：NextTrace 的帮助里写明"For China mainland users,
// please use sakura"——默认的 api.nxtrace.org 对中国大陆用户容易
// 限流（实测遇到 `RetToken failed ... too many requests`），
// 而令牌拿不到会让整次跟踪失败，不只是丢掉 ASN。
//
// 只在数据源为 NextTrace-API 时才有意义。
type PowProvider string

const (
	// PowDefault 是 NextTrace 默认的 PoW 源。
	PowDefault PowProvider = "api.nxtrace.org"

	// PowSakura 是面向中国大陆用户的 PoW 源。
	PowSakura PowProvider = "sakura"
)

// DefaultPowProvider 表示不显式指定，交给 NextTrace 自己决定。
//
// 刻意留空而不是写死 api.nxtrace.org：那样会把上游的默认值
// 固化进本项目，上游改了我们也跟着"改"成旧的。
const DefaultPowProvider PowProvider = ""

// PowProviders 返回全部可选 PoW 源。
func PowProviders() []PowProvider {
	return []PowProvider{PowDefault, PowSakura}
}

// Valid 报告 PoW 源是否受支持。
func (p PowProvider) Valid() bool {
	_, err := p.Normalize()
	return err == nil
}

// Normalize 把用户输入规范化成受支持的 PoW 源。
//
// 空字符串表示"不显式指定"。
func (p PowProvider) Normalize() (PowProvider, error) {
	raw := strings.ToLower(strings.TrimSpace(string(p)))
	switch raw {
	case "":
		return DefaultPowProvider, nil
	case "api.nxtrace.org", "nxtrace", "default":
		return PowDefault, nil
	case "sakura":
		return PowSakura, nil
	default:
		return "", fmt.Errorf("unknown pow provider %q (choose one of %s)",
			string(p), joinPowNames())
	}
}

// IsZero 报告是否未指定。
func (p PowProvider) IsZero() bool {
	return strings.TrimSpace(string(p)) == ""
}

// joinPowNames 返回规范名的列表（用于错误信息）。
func joinPowNames() string {
	providers := PowProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, string(provider))
	}
	return strings.Join(names, ", ")
}

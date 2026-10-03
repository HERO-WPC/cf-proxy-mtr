package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// geo-IP 源：会联系外部服务
// ---------------------------------------------------------------------------

// 默认参数与端点。
const (
	// DefaultGeoIPEndpoint 是默认的 geo-IP 服务。
	//
	// 选择它是因为它一个响应就包含国家 / 地区 / 城市 / ISP / ASN /
	// IP 版本，字段最全；而且是可替换的——用户完全可以用
	// --geoip-endpoint 指向自己信任的服务。
	//
	// 注意：它在免费额度下只提供 HTTP。因此这里**没有**假定它一定是
	// HTTPS，而是把安全性交给用户选择（可以在报告里看到实际端点）。
	DefaultGeoIPEndpoint = "http://ip-api.com/json/?fields=status,message,country,countryCode,regionName,city,isp,org,as,query"

	// geoIPMaxBody 限制响应体大小，避免异常服务撑爆内存。
	geoIPMaxBody = 64 << 10

	// geoIPMaxFields 是递归查找字段时的最大深度。
	geoIPMaxFields = 12
)

// GeoIPSource 通过公开的 geo-IP API 查询本机出口 IP 的位置与运营商。
//
// 隐私说明（必须对用户如实交代，见 Describe）：
//
//	这个源按**定义**需要服务端看到请求来源，否则无从判断"你在哪"。
//	因此它会把本机的公网 IP 暴露给所配置的服务。
//	ExposesLocalIP 返回 true，CLI 会在发起请求前打印这一点，
//	用户可以在那一刻中止。
//
// 端点是可配置的：用户应当选择一个自己信任的服务，
// 而不是被程序硬编码绑死在某一家上。
type GeoIPSource struct {
	endpoint string
	client   *http.Client
}

// NewGeoIPSource 创建 geo-IP 源。
//
// client 为 nil 时使用内置客户端（带超时与响应体上限）。
func NewGeoIPSource(endpoint string, client *http.Client) *GeoIPSource {
	if client == nil {
		client = &http.Client{
			Timeout: DefaultTimeout,
			// 不跟随重定向到其它域名：端点由用户指定，
			// 悄悄换到别处去发请求不是我们该做的事。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		}
	}
	return &GeoIPSource{
		endpoint: strings.TrimSpace(endpoint),
		client:   client,
	}
}

// Name 实现 Source。
func (s *GeoIPSource) Name() string { return "geoip" }

// Describe 实现 Source。
func (s *GeoIPSource) Describe() string {
	return fmt.Sprintf("queries %s for the public IP, country, region, city, ISP and ASN", s.endpoint)
}

// ExposesLocalIP 实现 Source。
func (s *GeoIPSource) ExposesLocalIP() bool { return true }

// Endpoint 返回配置的端点，供 CLI 展示"实际会请求哪里"。
func (s *GeoIPSource) Endpoint() string { return s.endpoint }

// Detect 实现 Source。
func (s *GeoIPSource) Detect(ctx context.Context) (Values, error) {
	if s.endpoint == "" {
		return Values{}, fmt.Errorf("no geo-IP endpoint configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return Values{}, fmt.Errorf("build geo-IP request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cf-route-tester")

	resp, err := s.client.Do(req)
	if err != nil {
		return Values{}, fmt.Errorf("geo-IP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, geoIPMaxBody))
	if err != nil {
		return Values{}, fmt.Errorf("read geo-IP response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Values{}, fmt.Errorf("geo-IP service returned http %d", resp.StatusCode)
	}

	values, err := parseGeoIP(body)
	if err != nil {
		return Values{}, err
	}
	if values.IsZero() {
		return Values{}, fmt.Errorf("geo-IP service returned no usable fields")
	}
	return values, nil
}

// ---------------------------------------------------------------------------
// 响应解析
// ---------------------------------------------------------------------------

// parseGeoIP 从 geo-IP 响应里尽力提取字段。
//
// 为什么写得这么宽松：这个端点是可以被用户替换的，不同服务的
// JSON 形状差别很大（有的把国家放在顶层 country，有的放在
// country_code；ipinfo 还把城市嵌在 location 对象里）。
// 与其为每个服务写一个解析器，不如按"键名同义词 + 有限递归"来找。
//
// 但宽松不等于瞎猜：找不到就是找不到，不会用别的字段硬凑。
func parseGeoIP(body []byte) (Values, error) {
	body = trimBOM(body)
	if len(body) == 0 {
		return Values{}, fmt.Errorf("empty geo-IP response")
	}

	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return Values{}, fmt.Errorf("geo-IP response is not a json object: %w", err)
	}

	// 有些服务用 status 表达失败（例如 ip-api 的 status=fail + message）。
	if status := lookupString(document, "status", 0); strings.EqualFold(status, "fail") {
		message := lookupString(document, "message", 0)
		if message == "" {
			message = "service reported failure"
		}
		return Values{}, fmt.Errorf("geo-IP service error: %s", message)
	}

	var out Values

	// 国家：优先两字母代码（我们的模型要求 2 位大写）。
	out.Country = firstNonEmpty(
		lookupString(document, "countryCode", 0),
		lookupString(document, "country_code", 0),
		lookupString(document, "countryCode2", 0),
		lookupString(document, "country", 0),
		lookupString(document, "country_name", 0),
	)

	out.Province = firstNonEmpty(
		lookupString(document, "regionName", 0),
		lookupString(document, "region", 0),
		lookupString(document, "region_name", 0),
		lookupString(document, "province", 0),
		lookupString(document, "state", 0),
	)

	out.City = firstNonEmpty(
		lookupString(document, "city", 0),
		lookupString(document, "town", 0),
		lookupString(document, "locality", 0),
	)

	// ISP 名称：org 之类的字段常常带 "AS13335 " 前缀，
	// 去掉前缀才是用户认得的运营商名。
	out.ISP = stripASNPrefix(firstNonEmpty(
		lookupString(document, "isp", 0),
		lookupString(document, "org", 0),
		lookupString(document, "organization", 0),
		lookupString(document, "asname", 0),
		lookupString(document, "as_name", 0),
	))

	// ASN：服务可能给 "AS9808 China Mobile"、纯数字 9808、
	// 或 "AS9808"。三种都要能处理，且必须归一化成 "AS<数字>"。
	//
	// org 也作为候选：ipinfo 这类服务不单独给 asn，
	// 而是把 ASN 写在 org 开头（"AS4713 NTT Communications"）。
	// 这是常见的字段形态，不是猜测——只有能明确提取出 ASN 时才采用。
	//
	// 最后仍要校验归一化结果确实是 "AS<数字>"：
	// 否则宁可留空，也不要写进 "ASCMCC" 这种值——那会污染分组。
	out.ASN = extractASN(
		lookupString(document, "as", 0),
		lookupString(document, "asn", 0),
		lookupString(document, "autonomous_system_number", 0),
		lookupString(document, "org", 0),
		lookupString(document, "organization", 0),
	)
	if !looksLikeASN(out.ASN) {
		out.ASN = ""
	}

	// IP 版本从返回的公网 IP 推断，而不是信任服务自报的字段。
	if ip := firstNonEmpty(
		lookupString(document, "query", 0),
		lookupString(document, "ip", 0),
		lookupString(document, "ipAddress", 0),
	); ip != "" {
		if addr, ok := model.ParseAddr(ip); ok {
			out.IPVersion = string(model.IPVersionOf(addr))
		}
	}

	// 国家代码必须是 2 位大写，否则宁可留空：
	// 把 "United States" 塞进 country 会让后续分组出现两套写法。
	country := strings.ToUpper(strings.TrimSpace(out.Country))
	if len(country) != 2 {
		// 有些服务只给全称（"United States"）。这种情况我们没有
		// 权威映射表，因此留空而不是猜。
		country = ""
	}
	out.Country = country

	// 一个字段都没解析出来时明确报错，而不是返回一个静默的空结果：
	// 空结果会让上层以为"检测成功但没数据"，掩盖了端点配置错误。
	if out.IsZero() {
		return Values{}, fmt.Errorf("geo-IP response contained none of the expected fields")
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// ASN 与 ISP 名清理
// ---------------------------------------------------------------------------

// looksLikeASN 报告值是否是归一化后的 ASN 形式（"AS<数字>"）。
func looksLikeASN(s string) bool {
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

// extractASN 从候选值里提取 ASN。
//
// 支持的输入形态（都实测见过）：
//
//	"AS13335"                    纯 ASN
//	"AS13335 Cloudflare, Inc."   带运营商名
//	"AS4713 NTT Communications"  带运营商名
//	9808                         纯数字（JSON number）
//
// 只保留 "AS<数字>" 部分，其余丢掉。全部候选都提取不出来时返回空。
func extractASN(candidates ...string) string {
	for _, candidate := range candidates {
		if asn := normalizeASNToken(candidate); asn != "" {
			return asn
		}
	}
	return ""
}

// normalizeASNToken 从单个字符串里提取 "AS<数字>"。
func normalizeASNToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// 先按空格切：ASN 总是在第一段（例如 "AS13335 Cloudflare, Inc."）。
	token := strings.Fields(s)[0]

	// 纯数字：补上 AS 前缀。
	if isAllDigits(token) {
		if asn := model.NormalizeASN(token); looksLikeASN(asn) {
			return asn
		}
		return ""
	}

	// "AS13335" / "as13335"。
	upper := strings.ToUpper(token)
	if strings.HasPrefix(upper, "AS") && looksLikeASN(upper) {
		// 去掉前导零（"AS09808" -> "AS9808"）。
		return model.NormalizeASN(upper)
	}
	return ""
}

// stripASNPrefix 去掉 ISP 名开头的 "AS<数字> " 前缀。
//
// 例："AS4713 NTT Communications" -> "NTT Communications"。
//
// 为什么值得做：运营商名字会进入按运营商分组的键。
// 留着 "AS4713 " 前缀会让同一个运营商在不同数据源下
// 产生两个分组，聚合结果就分不清了。
func stripASNPrefix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	fields := strings.Fields(s)
	if len(fields) < 2 {
		return s
	}
	if normalizeASNToken(fields[0]) == "" {
		return s
	}
	return strings.TrimSpace(strings.Join(fields[1:], " "))
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

// lookupString 在文档里按键名找字符串值，支持有限深度的递归。
//
// 递归是为了兼容 ipinfo 这类把字段嵌在 location 对象里的服务；
// 深度上限避免畸形响应导致无限递归或遍历巨大结构。
func lookupString(document map[string]any, key string, depth int) string {
	if document == nil || depth > geoIPMaxFields {
		return ""
	}

	// 先看当前层。
	if value, ok := document[key]; ok {
		if s := scalarToString(value); s != "" {
			return s
		}
	}

	// 再看子对象。
	for _, value := range document {
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if s := lookupString(nested, key, depth+1); s != "" {
			return s
		}
	}
	return ""
}

// scalarToString 把 JSON 标量转成字符串（数字也接受，例如 asn: 9808）。
func scalarToString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		// JSON 数字：ASN 可能是 9808 这样的整数。
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return strings.TrimSpace(fmt.Sprintf("%g", v))
	case bool:
		return ""
	default:
		return ""
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// trimBOM 去掉 UTF-8 BOM。
//
// 少数服务会在响应开头带上 BOM，不去掉的话 json 解析会直接失败。
func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

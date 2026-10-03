package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// fakeSource 是一个可编程的检测源。
type fakeSource struct {
	name     string
	values   Values
	err      error
	exposes  bool
	delay    time.Duration
	describe string

	// calls 记录被调用的次数。
	calls int
}

func (f *fakeSource) Name() string         { return f.name }
func (f *fakeSource) ExposesLocalIP() bool { return f.exposes }
func (f *fakeSource) Describe() string {
	if f.describe == "" {
		return "fake source " + f.name
	}
	return f.describe
}

func (f *fakeSource) Detect(ctx context.Context) (Values, error) {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Values{}, ctx.Err()
		}
	}
	if f.err != nil {
		return Values{}, f.err
	}
	return f.values, nil
}

// ---------------------------------------------------------------------------
// 合并规则
// ---------------------------------------------------------------------------

// TestDetectManualWinsOverSources 是需求第 22 条的核心测试。
//
// 手动配置的值永远优先于自动检测：用户明确写下的东西不该被覆盖。
func TestDetectManualWinsOverSources(t *testing.T) {
	first := &fakeSource{
		name:   "first",
		values: Values{Country: "US", Province: "California", City: "San Jose", ISP: "SomeCloud", ASN: "AS13335"},
	}
	second := &fakeSource{
		name:   "second",
		values: Values{Country: "DE", Province: "Hesse", City: "Frankfurt", ISP: "OtherISP", ASN: "AS24940"},
	}

	manual := Values{
		Country: "CN",
		ISP:     "China Mobile", // 用户知道自己用的是哪家
	}

	result, err := Detect(context.Background(), Options{
		Sources: []Source{first, second},
		Manual:  manual,
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// 手动给的字段必须保持原值，并被标记为 manual。
	if result.Values.Country != "CN" {
		t.Errorf("Country = %q, want CN (manual wins)", result.Values.Country)
	}
	if result.FieldSources[FieldCountry] != "manual" {
		t.Errorf("FieldSources[country] = %q, want manual", result.FieldSources[FieldCountry])
	}
	if result.Values.ISP != "China Mobile" {
		t.Errorf("ISP = %q, want China Mobile (manual wins)", result.Values.ISP)
	}

	// 其余字段由第一个源提供（先出现的优先）。
	if result.Values.Province != "California" {
		t.Errorf("Province = %q, want California (first source wins)", result.Values.Province)
	}
	if result.FieldSources[FieldProvince] != "first" {
		t.Errorf("FieldSources[province] = %q, want first", result.FieldSources[FieldProvince])
	}
	if result.Values.ASN != "AS13335" {
		t.Errorf("ASN = %q, want AS13335 (first source wins)", result.Values.ASN)
	}
	// 第二个源什么字段都没提供，因此不应该出现在 field sources 里。
	for field, source := range result.FieldSources {
		if source == "second" {
			t.Errorf("field %s came from the second source, but the first already filled it", field)
		}
	}
}

// TestDetectLaterSourceFillsGaps 验证后续源能补齐前面的空缺。
func TestDetectLaterSourceFillsGaps(t *testing.T) {
	sparse := &fakeSource{
		name:   "sparse",
		values: Values{IPVersion: "ipv4"},
	}
	rich := &fakeSource{
		name:   "rich",
		values: Values{Country: "JP", City: "Tokyo", ISP: "NTT", ASN: "AS4713", IPVersion: "ipv6"},
	}

	result, err := Detect(context.Background(), Options{Sources: []Source{sparse, rich}})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// sparse 提供的字段保持。
	if result.Values.IPVersion != "ipv4" {
		t.Errorf("IPVersion = %q, want ipv4 from the first source", result.Values.IPVersion)
	}
	// rich 补齐了剩下的。
	if result.Values.Country != "JP" || result.Values.City != "Tokyo" {
		t.Errorf("values = %+v, want the gaps filled by the second source", result.Values)
	}
	if result.FieldSources[FieldCity] != "rich" {
		t.Errorf("FieldSources[city] = %q, want rich", result.FieldSources[FieldCity])
	}
}

// TestDetectSourceFailureDoesNotAffectOthers 验证单源失败不影响整体。
func TestDetectSourceFailureDoesNotAffectOthers(t *testing.T) {
	broken := &fakeSource{name: "broken", err: errors.New("service down")}
	working := &fakeSource{name: "working", values: Values{Country: "SG", City: "Singapore"}}

	result, err := Detect(context.Background(), Options{Sources: []Source{broken, working}})
	if err != nil {
		t.Fatalf("Detect returned error for a single failing source: %v", err)
	}

	if result.Values.Country != "SG" {
		t.Errorf("Country = %q, want SG from the working source", result.Values.Country)
	}
	if len(result.Reports) != 2 {
		t.Fatalf("reports = %d, want 2", len(result.Reports))
	}
	if result.Reports[0].Err == nil {
		t.Error("first report should carry the failure")
	}
	if result.Reports[1].Err != nil {
		t.Errorf("second report should succeed, got %v", result.Reports[1].Err)
	}
	if !result.Reports[1].Succeeded() {
		t.Error("Succeeded() = false for a successful source")
	}
}

// TestDetectSourceTimeoutIsReportedReadably 验证超时被转换成可读信息。
func TestDetectSourceTimeoutIsReportedReadably(t *testing.T) {
	slow := &fakeSource{name: "slow", delay: time.Hour}

	result, err := Detect(context.Background(), Options{
		Sources: []Source{slow},
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	report := result.Reports[0]
	if report.Err == nil {
		t.Fatal("expected a timeout error")
	}
	// 不该把 "context deadline exceeded" 原样丢给用户。
	if strings.Contains(report.Err.Error(), "context deadline") {
		t.Errorf("error = %q, want a human-readable timeout message", report.Err)
	}
	if !strings.Contains(report.Err.Error(), "timed out") {
		t.Errorf("error = %q, want 'timed out'", report.Err)
	}
}

func TestDetectRequiresAtLeastOneSource(t *testing.T) {
	if _, err := Detect(context.Background(), Options{}); !errors.Is(err, ErrNoSource) {
		t.Errorf("Detect() error = %v, want ErrNoSource", err)
	}
}

// ---------------------------------------------------------------------------
// 隐私与透明度
// ---------------------------------------------------------------------------

// TestLocalSourceNeverExposesIP 是本阶段最重要的隐私断言。
func TestLocalSourceNeverExposesIP(t *testing.T) {
	source := NewLocalSource()

	if source.ExposesLocalIP() {
		t.Error("local source claims to expose the local IP; it must not")
	}
	if !strings.Contains(source.Describe(), "contacts no external service") {
		t.Errorf("Description = %q, want an explicit statement that it contacts nothing", source.Describe())
	}
}

// TestGeoIPSourceDeclaresExposure 验证会联网的源如实声明自己。
//
// 地理定位在原理上必须让服务端看到请求来源。这不是可以含糊过去的事：
// 用户要能在命令行上看到这句话。
func TestGeoIPSourceDeclaresExposure(t *testing.T) {
	source := NewGeoIPSource("https://example.test/geo", nil)

	if !source.ExposesLocalIP() {
		t.Error("geo-IP source must declare that it exposes the local IP")
	}
	if !strings.Contains(source.Describe(), "example.test") {
		t.Errorf("Description = %q, want the actual endpoint included", source.Describe())
	}
}

// TestExposureNoteNamesTheSources 验证在发起请求前能说清"谁会看到你的 IP"。
func TestExposureNoteNamesTheSources(t *testing.T) {
	local := &fakeSource{name: "local"}
	remote := &fakeSource{name: "geoip", exposes: true}

	t.Run("only local", func(t *testing.T) {
		result := &Result{Reports: []SourceReport{
			{Name: local.Name(), Used: true},
		}}
		note := result.ExposureNote()
		if !strings.Contains(note, "none of the enabled sources") {
			t.Errorf("note = %q, want a reassurance for local-only sources", note)
		}
	})

	t.Run("with exposing source", func(t *testing.T) {
		result := &Result{Reports: []SourceReport{
			{Name: local.Name(), Used: true},
			{Name: remote.Name(), Used: true, ExposesLocalIP: true},
		}}
		note := result.ExposureNote()
		if !strings.Contains(note, "geoip") {
			t.Errorf("note = %q, want the exposing source named", note)
		}
		if !strings.Contains(note, "public IP") {
			t.Errorf("note = %q, want an explicit mention of the public IP", note)
		}
	})

	t.Run("no source used", func(t *testing.T) {
		result := &Result{Reports: []SourceReport{{Name: "x", Used: false}}}
		if note := result.ExposureNote(); !strings.Contains(note, "no detection source") {
			t.Errorf("note = %q, want a no-source message", note)
		}
	})
}

// TestDescribeSourcesMarksEachSource 验证每个源的自述都带上暴露标记。
func TestDescribeSourcesMarksEachSource(t *testing.T) {
	result := &Result{Reports: []SourceReport{
		{Name: "local", Used: true, Description: "local thing"},
		{Name: "geoip", Used: true, Description: "remote thing", ExposesLocalIP: true},
		{Name: "unused", Used: false, Description: "not used"},
	}}

	lines := result.DescribeSources()
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (unused sources are skipped)", len(lines))
	}
	if !strings.Contains(lines[0], "does not contact an external service") {
		t.Errorf("local line = %q, want a non-exposure marker", lines[0])
	}
	if !strings.Contains(lines[1], "sends your public IP") {
		t.Errorf("geoip line = %q, want an exposure marker", lines[1])
	}
}

// TestValuesNeverHoldIdentifyingData 是一条结构性隐私测试。
//
// Values 是检测结果进入配置文件前的唯一形态。它只有 6 个字段，
// 且全部是粗粒度信息——没有 IP、没有 MAC、没有主机名。
// 这个测试会在有人往 Values 里加字段时立刻失败，从而强制 review。
func TestValuesNeverHoldIdentifyingData(t *testing.T) {
	// 字段集合固定：任何新增都必须显式改这里，也就必然经过 review。
	want := []Field{FieldCountry, FieldProvince, FieldCity, FieldISP, FieldASN, FieldIPVersion}
	got := AllFields()
	if len(got) != len(want) {
		t.Fatalf("AllFields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllFields()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 字段名里不允许出现任何指向"设备/地址"的词。
	forbidden := []string{"ip", "mac", "host", "device", "serial", "uuid", "addr"}
	for _, field := range got {
		name := strings.ToLower(string(field))
		// ip_version 是允许的例外：它记录的是协议版本，不是地址。
		if name == string(FieldIPVersion) {
			continue
		}
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("field %q looks like it could carry identifying data", field)
			}
		}
	}
}

// TestValuesSetTruncatesOverlongInput 验证字段长度被限制。
func TestValuesSetTruncatesOverlongInput(t *testing.T) {
	var values Values
	values.Set(FieldISP, strings.Repeat("x", maxFieldLength*2))
	if got := len(values.ISP); got != maxFieldLength {
		t.Errorf("ISP length = %d, want %d", got, maxFieldLength)
	}

	// 空白被去掉。
	values.Set(FieldCity, "   Tokyo  ")
	if values.City != "Tokyo" {
		t.Errorf("City = %q, want trimmed", values.City)
	}
}

// ---------------------------------------------------------------------------
// local 源
// ---------------------------------------------------------------------------

func TestLocalSourceDetectsEgressFamily(t *testing.T) {
	failDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("network unreachable")
	}

	cases := []struct {
		name        string
		v4, v6      bool
		wantVersion string
		wantErr     bool
	}{
		{"ipv4 only", true, false, "ipv4", false},
		{"ipv6 only", false, true, "ipv6", false},
		// 双栈时无法只凭本机判断出口走哪一族：留空比猜错好。
		{"dual stack", true, true, "", false},
		{"neither", false, false, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				if network == "udp4" && tc.v4 {
					return fakeConn{}, nil
				}
				if network == "udp6" && tc.v6 {
					return fakeConn{}, nil
				}
				return failDial(ctx, network, address)
			}

			source := NewLocalSourceWithDialer(dial)
			values, err := source.Detect(context.Background())

			if tc.wantErr {
				if err == nil {
					t.Fatal("Detect succeeded, want error when neither family works")
				}
				return
			}
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if values.IPVersion != tc.wantVersion {
				t.Errorf("IPVersion = %q, want %q", values.IPVersion, tc.wantVersion)
			}
			// 本地源只能给这一个字段：它不猜国家/城市/运营商。
			if values.NonEmpty() > 1 {
				t.Errorf("values = %+v, want at most the IP version", values)
			}
			if values.Country != "" || values.ISP != "" || values.ASN != "" {
				t.Errorf("values = %+v, want no guessed country/ISP/ASN", values)
			}
		})
	}
}

// TestLocalSourceUsesReservedAddresses 验证探测用的是文档用途地址。
//
// 即使某个平台意外发出报文，也只会打在保留地址上，
// 不会在真实网络上留下痕迹。
func TestLocalSourceUsesReservedAddresses(t *testing.T) {
	var seen []string
	source := NewLocalSourceWithDialer(func(ctx context.Context, network, address string) (net.Conn, error) {
		seen = append(seen, network+" "+address)
		return fakeConn{}, nil
	})

	if _, err := source.Detect(context.Background()); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	joined := strings.Join(seen, "; ")
	if !strings.Contains(joined, "192.0.2.1") {
		t.Errorf("dialed addresses = %q, want the RFC 5737 documentation range", joined)
	}
	if !strings.Contains(joined, "2001:db8::1") {
		t.Errorf("dialed addresses = %q, want the RFC 3849 documentation range", joined)
	}
}

// ---------------------------------------------------------------------------
// geo-IP 源与解析
// ---------------------------------------------------------------------------

func TestGeoIPSourceParsesProviderShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Values
	}{
		{
			name: "ip-api style",
			body: `{"status":"success","country":"United States","countryCode":"US",
			        "regionName":"California","city":"San Jose","isp":"Cloudflare, Inc.",
			        "org":"Cloudflare","as":"AS13335 Cloudflare, Inc.","query":"1.2.3.4"}`,
			want: Values{
				Country: "US", Province: "California", City: "San Jose",
				ISP: "Cloudflare, Inc.", ASN: "AS13335", IPVersion: "ipv4",
			},
		},
		{
			name: "ipinfo style with nested location",
			body: `{"ip":"2001:db8::1","city":"Tokyo","region":"Tokyo","country":"JP",
			        "org":"AS4713 NTT Communications",
			        "location":{"city":"Tokyo","region":"Tokyo","country":"JP"}}`,
			want: Values{
				Country: "JP", Province: "Tokyo", City: "Tokyo",
				// ISP 去掉 "AS4713 " 前缀，ASN 从中提取出来。
				// 留着前缀会让同一个运营商产生两个分组。
				ISP: "NTT Communications", ASN: "AS4713", IPVersion: "ipv6",
			},
		},
		{
			name: "numeric asn without prefix",
			body: `{"country_code":"DE","region_name":"Hesse","city":"Frankfurt",
			        "organization":"Hetzner","asn":24940,"ip":"5.6.7.8"}`,
			want: Values{
				Country: "DE", Province: "Hesse", City: "Frankfurt",
				ISP: "Hetzner", ASN: "AS24940", IPVersion: "ipv4",
			},
		},
		{
			name: "country full name only is dropped",
			body: `{"country":"Japan","city":"Osaka","query":"9.9.9.9"}`,
			want: Values{
				// 没有权威映射表，因此 NULL 国家而不是硬凑。
				Country: "", City: "Osaka", IPVersion: "ipv4",
			},
		},
		{
			name: "missing asn is left empty",
			body: `{"countryCode":"SG","city":"Singapore","query":"8.8.8.8"}`,
			want: Values{Country: "SG", City: "Singapore", IPVersion: "ipv4"},
		},
		{
			name: "bom prefixed response",
			body: "\uFEFF" + `{"countryCode":"NL","city":"Amsterdam","query":"1.1.1.1"}`,
			want: Values{Country: "NL", City: "Amsterdam", IPVersion: "ipv4"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGeoIP([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseGeoIP: %v", err)
			}
			if got != tc.want {
				t.Errorf("parsed = %+v\nwant     %+v", got, tc.want)
			}
		})
	}
}

func TestGeoIPParseErrors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not json":         "hello",
		"json array":       `[1,2,3]`,
		"json string":      `"nope"`,
		"provider failure": `{"status":"fail","message":"reserved range"}`,
		"nothing usable":   `{"foo":"bar"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGeoIP([]byte(body)); err == nil {
				t.Errorf("parseGeoIP(%q) succeeded, want error", body)
			}
		})
	}

	// 服务端失败必须把它的消息带出来，便于排查。
	_, err := parseGeoIP([]byte(`{"status":"fail","message":"quota exceeded"}`))
	if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error = %v, want the provider message included", err)
	}
}

func TestGeoIPSourceAgainstTestServer(t *testing.T) {
	var gotPath, gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","countryCode":"CN","regionName":"Zhejiang",
		                       "city":"Hangzhou","isp":"China Mobile",
		                       "as":"AS9808 China Mobile","query":"1.2.3.4"}`))
	}))
	defer server.Close()

	source := NewGeoIPSource(server.URL+"/geo", nil)
	values, err := source.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	want := Values{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: "ipv4",
	}
	if values != want {
		t.Errorf("values = %+v\nwant     %+v", values, want)
	}
	if gotPath != "/geo" {
		t.Errorf("requested path = %q, want /geo", gotPath)
	}
	if !strings.HasPrefix(gotUserAgent, "cf-route-tester") {
		t.Errorf("user-agent = %q, want cf-route-tester", gotUserAgent)
	}
}

func TestGeoIPSourceHandlesHTTPErrors(t *testing.T) {
	cases := map[string]int{
		"server error": http.StatusInternalServerError,
		"not found":    http.StatusNotFound,
		"rate limited": http.StatusTooManyRequests,
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			source := NewGeoIPSource(server.URL, nil)
			if _, err := source.Detect(context.Background()); err == nil {
				t.Error("Detect succeeded on an HTTP error, want failure")
			}
		})
	}
}

func TestGeoIPSourceWithoutEndpointFails(t *testing.T) {
	source := NewGeoIPSource("", nil)
	if _, err := source.Detect(context.Background()); err == nil {
		t.Error("Detect succeeded without an endpoint")
	}
}

// ---------------------------------------------------------------------------
// 源筛选
// ---------------------------------------------------------------------------

func TestFilterSources(t *testing.T) {
	sources := []Source{
		NewLocalSource(),
		NewGeoIPSource("https://example.test/geo", nil),
	}

	t.Run("no filter keeps all", func(t *testing.T) {
		got, err := FilterSources(sources, nil)
		if err != nil || len(got) != 2 {
			t.Fatalf("got %d sources (err=%v), want 2", len(got), err)
		}
	})

	t.Run("select one", func(t *testing.T) {
		got, err := FilterSources(sources, []string{"local"})
		if err != nil {
			t.Fatalf("FilterSources: %v", err)
		}
		if len(got) != 1 || got[0].Name() != "local" {
			t.Errorf("got %v, want only local", sourceNames(got))
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		got, err := FilterSources(sources, []string{"LOCAL"})
		if err != nil || len(got) != 1 {
			t.Fatalf("got %d sources (err=%v), want 1", len(got), err)
		}
	})

	// 未知名字必须报错：静默忽略会让用户以为某个源被启用了。
	t.Run("unknown name errors", func(t *testing.T) {
		_, err := FilterSources(sources, []string{"nope"})
		if err == nil {
			t.Fatal("FilterSources accepted an unknown name")
		}
		if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "local") {
			t.Errorf("error = %v, want the unknown name and the available ones", err)
		}
	})

	t.Run("multiple unknown names are sorted", func(t *testing.T) {
		_, err := FilterSources(sources, []string{"zeta", "alpha"})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "alpha, zeta") {
			t.Errorf("error = %v, want sorted unknown names for stable output", err)
		}
	})
}

// TestAllSourcesOrdering 验证默认顺序是"先离线、后联网"。
func TestAllSourcesOrdering(t *testing.T) {
	sources := AllSources("https://example.test/geo")
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	if sources[0].Name() != "local" {
		t.Errorf("first source = %q, want local (offline first)", sources[0].Name())
	}
	if sources[1].Name() != "geoip" {
		t.Errorf("second source = %q, want geoip", sources[1].Name())
	}

	// 端点为空的 geo-IP 源不应该被构造出来。
	only := AllSources("")
	if len(only) != 1 || only[0].Name() != "local" {
		t.Errorf("sources with no endpoint = %v, want only local", sourceNames(only))
	}
}

// ---------------------------------------------------------------------------
// Values 转换
// ---------------------------------------------------------------------------

func TestValuesProfileRoundTrip(t *testing.T) {
	profile := model.CollectorProfile{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: model.IPVersionIPv4,
	}

	values := FromProfile(profile)
	back := values.ToProfile()

	if back != profile {
		t.Errorf("round trip changed the profile:\n want %+v\n got  %+v", profile, back)
	}
}

func TestValuesToProfileNormalizes(t *testing.T) {
	values := Values{
		Country:   " cn ",
		Province:  " Zhejiang ",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "9808", // 缺前缀
		IPVersion: "IPv4",
	}

	profile := values.ToProfile()
	if profile.Country != "CN" || profile.ASN != "AS9808" || profile.IPVersion != model.IPVersionIPv4 {
		t.Errorf("profile = %+v, want normalized CN/AS9808/ipv4", profile)
	}
}

func TestValuesGetSetAndNonEmpty(t *testing.T) {
	var values Values
	if values.NonEmpty() != 0 {
		t.Error("zero Values should have no fields")
	}
	if !values.IsZero() {
		t.Error("zero Values should be zero")
	}

	for _, field := range AllFields() {
		values.Set(field, "v-"+string(field))
	}
	if values.NonEmpty() != len(AllFields()) {
		t.Errorf("NonEmpty() = %d, want %d", values.NonEmpty(), len(AllFields()))
	}
	for _, field := range AllFields() {
		if got := values.Get(field); got != "v-"+string(field) {
			t.Errorf("Get(%s) = %q", field, got)
		}
	}
	if values.IsZero() {
		t.Error("filled Values reported as zero")
	}

	// 未知字段不 panic、不写入。
	values.Set(Field("bogus"), "x")
	if values.Get(Field("bogus")) != "" {
		t.Error("unknown field should read back as empty")
	}
}

func TestResultElapsed(t *testing.T) {
	start := time.Now().UTC()
	result := Result{StartedAt: start, FinishedAt: start.Add(1500 * time.Millisecond)}
	if got := result.Elapsed(); got != 1500*time.Millisecond {
		t.Errorf("Elapsed() = %s, want 1.5s", got)
	}
	if got := (Result{}).Elapsed(); got != 0 {
		t.Errorf("Elapsed() = %s, want 0", got)
	}
}

// TestDetectReportsSourceDurations 验证每个源的耗时被记录。
func TestDetectReportsSourceDurations(t *testing.T) {
	source := &fakeSource{name: "slowish", delay: 20 * time.Millisecond, values: Values{Country: "US"}}

	result, err := Detect(context.Background(), Options{Sources: []Source{source}})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if result.Reports[0].Duration < 10*time.Millisecond {
		t.Errorf("Duration = %s, want >= 10ms for a 20ms source", result.Reports[0].Duration)
	}
	if source.calls != 1 {
		t.Errorf("source called %d times, want 1", source.calls)
	}
	if result.Elapsed() <= 0 {
		t.Error("Elapsed() = 0, want positive")
	}
}

// fakeConn 是一个空连接（本地源只关心 dial 是否成功）。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (fakeConn) Write([]byte) (int, error)        { return 0, errors.New("not implemented") }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return "udp" }
func (a fakeAddr) String() string  { return string(a) }

// 保证 json 与 fmt 导入被使用（解析测试里用到）。
var (
	_ = json.Marshal
	_ = fmt.Sprintf
)

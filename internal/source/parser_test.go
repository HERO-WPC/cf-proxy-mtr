package source

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// jsonItem 构造一条 all.json 记录（map 形式，便于逐个用例覆盖字段形状）。
//
// 字段名与真实数据一致；值是真实数据中出现过的形状
// （例如 latitude 是字符串、asn 是数字、port 是数组）。
func jsonItem(ip string, ports []int, meta map[string]any) map[string]any {
	p := make([]any, 0, len(ports))
	for _, port := range ports {
		p = append(p, port)
	}
	return map[string]any{"ip": ip, "port": p, "meta": meta}
}

// realMeta 返回一份与生产数据字段一致的 meta。
func realMeta() map[string]any {
	return map[string]any{
		"hostname":       "speed.cloudflare.com",
		"clientIp":       "159.60.175.146",
		"httpProtocol":   "HTTP/1.1",
		"asn":            35280,
		"asOrganization": "Example Hosting",
		"country":        "US",
		"city":           "Chicago",
		"region":         "Illinois",
		"postalCode":     "60608",
		"latitude":       "41.85003",
		"longitude":      "-87.65005",
		"colo": map[string]any{
			"iata":   "ORD",
			"lat":    41.9786,
			"lon":    -87.9048,
			"cca2":   "US",
			"region": "North America",
			"city":   "Chicago",
		},
		"_port":         443,
		"country_cn":    "美国",
		"country_en":    "United States",
		"country_emoji": "🇺🇸",
		"continent":     "NA",
		"continent_cn":  "北美洲",
		"continent_en":  "North America",
	}
}

// jsonDoc 把记录序列化成 all.json 形状的文档。
func jsonDoc(t *testing.T, items []map[string]any, extra map[string]any) []byte {
	t.Helper()

	root := map[string]any{
		"generated_at": "2026-10-03T03:52:35.771190",
		"list": map[string]any{
			"country": map[string]any{"US": 1, "DE": 2},
			"ips":     len(items),
		},
		"data": items,
	}
	for k, v := range extra {
		root[k] = v
	}

	blob, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return blob
}

func mustParseJSON(t *testing.T, data []byte) *ParseResult {
	t.Helper()
	res, err := ParseJSON(data)
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// JSON 解析：正常路径
// ---------------------------------------------------------------------------

func TestParseJSONBasicTarget(t *testing.T) {
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)

	if len(res.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(res.Targets))
	}
	got := res.Targets[0]

	if got.ID != "1.2.3.4:443" {
		t.Errorf("ID = %q, want 1.2.3.4:443", got.ID)
	}
	if got.IP != "1.2.3.4" || got.Port != 443 {
		t.Errorf("IP/Port = %s/%d, want 1.2.3.4/443", got.IP, got.Port)
	}
	if got.IPVersion != "ipv4" {
		t.Errorf("IPVersion = %q, want ipv4", got.IPVersion)
	}

	// Location 必须来自 meta（目标 IP 的地理信息），而不是测量者的位置。
	loc := got.Location
	if loc.CCA2 != "US" || loc.Country != "US" {
		t.Errorf("CCA2/Country = %q/%q, want US/US", loc.CCA2, loc.Country)
	}
	if loc.IATA != "ORD" {
		t.Errorf("IATA = %q, want ORD", loc.IATA)
	}
	if loc.City != "Chicago" || loc.Region != "Illinois" {
		t.Errorf("City/Region = %q/%q, want Chicago/Illinois", loc.City, loc.Region)
	}
	if !loc.HasCoordinates {
		t.Fatal("HasCoordinates = false, want true")
	}
	// 字符串形式的经纬度必须被正确解析。
	if loc.Latitude != 41.85003 || loc.Longitude != -87.65005 {
		t.Errorf("Latitude/Longitude = %v/%v, want 41.85003/-87.65005", loc.Latitude, loc.Longitude)
	}
	// 坐标来自 meta，不是 colo，因此不应标记回退。
	if loc.FromColoFallback {
		t.Error("FromColoFallback = true, want false")
	}
	if loc.CountryEN != "United States" {
		t.Errorf("CountryEN = %q, want United States", loc.CountryEN)
	}
}

func TestParseJSONMultiplePortsExpandToSeparateTargets(t *testing.T) {
	// 真实数据里一个 IP 最多出现 6 个端口，必须展开成多个 Target。
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{8443, 443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)

	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(res.Targets))
	}
	// 端口必须升序且保留真实端口，绝不能统一成 443。
	if res.Targets[0].Port != 443 || res.Targets[1].Port != 8443 {
		t.Fatalf("ports = %d,%d, want 443,8443", res.Targets[0].Port, res.Targets[1].Port)
	}
	if res.Targets[0].ID != "1.2.3.4:443" || res.Targets[1].ID != "1.2.3.4:8443" {
		t.Fatalf("ids = %s,%s", res.Targets[0].ID, res.Targets[1].ID)
	}
	if res.Stats.RawCombos != 2 {
		t.Errorf("RawCombos = %d, want 2", res.Stats.RawCombos)
	}
}

func TestParseJSONUsesTargetLocationNotCollectorLocation(t *testing.T) {
	// 目标在德国，colo 在阿姆斯特丹：Coordinate/Region 必须跟随 meta。
	meta := map[string]any{
		"country":   "DE",
		"city":      "Nuremberg",
		"region":    "Bavaria",
		"latitude":  "49.45421",
		"longitude": "11.07752",
		"colo": map[string]any{
			"iata": "AMS", "lat": 52.308601, "lon": 4.76389, "cca2": "NL",
			"region": "Europe", "city": "Amsterdam",
		},
	}
	doc := jsonDoc(t, []map[string]any{jsonItem("62.3.41.40", []int{8443}, meta)}, nil)

	got := mustParseJSON(t, doc).Targets[0].Location

	if got.CCA2 != "NL" {
		t.Errorf("CCA2 = %q, want NL (from colo)", got.CCA2)
	}
	if got.Country != "DE" {
		t.Errorf("Country = %q, want DE (from meta)", got.Country)
	}
	if got.IATA != "AMS" {
		t.Errorf("IATA = %q, want AMS", got.IATA)
	}
	if got.City != "Nuremberg" {
		t.Errorf("City = %q, want Nuremberg", got.City)
	}
	if got.Latitude != 49.45421 {
		t.Errorf("Latitude = %v, want meta latitude 49.45421", got.Latitude)
	}
}

func TestParseJSONColoFallbackMarked(t *testing.T) {
	// meta 没有经纬度时回退到 colo 坐标，并且必须显式标记。
	meta := map[string]any{
		"country": "SG",
		"city":    "Singapore",
		"colo": map[string]any{
			"iata": "SIN", "lat": 1.35019, "lon": 103.994003, "cca2": "SG",
		},
	}
	doc := jsonDoc(t, []map[string]any{jsonItem("9.9.9.9", []int{443}, meta)}, nil)

	got := mustParseJSON(t, doc).Targets[0].Location

	if !got.HasCoordinates {
		t.Fatal("HasCoordinates = false, want true (colo fallback)")
	}
	if !got.FromColoFallback {
		t.Error("FromColoFallback = false, want true")
	}
	if got.Latitude != 1.35019 {
		t.Errorf("Latitude = %v, want 1.35019", got.Latitude)
	}
}

func TestParseJSONZeroCoordinatesAreValid(t *testing.T) {
	// 0,0 是合法坐标（几内亚湾），不能被当成"没有坐标"。
	meta := map[string]any{"country": "GH", "latitude": 0, "longitude": 0}
	doc := jsonDoc(t, []map[string]any{jsonItem("8.8.8.8", []int{443}, meta)}, nil)

	got := mustParseJSON(t, doc).Targets[0].Location
	if !got.HasCoordinates {
		t.Error("HasCoordinates = false, want true for 0,0 coordinates")
	}
	if got.FromColoFallback {
		t.Error("FromColoFallback = true, want false for explicit 0,0")
	}
}

// ---------------------------------------------------------------------------
// JSON 解析：容错
// ---------------------------------------------------------------------------

func TestParseJSONIgnoresUnknownFields(t *testing.T) {
	// 需求：不因为未知 JSON 字段失败。上游随时可能新增字段。
	target := jsonItem("1.2.3.4", []int{443}, realMeta())
	target["brand_new_field"] = map[string]any{"nested": []any{1, 2, 3}}
	target["another"] = "whatever"

	doc := jsonDoc(t, []map[string]any{target}, map[string]any{
		"totally_new_top_level": []any{"a", "b"},
		"future_schema":         map[string]any{"version": 99},
	})

	res := mustParseJSON(t, doc)
	if len(res.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(res.Targets))
	}
	if res.Targets[0].ID != "1.2.3.4:443" {
		t.Errorf("ID = %q, want 1.2.3.4:443", res.Targets[0].ID)
	}
}

func TestParseJSONSkipsInvalidIPAndRecordsReason(t *testing.T) {
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{443}, realMeta()),
		jsonItem("not-an-ip", []int{443}, realMeta()),
		jsonItem("", []int{443}, nil),
		jsonItem("999.999.999.999", []int{443}, nil),
		jsonItem("0.0.0.0", []int{443}, nil),   // 未指定地址不是有效目标
		jsonItem("224.0.0.1", []int{443}, nil), // 组播地址不是有效目标
		jsonItem("5.6.7.8", []int{443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)

	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2 (only valid ones)", len(res.Targets))
	}
	if res.Stats.SkippedItems != 5 {
		t.Errorf("SkippedItems = %d, want 5", res.Stats.SkippedItems)
	}
	if res.Stats.Reasons["invalid ip"] != 5 {
		t.Errorf("reasons[invalid ip] = %d, want 5", res.Stats.Reasons["invalid ip"])
	}
	if len(res.Warnings) == 0 {
		t.Error("expected warnings for invalid records")
	}
}

func TestParseJSONSkipsInvalidPortAndRecordsReason(t *testing.T) {
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{443, 0, 65536, -1, 70000}, realMeta()),
		// 字符串 / 浮点写法应被宽容处理（真实数据里 latitude 就是字符串）。
		{"ip": "5.6.7.8", "port": []any{"8443", 443.0}, "meta": realMeta()},
	}, nil)

	res := mustParseJSON(t, doc)

	if res.Stats.InvalidPorts != 4 {
		t.Errorf("InvalidPorts = %d, want 4", res.Stats.InvalidPorts)
	}
	ids := make([]string, 0, len(res.Targets))
	for _, tg := range res.Targets {
		ids = append(ids, tg.ID)
	}
	want := []string{"1.2.3.4:443", "5.6.7.8:443", "5.6.7.8:8443"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestParseJSONHandlesMissingPortField(t *testing.T) {
	items := []map[string]any{
		{"ip": "1.2.3.4", "meta": realMeta()},                     // 完全没有 port
		{"ip": "2.3.4.5", "port": []any{}, "meta": realMeta()},    // 空数组
		{"ip": "3.4.5.6", "port": nil, "meta": realMeta()},        // null
		{"ip": "4.5.6.7", "port": []any{"x"}, "meta": realMeta()}, // 全非法
	}
	doc := jsonDoc(t, items, nil)

	res := mustParseJSON(t, doc)

	if len(res.Targets) != 0 {
		t.Fatalf("targets = %d, want 0", len(res.Targets))
	}
	if res.Stats.Unknown != 4 {
		t.Errorf("Unknown = %d, want 4", res.Stats.Unknown)
	}
}

func TestParseJSONAcceptsScalarPortShape(t *testing.T) {
	// 防御性：上游若把 port 从数组改成数字，仍然可用。
	doc := jsonDoc(t, []map[string]any{
		{"ip": "1.2.3.4", "port": 2053, "meta": realMeta()},
	}, nil)

	res := mustParseJSON(t, doc)
	if len(res.Targets) != 1 || res.Targets[0].Port != 2053 {
		t.Fatalf("targets = %+v, want single target with port 2053", res.Targets)
	}
}

func TestParseJSONHandlesMissingMetaAndColo(t *testing.T) {
	// 生产数据中确有 8 条缺 colo、15 条缺 asOrganization、若干缺 region。
	doc := jsonDoc(t, []map[string]any{
		{"ip": "1.2.3.4", "port": []any{443}},
		jsonItem("2.3.4.5", []int{443}, map[string]any{"country": "JP", "city": "Tokyo"}),
		jsonItem("3.4.5.6", []int{443}, map[string]any{"country": "JP", "colo": map[string]any{
			"iata": "NRT", "lat": 35.764702, "lon": 140.386002, "cca2": "JP", "city": "Tokyo",
		}}),
	}, nil)

	res := mustParseJSON(t, doc)
	if len(res.Targets) != 3 {
		t.Fatalf("targets = %d, want 3", len(res.Targets))
	}

	// 完全没有 meta：Location 为空但不报错。
	if !res.Targets[0].Location.IsZero() {
		t.Errorf("target[0].Location = %+v, want zero", res.Targets[0].Location)
	}

	// 没有 colo：CCA2 回退到 meta.country。
	if got := res.Targets[1].Location; got.CCA2 != "JP" || got.IATA != "" {
		t.Errorf("target[1].Location = %+v, want CCA2=JP, IATA empty", got)
	}

	// 没有 meta.country：CCA2 来自 colo。
	if got := res.Targets[2].Location; got.CCA2 != "JP" || got.IATA != "NRT" {
		t.Errorf("target[2].Location = %+v, want CCA2=JP, IATA=NRT", got)
	}
}

func TestParseJSONRejectsNonObjectAndMissingData(t *testing.T) {
	for name, body := range map[string]string{
		"array":         `[1,2,3]`,
		"string":        `"hello"`,
		"number":        `42`,
		"truncated":     `{"data": [{"ip": "1.2.3.4"`,
		"empty object":  `{}`,
		"null data":     `{"data": null}`,
		"empty content": ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(body)); err == nil {
				t.Fatalf("ParseJSON(%q) succeeded, want error", body)
			}
		})
	}
}

func TestParseJSONEmptyDataArrayIsAccepted(t *testing.T) {
	// 合法的"空列表"不是格式错误：返回 0 个目标，由调用方决定如何处理。
	res, err := ParseJSON([]byte(`{"generated_at":"2026-10-03T00:00:00","data":[]}`))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(res.Targets) != 0 {
		t.Errorf("targets = %d, want 0", len(res.Targets))
	}
}

// ---------------------------------------------------------------------------
// 去重
// ---------------------------------------------------------------------------

func TestParseJSONDeduplicatesIPPortCombinations(t *testing.T) {
	// 同一个 IP:port 重复出现时只保留一条，并且保持首次出现的顺序。
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{443, 8443}, realMeta()),
		jsonItem("1.2.3.4", []int{443}, realMeta()),             // 完全重复
		jsonItem("1.2.3.4", []int{443, 8443, 2053}, realMeta()), // 部分重复
		jsonItem("5.6.7.8", []int{443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)

	want := []string{"1.2.3.4:443", "1.2.3.4:8443", "1.2.3.4:2053", "5.6.7.8:443"}
	got := make([]string, 0, len(res.Targets))
	for _, tg := range res.Targets {
		got = append(got, tg.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", got, want)
	}
	if res.Stats.Duplicates != 3 {
		t.Errorf("Duplicates = %d, want 3", res.Stats.Duplicates)
	}
	// 去重后数量必须等于 组合数 - 重复 - 非法端口 - 未知
	if res.Stats.RawCombos != len(res.Targets)+res.Stats.Duplicates+res.Stats.InvalidPorts+res.Stats.Unknown {
		t.Errorf("stats inconsistent: %+v, targets=%d", res.Stats, len(res.Targets))
	}
}

func TestParseJSONDedupIsCaseAndFormatInsensitive(t *testing.T) {
	// IPv4-mapped IPv6 与带方括号写法必须归一到同一个目标。
	doc := jsonDoc(t, []map[string]any{
		jsonItem("1.2.3.4", []int{443}, realMeta()),
		jsonItem("::ffff:1.2.3.4", []int{443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)
	if len(res.Targets) != 1 {
		t.Fatalf("targets = %d, want 1 (IPv4-mapped must normalize)", len(res.Targets))
	}
	if res.Targets[0].ID != "1.2.3.4:443" {
		t.Errorf("ID = %q, want 1.2.3.4:443", res.Targets[0].ID)
	}
}

func TestParseJSONIPv6Normalization(t *testing.T) {
	doc := jsonDoc(t, []map[string]any{
		// 展开写法必须被压缩，并与下面的方括号写法归一到同一个目标。
		jsonItem("2001:0db8:0000:0000:0000:0000:0000:0001", []int{443}, realMeta()),
		jsonItem("[2001:db8::1]", []int{443}, realMeta()),
		jsonItem("[2001:db8::1]", []int{8443}, realMeta()),
		// zone 属于本机信息，必须被去掉。
		jsonItem("fe80::1%eth0", []int{443}, realMeta()),
	}, nil)

	res := mustParseJSON(t, doc)

	want := []string{"[2001:db8::1]:443", "[2001:db8::1]:8443", "[fe80::1]:443"}
	got := make([]string, 0, len(res.Targets))
	for _, tg := range res.Targets {
		got = append(got, tg.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if res.Targets[0].IPVersion != "ipv6" {
		t.Errorf("IPVersion = %q, want ipv6", res.Targets[0].IPVersion)
	}
	if res.Stats.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", res.Stats.Duplicates)
	}
}

// ---------------------------------------------------------------------------
// source metadata
// ---------------------------------------------------------------------------

func TestParseJSONKeepsSourceMetadata(t *testing.T) {
	doc := jsonDoc(t, []map[string]any{jsonItem("1.2.3.4", []int{443}, realMeta())}, nil)
	res := mustParseJSON(t, doc)

	if res.Meta.Format != "json" {
		t.Errorf("Format = %q, want json", res.Meta.Format)
	}
	if !res.Meta.HasGeneratorTime {
		t.Fatal("HasGeneratorTime = false, want true")
	}
	// 生产数据的 generated_at 没有时区后缀，必须按 UTC 解析。
	wantTime := time.Date(2026, 10, 3, 3, 52, 35, 771190000, time.UTC)
	if !res.Meta.GeneratorGeneratedAt.Equal(wantTime) {
		t.Errorf("GeneratorGeneratedAt = %s, want %s", res.Meta.GeneratorGeneratedAt, wantTime)
	}
	if res.Meta.GeneratedAtLocation() != time.UTC {
		t.Errorf("generated_at location = %s, want UTC", res.Meta.GeneratedAtLocation())
	}
	if res.Meta.ReportedCount != 1 {
		t.Errorf("ReportedCount = %d, want 1", res.Meta.ReportedCount)
	}
	if res.Meta.CountryCounts["DE"] != 2 {
		t.Errorf("CountryCounts = %v, want DE=2", res.Meta.CountryCounts)
	}
}

func TestFlexTimeFormats(t *testing.T) {
	cases := map[string]bool{
		"2026-10-03T03:52:35.771190":  true,
		"2026-10-03T03:52:35.771190Z": true,
		"2026-10-03T03:52:35Z":        true,
		"2026-10-03T03:52:35+08:00":   true,
		"2026-10-03 03:52:35":         true,
		"2026-10-03":                  true,
		"":                            false,
		"not a time":                  false,
		"1759459955":                  false,
	}
	for in, wantOK := range cases {
		t.Run(in, func(t *testing.T) {
			got := parseFlexTime(in)
			if got.Valid != wantOK {
				t.Errorf("parseFlexTime(%q).Valid = %v, want %v", in, got.Valid, wantOK)
			}
		})
	}

	// 带偏移的时间必须被正确换算，而不是当成 UTC。
	got := parseFlexTime("2026-10-03T11:52:35+08:00")
	if !got.Valid {
		t.Fatal("expected valid parse")
	}
	if got.Time.UTC().Hour() != 3 {
		t.Errorf("UTC hour = %d, want 3", got.Time.UTC().Hour())
	}
}

// ---------------------------------------------------------------------------
// 文本源解析
// ---------------------------------------------------------------------------

func TestParseTextBasic(t *testing.T) {
	body := strings.Join([]string{
		"# generated by upstream",
		"101.32.169.108:443#SG",
		"101.99.75.101:8443#NL",
		"",
		"   ",
		"103.106.228.126:2053#jp", // 小写国家代码应被规范化
		"103.11.76.116:443",       // 没有国家信息也可以
		"// comment",
	}, "\n")

	res, err := ParseText([]byte(body))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	if len(res.Targets) != 4 {
		t.Fatalf("targets = %d, want 4 (%v)", len(res.Targets), res.Targets)
	}
	if res.Meta.Format != "text" {
		t.Errorf("Format = %q, want text", res.Meta.Format)
	}
	if res.Targets[0].ID != "101.32.169.108:443" || res.Targets[0].Location.CCA2 != "SG" {
		t.Errorf("target[0] = %+v, want 101.32.169.108:443 SG", res.Targets[0])
	}
	if res.Targets[1].Port != 8443 {
		t.Errorf("port = %d, want 8443 (must not be forced to 443)", res.Targets[1].Port)
	}
	if res.Targets[2].Location.CCA2 != "JP" {
		t.Errorf("CCA2 = %q, want JP", res.Targets[2].Location.CCA2)
	}
	if res.Targets[3].Location.CCA2 != "" {
		t.Errorf("CCA2 = %q, want empty when no country suffix", res.Targets[3].Location.CCA2)
	}
	// 注释行与空行不计入记录数。
	if res.Stats.RawItems != 4 {
		t.Errorf("RawItems = %d, want 4", res.Stats.RawItems)
	}
}

func TestParseTextDedupAndInvalid(t *testing.T) {
	body := strings.Join([]string{
		"1.2.3.4:443#US",
		"1.2.3.4:443#US",
		"1.2.3.4:8443#US",
		"999.1.1.1:443#US",
		"1.2.3.4:0#US",
		"1.2.3.4:99999#US",
		"1.2.3.4#US", // 缺端口
		"[2001:db8::1]:443#DE",
	}, "\n")

	res, err := ParseText([]byte(body))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	want := []string{"1.2.3.4:443", "1.2.3.4:8443", "[2001:db8::1]:443"}
	got := make([]string, 0, len(res.Targets))
	for _, tg := range res.Targets {
		got = append(got, tg.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", got, want)
	}
	if res.Stats.Duplicates != 1 {
		t.Errorf("Duplicates = %d, want 1", res.Stats.Duplicates)
	}
	if res.Stats.SkippedItems != 4 {
		t.Errorf("SkippedItems = %d, want 4", res.Stats.SkippedItems)
	}
}

func TestParseTextRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"blank lines":   "\n\n   \n",
		"comments only": "# nothing\n// nothing\n",
		"html":          "<!DOCTYPE html>\n<html><body>error</body></html>",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseText([]byte(body)); err == nil {
				t.Fatalf("ParseText(%q) succeeded, want error", body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 格式自动识别
// ---------------------------------------------------------------------------

func TestParseAuto(t *testing.T) {
	jsonBody := jsonDoc(t, []map[string]any{jsonItem("1.2.3.4", []int{443}, realMeta())}, nil)

	cases := []struct {
		name       string
		body       []byte
		wantFormat string
		wantTarget int
		wantErr    bool
	}{
		{"json", jsonBody, "json", 1, false},
		{"json with BOM", append([]byte{0xEF, 0xBB, 0xBF}, jsonBody...), "json", 1, false},
		{"json with leading whitespace", append([]byte("\n\t  "), jsonBody...), "json", 1, false},
		{"text", []byte("1.2.3.4:443#US\n"), "text", 1, false},
		{"text despite .json name", []byte("  \n1.2.3.4:443#US\n"), "text", 1, false},
		{"html is rejected", []byte("<!DOCTYPE html><html></html>"), "text", 0, true},
		{"empty", []byte("   \n"), "", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, format, err := ParseAuto(tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAuto succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAuto: %v", err)
			}
			if format != tc.wantFormat {
				t.Errorf("format = %q, want %q", format, tc.wantFormat)
			}
			if len(res.Targets) != tc.wantTarget {
				t.Errorf("targets = %d, want %d", len(res.Targets), tc.wantTarget)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 宽松类型转换
// ---------------------------------------------------------------------------

func TestAsStringAndAsInt(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`[1, 1.5, "41.85003", 42, true, null, {"a":1}, [1], "  x  "]`))
	dec.UseNumber()
	var values []any
	if err := dec.Decode(&values); err != nil {
		t.Fatalf("decode: %v", err)
	}

	gotStrings := make([]string, 0, len(values))
	for _, v := range values {
		gotStrings = append(gotStrings, asString(v))
	}
	wantStrings := []string{"1", "1.5", "41.85003", "42", "", "", "", "", "x"}
	for i := range wantStrings {
		if gotStrings[i] != wantStrings[i] {
			t.Errorf("asString(%v) = %q, want %q", values[i], gotStrings[i], wantStrings[i])
		}
	}

	// asInt 必须拒绝小数与不可解析的值。
	intCases := []struct {
		in   any
		want int
		ok   bool
	}{
		{json.Number("42"), 42, true},
		{"443", 443, true},
		{443.0, 443, true},
		{json.Number("1.5"), 0, false},
		{"abc", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, tc := range intCases {
		got, ok := asInt(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("asInt(%v) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizePorts(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []int
		bad  int
		ok   bool
	}{
		{"array", []any{json.Number("443"), json.Number("8443")}, []int{443, 8443}, 0, true},
		{"unsorted", []any{json.Number("8443"), json.Number("443")}, []int{443, 8443}, 0, true},
		{"with dupes", []any{json.Number("443"), json.Number("443")}, []int{443}, 0, true},
		{"with invalid", []any{json.Number("443"), json.Number("0"), json.Number("70000")}, []int{443}, 2, true},
		{"empty array", []any{}, nil, 0, false},
		{"nil", nil, nil, 0, false},
		{"scalar", json.Number("2053"), []int{2053}, 0, true},
		{"scalar invalid", json.Number("0"), nil, 1, false},
		{"all invalid", []any{"x"}, nil, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ports, bad, ok := normalizePorts(tc.in)
			if ok != tc.ok || bad != tc.bad {
				t.Fatalf("normalizePorts(%v) = (%v, %d, %v), want (_, %d, %v)", tc.in, ports, bad, ok, tc.bad, tc.ok)
			}
			if fmt.Sprint(ports) != fmt.Sprint(tc.want) {
				t.Errorf("ports = %v, want %v", ports, tc.want)
			}
		})
	}
}

func TestWarningListIsBoundedAndDeduped(t *testing.T) {
	w := newWarningList(3)
	for i := 0; i < 100; i++ {
		w.add("same message")
	}
	for i := 0; i < 10; i++ {
		w.add("unique %d", i)
	}

	items := w.items()
	// 3 条内容 + 1 条汇总。
	if len(items) != 4 {
		t.Fatalf("items = %d (%v), want 4", len(items), items)
	}
	if !strings.Contains(items[3], "more similar warnings") {
		t.Errorf("last item = %q, want summary line", items[3])
	}
}

func TestBackoffIsCapped(t *testing.T) {
	base := 100 * time.Millisecond
	if got := backoff(base, 1); got != base {
		t.Errorf("backoff(1) = %s, want %s", got, base)
	}
	if got := backoff(base, 2); got != 200*time.Millisecond {
		t.Errorf("backoff(2) = %s, want 200ms", got)
	}
	// 必须被上限截断，避免指数退避失控。
	for attempt := 3; attempt < 40; attempt++ {
		if got := backoff(base, attempt); got > maxRetryBackoff {
			t.Fatalf("backoff(%d) = %s, want <= %s", attempt, got, maxRetryBackoff)
		}
	}
}

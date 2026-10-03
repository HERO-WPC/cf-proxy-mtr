package source

import (
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// realWorldJSON 是一份贴近生产数据的 all.json 片段。
//
// 覆盖了实测中出现的所有"别扭"情况：
//   - port 是数组（一个 IP 多个端口）
//   - latitude/longitude 是字符串
//   - asn 是数字、asOrganization 缺失
//   - colo 缺失（8/11610 条）
//   - colo.cca2 与 meta.country 不一致（约 21%）
//   - 未知字段
//   - 重复的 IP:Port
//   - 非法 IP、非法端口
//   - IPv6 目标
//
// 用途：把"解析层产出必须满足模型层不变式"写成可执行契约，
// 而不是靠各写各的断言。
const realWorldJSON = `{
  "generated_at": "2026-10-03T03:52:35.771190",
  "list": {"country": {"US": 2, "DE": 1, "NL": 1, "JP": 1}, "ips": 5},
  "data": [
    {
      "ip": "1.2.3.4",
      "port": [443, 8443],
      "meta": {
        "hostname": "speed.cloudflare.com",
        "clientIp": "159.60.175.146",
        "httpProtocol": "HTTP/1.1",
        "asn": 35280,
        "asOrganization": "Example Hosting",
        "country": "US", "city": "Chicago", "region": "Illinois",
        "postalCode": "60608",
        "latitude": "41.85003", "longitude": "-87.65005",
        "colo": {"iata": "ORD", "lat": 41.9786, "lon": -87.9048, "cca2": "US",
                 "region": "North America", "city": "Chicago"},
        "_port": 443,
        "country_cn": "美国", "country_en": "United States",
        "country_emoji": "US", "continent": "NA",
        "continent_cn": "北美洲", "continent_en": "North America"
      }
    },
    {
      "ip": "62.3.41.40",
      "port": [8443],
      "meta": {
        "hostname": "speed.cloudflare.com",
        "asn": 24940,
        "asOrganization": "Hetzner Online GmbH",
        "country": "DE", "city": "Nuremberg", "region": "Bavaria",
        "postalCode": "90051",
        "latitude": "49.45421", "longitude": "11.07752",
        "colo": {"iata": "CDG", "lat": 49.012798, "lon": 2.55, "cca2": "FR",
                 "region": "Europe", "city": "Paris"},
        "_port": 8443,
        "country_en": "Germany"
      }
    },
    {
      "ip": "203.0.113.10",
      "port": [443],
      "meta": {
        "hostname": "speed.cloudflare.com",
        "asn": 13335,
        "country": "JP", "city": "Tokyo", "region": "Tokyo",
        "latitude": "35.6895", "longitude": "139.69171",
        "colo": {"iata": "NRT", "lat": 35.764702, "lon": 140.386002, "cca2": "JP",
                 "region": "Asia Pacific", "city": "Tokyo"},
        "_port": 443,
        "country_en": "Japan",
        "unknown_future_field": {"nested": [1, 2, 3]}
      }
    },
    {
      "ip": "198.51.100.7",
      "port": [2053],
      "meta": {
        "country": "SG",
        "city": "Singapore",
        "latitude": "1.28967", "longitude": "103.85007",
        "_port": 2053,
        "country_en": "Singapore"
      }
    },
    {
      "ip": "2001:db8::1",
      "port": [443, 0, 70000],
      "meta": {
        "country": "NL", "city": "Amsterdam", "region": "North Holland",
        "latitude": "52.37403", "longitude": "4.88969",
        "colo": {"iata": "AMS", "lat": 52.308601, "lon": 4.76389, "cca2": "NL",
                 "region": "Europe", "city": "Amsterdam"},
        "_port": 443,
        "country_en": "Netherlands"
      }
    },
    {"ip": "not-an-ip", "port": [443], "meta": {"country": "US"}},
    {"ip": "0.0.0.0", "port": [443]},
    {"ip": "", "port": [443]},
    {"ip": "192.0.2.55", "meta": {"country": "US"}},
    {"ip": "1.2.3.4", "port": [443], "meta": {"country": "US"}}
  ]
}`

// parseRealWorld 解析真实形状的数据，并在有错误时直接失败。
func parseRealWorld(t *testing.T) *ParseResult {
	t.Helper()

	res, err := ParseJSON([]byte(realWorldJSON))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	return res
}

// TestRealWorldEveryTargetSatisfiesModelInvariants 是解析层与模型层的契约测试。
//
// 它回答的问题是："解析出来的目标，是否真的都是模型认可的目标？"
// 只要解析层与模型层对 ID / 归一化 / 坐标规则的理解出现分歧，
// 这里就会失败，而不是等到入库或上传时才发现。
func TestRealWorldEveryTargetSatisfiesModelInvariants(t *testing.T) {
	res := parseRealWorld(t)

	if len(res.Targets) == 0 {
		t.Fatal("no targets parsed")
	}

	for i, target := range res.Targets {
		if err := target.Validate(); err != nil {
			t.Errorf("target #%d (%s) fails model validation: %v", i, target.ID, err)
			continue
		}
		// 解析产物必须是规范形式：模型不应再发现任何可归一化的内容。
		if target.Normalize() {
			t.Errorf("target #%d (%s) was not canonical: %+v", i, target.ID, target)
		}
		// ID 必须与模型层独立计算的 ID 完全一致。
		if want := model.TargetID(target.IP, target.Port); target.ID != want {
			t.Errorf("target #%d: ID = %q, want %q", i, target.ID, want)
		}
	}

	// 批量校验一次，覆盖 ValidateAll 的错误定位路径。
	if err := model.ValidateAll(res.Targets); err != nil {
		t.Errorf("ValidateAll: %v", err)
	}
}

// TestRealWorldTargetAndColoAreIndependent 确认解析层没有把两者混在一起。
//
// 具体检查：
//   - 目标是德国、接入点是法国时，两边的国家代码必须不同；
//   - 缺 colo 的记录，Colo 必须留空而不是复制目标位置；
//   - 缺目标坐标的记录，Location 不得被 colo 坐标填充。
func TestRealWorldTargetAndColoAreIndependent(t *testing.T) {
	res := parseRealWorld(t)

	byID := make(map[string]model.Target, len(res.Targets))
	for _, target := range res.Targets {
		byID[target.ID] = target
	}

	germany, ok := byID["62.3.41.40:8443"]
	if !ok {
		t.Fatalf("expected 62.3.41.40:8443 in %v", model.Keys(res.Targets))
	}
	if germany.Location.Country != "DE" {
		t.Errorf("Location.Country = %q, want DE", germany.Location.Country)
	}
	if germany.Location.CCA2 != "FR" {
		t.Errorf("Location.CCA2 = %q, want FR (from colo)", germany.Location.CCA2)
	}
	if germany.Colo.CCA2 != "FR" || germany.Colo.IATA != "CDG" {
		t.Errorf("Colo = %+v, want FR/CDG", germany.Colo)
	}
	// 目标坐标来自 meta，接入点坐标来自 colo：必须是两组不同的值。
	if germany.Location.Latitude == germany.Colo.Latitude {
		t.Error("target and colo coordinates are identical; sources were probably mixed up")
	}

	singapore, ok := byID["198.51.100.7:2053"]
	if !ok {
		t.Fatalf("expected 198.51.100.7:2053")
	}
	if !singapore.Colo.IsZero() {
		t.Errorf("Colo = %+v, want empty when meta has no colo", singapore.Colo)
	}
	if singapore.Location.Country != "SG" || singapore.Location.CCA2 != "SG" {
		t.Errorf("Location = %+v, want SG/SG with CCA2 falling back to country", singapore.Location)
	}

	ipv6, ok := byID["[2001:db8::1]:443"]
	if !ok {
		t.Fatalf("expected [2001:db8::1]:443")
	}
	if ipv6.IPVersion != model.IPVersionIPv6 {
		t.Errorf("IPVersion = %q, want ipv6", ipv6.IPVersion)
	}
	if !strings.HasPrefix(ipv6.Network(), "tcp6") {
		t.Errorf("Network() = %q, want tcp6", ipv6.Network())
	}

	// 目标坐标缺失时，Location 必须保持"没有坐标"，而 colo 坐标照常保留。
	// 这条断言锁住"不把接入点坐标当成目标坐标"这个决定。
	withoutTargetCoords := model.Target{
		ID: "198.51.100.7:2053",
		Location: model.Location{
			Country: "SG",
			CCA2:    "SG",
			City:    "Singapore",
		},
	}
	if withoutTargetCoords.Location.HasCoordinates {
		t.Error("Location.HasCoordinates = true, want false without meta coordinates")
	}
	lat, lon, ok, fromColo := withoutTargetCoords.Location.ResolveCoordinates(withoutTargetCoords.Colo)
	if ok || fromColo {
		t.Errorf("ResolveCoordinates = (%v,%v,ok=%v,fromColo=%v), want no coordinates",
			lat, lon, ok, fromColo)
	}
}

// TestRealWorldParseStatsAreConsistent 检查解析统计与实际目标数量对得上。
//
// 严格不变式（解析层承诺的会计关系）：
//
//	RawItems  = TargetCandidates + SkippedItems + Unknown
//	RawCombos = len(Targets) + Duplicates
//
// InvalidPorts 刻意不参与加法：它统计的是"被丢弃的非法端口值个数"，
// 而同一条记录可能既丢了非法端口、又用合法端口产生了组合。
// 这里用一个不等式把它锁住，避免以后有人把它当成加和项。
func TestRealWorldParseStatsAreConsistent(t *testing.T) {
	res := parseRealWorld(t)
	st := res.Stats

	if st.RawItems != st.TargetCandidates+st.SkippedItems+st.Unknown {
		t.Errorf("RawItems = %d, want TargetCandidates(%d) + SkippedItems(%d) + Unknown(%d)",
			st.RawItems, st.TargetCandidates, st.SkippedItems, st.Unknown)
	}
	if st.RawCombos != len(res.Targets)+st.Duplicates {
		t.Errorf("RawCombos = %d, want len(Targets)(%d) + Duplicates(%d)",
			st.RawCombos, len(res.Targets), st.Duplicates)
	}
	if st.TargetCandidates > st.RawCombos {
		t.Errorf("TargetCandidates = %d > RawCombos = %d (每个候选记录至少要产生一个组合)",
			st.TargetCandidates, st.RawCombos)
	}
	// 具体数字：IPv6 记录的端口数组是 [443, 0, 70000]，
	// 因此必须恰好丢掉 2 个非法端口值，同时仍然产生一个 443 的组合。
	if st.InvalidPorts != 2 {
		t.Errorf("InvalidPorts = %d, want 2 (0 and 70000)", st.InvalidPorts)
	}

	if st.SkippedItems == 0 {
		t.Error("SkippedItems = 0, want invalid records counted")
	}
	if st.Unknown == 0 {
		t.Error("Unknown = 0, want records without port counted")
	}
	if st.Duplicates == 0 {
		t.Error("Duplicates = 0, want the duplicated ip:port counted")
	}
	if st.InvalidPorts == 0 {
		t.Error("InvalidPorts = 0, want 0/70000 counted")
	}

	// 1.2.3.4 出现两次，端口 443 重复一次：去重后必须只剩一条。
	dupes := 0
	for _, target := range res.Targets {
		if target.ID == "1.2.3.4:443" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Errorf("1.2.3.4:443 appears %d times, want 1", dupes)
	}
}

// TestRealWorldIDsMatchAcrossParseAndCache 是解析层与缓存层的一致性契约。
//
// 目标经过"解析 -> 写缓存 -> 读缓存"之后必须完全不变。
// 这条性质是断点续测与导出 diff 的基础：如果往返会改变目标，
// 那么"哪些目标已测过"的判断就会漂移。
func TestRealWorldIDsMatchAcrossParseAndCache(t *testing.T) {
	res := parseRealWorld(t)
	path := t.TempDir() + "/cache.json"

	res.Meta.URL = "https://example.test/all.json"
	if err := WriteCache(path, res.Meta, res.Stats, res.Targets); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	_, _, roundTripped, _, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}

	if len(roundTripped) != len(res.Targets) {
		t.Fatalf("round trip changed target count: %d -> %d", len(res.Targets), len(roundTripped))
	}
	for i := range res.Targets {
		if !roundTripped[i].Equal(res.Targets[i]) {
			t.Errorf("target #%d changed across cache round trip:\n before = %+v\n after  = %+v",
				i, res.Targets[i], roundTripped[i])
		}
	}
}

// TestRealWorldKeysAreStableAndUnique 确认目标键唯一且顺序稳定。
func TestRealWorldKeysAreStableAndUnique(t *testing.T) {
	first := parseRealWorld(t)
	second := parseRealWorld(t)

	keysFirst := model.Keys(first.Targets)
	keysSecond := model.Keys(second.Targets)

	if strings.Join(keysFirst, ",") != strings.Join(keysSecond, ",") {
		t.Errorf("parse is not deterministic:\n %v\n %v", keysFirst, keysSecond)
	}

	seen := make(map[string]struct{}, len(keysFirst))
	for _, k := range keysFirst {
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate key %q in parsed targets", k)
		}
		seen[k] = struct{}{}
	}
}

// TestRealWorldInvalidRecordsAreCountedNotFatal 确认坏记录不会拖垮整体解析。
func TestRealWorldInvalidRecordsAreCountedNotFatal(t *testing.T) {
	res := parseRealWorld(t)

	// 3 条非法 IP（not-an-ip / 0.0.0.0 / 空）、1 条缺端口。
	if res.Stats.SkippedItems != 3 {
		t.Errorf("SkippedItems = %d, want 3", res.Stats.SkippedItems)
	}
	if res.Stats.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", res.Stats.Unknown)
	}
	// 仍然解析出了有效目标：1.2.3.4 两个端口、其余 4 个 IP 各一个端口。
	// 末尾重复的 1.2.3.4:443 被去重，不作为独立目标。
	if len(res.Targets) != 6 {
		t.Errorf("targets = %d, want 6 (%v)", len(res.Targets), model.Keys(res.Targets))
	}
	// 告警必须被保留，便于用户知道数据有问题。
	if len(res.Warnings) == 0 {
		t.Error("Warnings is empty, want invalid records reported")
	}
}

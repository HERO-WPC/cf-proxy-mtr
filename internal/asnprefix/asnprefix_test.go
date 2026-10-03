package asnprefix

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件锁定前缀匹配与"整表下载"的正确性。
//
// 为什么值得单独测：匹配错了不会报错，只会让线路名**悄悄不对**
// ——比如把"走了 CN2"认成"没走"，或者把 163 认成 CN2。
// 那种错误从日志上看完全正常。

// ---------------------------------------------------------------------------
// 集合与前缀匹配
// ---------------------------------------------------------------------------

// TestSetContainsIPv4 验证 IPv4 包含判断。
func TestSetContainsIPv4(t *testing.T) {
	set, skipped, err := NewSet([]string{
		"1.71.103.0/24",
		"27.148.248.0/21",
		"58.43.192.0/18",
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}

	cases := []struct {
		ip   string
		want bool
	}{
		{"1.71.103.1", true},
		{"1.71.103.255", true},
		{"1.71.104.1", false},   // 刚好越界
		{"1.71.102.255", false}, // 刚好越界（另一侧）
		{"27.148.248.1", true},
		{"27.148.255.255", true}, // /21 的末尾
		{"27.149.0.0", false},
		{"58.43.192.1", true},
		{"58.43.255.255", true},
		{"58.44.0.0", false},
		{"8.8.8.8", false},
	}

	for _, tc := range cases {
		if got := set.Contains(mustAddr(t, tc.ip)); got != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestSetContainsIPv6 验证 IPv6 包含判断。
func TestSetContainsIPv6(t *testing.T) {
	set, _, err := NewSet([]string{
		"2400:9380:9001::/48",
		"2401:8a00::/32",
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		ip   string
		want bool
	}{
		{"2400:9380:9001::1", true},
		{"2400:9380:9001:ffff::1", true},
		{"2400:9380:9002::1", false},
		{"2401:8a00::1", true},
		{"2401:8a01::1", false},
		{"2001:4860:4860::8888", false},
	}

	for _, tc := range cases {
		if got := set.Contains(mustAddr(t, tc.ip)); got != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestSetHandlesHostBitsInPrefix 验证 "1.2.3.4/24" 这种带主机位的写法。
func TestSetHandlesHostBitsInPrefix(t *testing.T) {
	set, _, err := NewSet([]string{"1.2.3.4/24"})
	if err != nil {
		t.Fatal(err)
	}

	for _, ip := range []string{"1.2.3.1", "1.2.3.4", "1.2.3.255"} {
		if !set.Contains(mustAddr(t, ip)) {
			t.Errorf("Contains(%s) = false; host bits in the prefix were not masked off", ip)
		}
	}
	if set.Contains(mustAddr(t, "1.2.4.1")) {
		t.Error("Contains(1.2.4.1) = true, want false")
	}
}

// TestSetHandlesNestedPrefixes 验证嵌套前缀。
func TestSetHandlesNestedPrefixes(t *testing.T) {
	set, _, err := NewSet([]string{
		"10.0.0.0/8",
		"10.1.0.0/16",
		"10.1.2.0/24",
		"10.1.2.128/25",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, ip := range []string{"10.0.0.1", "10.1.0.1", "10.1.2.1", "10.1.2.200", "10.255.255.255"} {
		if !set.Contains(mustAddr(t, ip)) {
			t.Errorf("Contains(%s) = false for a nested prefix set", ip)
		}
	}
	if set.Contains(mustAddr(t, "11.0.0.1")) {
		t.Error("Contains(11.0.0.1) = true, want false")
	}
}

// TestSetSkipsUnparseablePrefixes 验证坏数据被跳过而不是让整批失败。
func TestSetSkipsUnparseablePrefixes(t *testing.T) {
	set, skipped, err := NewSet([]string{
		"1.1.1.0/24",
		"not-a-prefix",
		"",
		"999.999.999.999/24",
		"2.2.2.0/24",
	})
	if err != nil {
		t.Fatalf("NewSet returned an error for a batch with bad entries: %v", err)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
	if set.Len() != 2 {
		t.Errorf("Len = %d, want 2 (the good ones survive)", set.Len())
	}
}

// TestSetDistinguishesAddressFamilies 验证 v4 与 v6 不互相污染。
func TestSetDistinguishesAddressFamilies(t *testing.T) {
	set, _, err := NewSet([]string{"1.1.1.0/24", "2400::/24"})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len4() != 1 || set.Len6() != 1 {
		t.Errorf("Len4=%d Len6=%d, want 1 and 1", set.Len4(), set.Len6())
	}
	if !set.Contains(mustAddr(t, "1.1.1.1")) {
		t.Error("v4 lookup failed")
	}
	if !set.Contains(mustAddr(t, "2400::1")) {
		t.Error("v6 lookup failed")
	}
}

// ---------------------------------------------------------------------------
// 区间到 CIDR 的转换
// ---------------------------------------------------------------------------

// TestRangeToPrefixes 验证区间转 CIDR。
//
// 全量表给的是区间（起始、结束），而项目的其余部分用 CIDR。
// 转换错了会让一段 IP **静默地**落在集合外——线路名少一条，
// 而没有任何报错。因此这里逐个断言精确的转换结果。
func TestRangeToPrefixes(t *testing.T) {
	cases := []struct {
		from, to string
		want     []string
	}{
		// 恰好一条 CIDR 对齐的区间。
		{"1.0.0.0", "1.0.0.255", []string{"1.0.0.0/24"}},
		{"1.71.103.0", "1.71.103.255", []string{"1.71.103.0/24"}},
		// /23 对齐。
		{"1.203.112.0", "1.203.113.255", []string{"1.203.112.0/23"}},
		// 单地址。
		{"1.1.1.1", "1.1.1.1", []string{"1.1.1.1/32"}},
		// 非对齐：必须拆成多条，且合起来恰好覆盖原区间。
		{"1.0.0.1", "1.0.0.2", []string{"1.0.0.1/32", "1.0.0.2/32"}},
		// 跨幂边界：1.0.0.0/24 + 1.0.1.0/24。
		{"1.0.0.0", "1.0.1.255", []string{"1.0.0.0/23"}},
		// 经典拆分段：3 个地址 -> /31 + /32。
		{"10.0.0.0", "10.0.0.2", []string{"10.0.0.0/31", "10.0.0.2/32"}},
		// 整个 IPv4 空间。
		{"0.0.0.0", "255.255.255.255", []string{"0.0.0.0/0"}},
		// IPv6。
		{"2400:9380:8001::", "2400:9380:8001:ffff:ffff:ffff:ffff:ffff", []string{"2400:9380:8001::/48"}},
	}

	for _, tc := range cases {
		got := rangeToPrefixes(mustAddr(t, tc.from), mustAddr(t, tc.to))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("rangeToPrefixes(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestRangeToPrefixesCoversExactlyTheRange 验证转换结果与区间等价。
//
// 上一条测的是精确的拆法，这条测的是**语义**：转换后的集合
// 必须恰好覆盖原区间，不多不少。两者互补——只测语义可能漏掉
// "拆得很难看但勉强正确"的情况，只测精确值又会漏掉边界理解错误。
func TestRangeToPrefixesCoversExactlyTheRange(t *testing.T) {
	ranges := []struct{ from, to string }{
		{"1.0.0.1", "1.0.0.2"},
		{"10.0.0.0", "10.0.0.2"},
		{"27.148.248.0", "27.148.255.255"},
		{"1.0.0.0", "1.0.1.255"},
		{"192.168.1.5", "192.168.1.250"},
	}

	for _, r := range ranges {
		prefixes := rangeToPrefixes(mustAddr(t, r.from), mustAddr(t, r.to))
		set, _, err := NewSet(prefixes)
		if err != nil {
			t.Fatalf("NewSet: %v", err)
		}

		// 两端必须在集合里。
		for _, ip := range []string{r.from, r.to} {
			if !set.Contains(mustAddr(t, ip)) {
				t.Errorf("range %s-%s: endpoint %s is not covered", r.from, r.to, ip)
			}
		}

		// 紧邻区间外的地址必须不在集合里。
		//
		// 上界用 end+1；下界用 start-1（start 为 0 时越界，跳过）。
		next := addInt(mustAddr(t, r.to), 1)
		if next.IsValid() && set.Contains(next) {
			t.Errorf("range %s-%s: address %s just past the end is wrongly covered",
				r.from, r.to, next)
		}

		prev := addInt(mustAddr(t, r.from), -1)
		if prev.IsValid() && set.Contains(prev) {
			t.Errorf("range %s-%s: address %s just before the start is wrongly covered",
				r.from, r.to, prev)
		}
	}
}

// addInt 返回 addr + delta（delta 可为负）；越界时返回无效地址。
//
// 用它而不是自己移位：测试要断言"区间外的紧邻地址"，
// 而那正是最容易算错的地方。
func addInt(addr netip.Addr, delta int) netip.Addr {
	value := addrToInt(addr)
	value.Add(value, big.NewInt(int64(delta)))

	bits := 128
	if addr.Is4() {
		bits = 32
	}
	if value.Sign() < 0 || value.BitLen() > bits {
		return netip.Addr{}
	}
	return intToAddr(value, bits)
}

// ---------------------------------------------------------------------------
// 整表下载
// ---------------------------------------------------------------------------

// fakeBulkServer 提供一个假的"全量映射表"服务。
//
// 返回 gzip 压缩的 TSV，格式与 iptoasn.com 一致：
//
//	range_start  range_end  asn  country  description
func fakeBulkServer(t *testing.T, rows []string) (*httptest.Server, *int64) {
	t.Helper()

	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		for _, row := range rows {
			if _, err := gz.Write([]byte(row + "\n")); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// sampleBulkRows 是覆盖两条线路的样本数据（含无关的 ASN 与坏行）。
func sampleBulkRows() []string {
	return []string{
		"1.71.103.0\t1.71.103.255\t4809\tCN\tCHINATELECOM-CORE-WAN-CN2",
		"103.11.109.0\t103.11.109.255\t58453\tHK\tCMI-INT-HK China Mobile",
		"27.0.160.0\t27.0.163.255\t9808\tCN\tCHINAMOBILE-CN",
		// 无关的 ASN：不该被收进来。
		"1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARENET",
		// 未路由：asn 为 0，跳过。
		"1.0.1.0\t1.0.3.255\t0\tNone\tNot routed",
		// 坏行：字段不足，跳过而不是让整份数据作废。
		"broken-line",
	}
}

// TestLoadParsesBulkTable 验证一次下载就能拿到多条线路的前缀。
func TestLoadParsesBulkTable(t *testing.T) {
	server, hits := fakeBulkServer(t, sampleBulkRows())

	resolver := Load(context.Background(), Options{
		Dir:        t.TempDir(),
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809", "AS58453"},
		HTTPClient: server.Client(),
	})

	if !resolver.Ready() {
		t.Fatal("resolver is not ready")
	}
	if *hits != 1 {
		t.Errorf("made %d requests, want exactly 1 (one bulk download for all ASNs)", *hits)
	}

	// CN2
	if got := resolver.Match("1.71.103.5"); len(got) != 1 || got[0] != "AS4809" {
		t.Errorf("Match(CN2 address) = %v, want [AS4809]", got)
	}
	// CMI
	if got := resolver.Match("103.11.109.5"); len(got) != 1 || got[0] != "AS58453" {
		t.Errorf("Match(CMI address) = %v, want [AS58453]", got)
	}
	// 无关的 ASN 不该被加载。
	if got := resolver.Match("1.0.0.5"); len(got) != 0 {
		t.Errorf("Match(Cloudflare address) = %v, want empty (AS13335 was not requested)", got)
	}
	// 未路由的段不该被加载。
	if got := resolver.Match("1.0.1.5"); len(got) != 0 {
		t.Errorf("Match(unrouted address) = %v, want empty", got)
	}
}

// TestLoadWritesCacheForAllRequestedASNs 验证**全有或全无**。
//
// 这是这次改造的核心：旧的"每个 ASN 查一次"会得到半成品
// （60 个请求成功 18 个，于是 12 条线路静默失效）。整表下载
// 要么全部成功、要么整份退回缓存。
func TestLoadWritesCacheForAllRequestedASNs(t *testing.T) {
	server, _ := fakeBulkServer(t, sampleBulkRows())
	dir := t.TempDir()

	asns := []string{"AS4809", "AS58453", "AS9808", "AS99999"}
	Load(context.Background(), Options{
		Dir:        dir,
		BulkURL:    server.URL,
		ASNs:       asns,
		HTTPClient: server.Client(),
	})

	// 每个请求过的 ASN 都必须有缓存文件——**包括表里没有段的那个**。
	// 空结果同样是一次有效回答，不写的话每次运行都会重下整张表。
	for _, asn := range asns {
		if _, err := os.Stat(filepath.Join(dir, asn+".json")); err != nil {
			t.Errorf("no cache file for %s: %v", asn, err)
		}
	}
}

// TestLoadIsOfflineWhenCacheIsFresh 验证缓存新鲜时完全不联网。
func TestLoadIsOfflineWhenCacheIsFresh(t *testing.T) {
	server, hits := fakeBulkServer(t, sampleBulkRows())
	dir := t.TempDir()
	opts := Options{
		Dir:        dir,
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809", "AS58453"},
		HTTPClient: server.Client(),
	}

	Load(context.Background(), opts)
	first := *hits
	if first == 0 {
		t.Fatal("the first load did not fetch anything")
	}

	second := Load(context.Background(), opts)
	if *hits != first {
		t.Errorf("second load made %d extra requests; a fresh cache must be offline", *hits-first)
	}
	if !second.Ready() {
		t.Error("second load is not ready")
	}
	if got := second.Match("1.71.103.5"); len(got) != 1 {
		t.Errorf("cached data does not work: Match = %v", got)
	}
}

// TestCacheExpiryUsesInjectedClock 验证过期判断走的是注入的时钟。
//
// 这条测试存在的理由：早先的 TTL 测试用 "TTL=1ns" 制造过期，
// 结果因为文件系统时间戳粒度而偶发失败（Windows 时间戳约 100ns
// 粒度且可能滞后）。把"过期由时钟决定"显式钉住，就不会再有人
// 退回去依赖真实时间戳。
func TestCacheExpiryUsesInjectedClock(t *testing.T) {
	server, hits := fakeBulkServer(t, sampleBulkRows())
	base := Options{
		Dir:        t.TempDir(),
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	}

	Load(context.Background(), base)
	first := *hits

	future := base
	future.Now = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	Load(context.Background(), future)

	if *hits == first {
		t.Error("a cache that expired by the injected clock was reused")
	}
}

// TestLoadFallsBackToCacheWhenBulkFails 验证下载失败时用已有缓存。
//
// 骨干线路的 IP 段变化很慢，过期的前缀仍然比没有强。
func TestLoadFallsBackToCacheWhenBulkFails(t *testing.T) {
	server, _ := fakeBulkServer(t, sampleBulkRows())
	dir := t.TempDir()
	base := Options{
		Dir:        dir,
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809", "AS58453"},
		HTTPClient: server.Client(),
	}
	Load(context.Background(), base)

	// 表坏掉 + 缓存过期（用注入时钟让过期确定发生）。
	broken, _ := fakeBulkServer(t, nil)
	broken.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	stale := base
	stale.BulkURL = broken.URL
	stale.Now = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }

	var logs []string
	stale.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	resolver := Load(context.Background(), stale)
	if !resolver.Ready() {
		t.Fatal("resolver gave up instead of using the cached data")
	}
	if got := resolver.Match("1.71.103.5"); len(got) != 1 {
		t.Errorf("Match = %v, want the cached data to still work", got)
	}
	if !containsAny(logs, "退回本地缓存") {
		t.Errorf("logs do not mention the cache fallback: %v", logs)
	}
}

// TestLoadSurvivesTotalFailure 验证下载全盘失败时**不 panic、不阻塞**，
// 而且仍然可用（靠内置快照）。
//
// 这条测试的契约在加入内置快照之后变了：以前"全盘失败"等于
// "没有任何数据"，现在等于"退回内置快照"。**后者才是想要的
// 行为**——下载失败不该让线路名整个消失。
//
// "确实一点数据都没有"的场景由 TestSnapshotDoesNotPretendToHaveUnknownASNs
// 覆盖（快照里没有的 ASN 不该被假装加载成功）。
func TestLoadSurvivesTotalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var logs []string
	resolver := Load(context.Background(), Options{
		Dir:        t.TempDir(),
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809", "AS58453"},
		HTTPClient: server.Client(),
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})

	// 不能因为下载失败就崩掉或卡住——回到这里就说明没卡住。
	if !containsAny(logs, "不可用") {
		t.Errorf("logs do not report the download failure: %v", logs)
	}

	// 而且必须仍然可用：内置快照接过来了。
	if !resolver.Ready() {
		t.Fatalf("下载失败后既没有缓存也没有用上内置快照；日志：%v", logs)
	}
	if !containsAny(logs, "内置快照") {
		t.Errorf("logs do not mention the snapshot fallback: %v", logs)
	}
	if got := resolver.Match("1.1.1.1"); got == nil {
		// 1.1.1.1 本身不属于这些线路很正常；这里只确认调用不 panic。
		_ = got
	}
}

// TestCacheWriteIsAtomic 验证缓存写入不留临时文件。
func TestCacheWriteIsAtomic(t *testing.T) {
	server, _ := fakeBulkServer(t, sampleBulkRows())
	dir := t.TempDir()

	Load(context.Background(), Options{
		Dir: dir, BulkURL: server.URL, ASNs: []string{"AS4809"}, HTTPClient: server.Client(),
	})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", entry.Name())
		}
	}
}

// TestNormalizeASNKey 验证 ASN 写法归一化。
//
// 表里是裸数字 "4809"，asnmap 用 "AS4809"。两边对不上会
// **一个都匹配不到**，而且不会有任何提示——因此这条必须测。
func TestNormalizeASNKey(t *testing.T) {
	cases := map[string]string{
		"AS4809":   "AS4809",
		"4809":     "AS4809",
		" as4809 ": "AS4809",
		"":         "",
	}
	for input, want := range cases {
		if got := normalizeASNKey(input); got != want {
			t.Errorf("normalizeASNKey(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestParseBulkSkipsBadRows 验证坏行被跳过而不是让整份数据作废。
func TestParseBulkSkipsBadRows(t *testing.T) {
	rows := strings.Join([]string{
		"1.1.1.0\t1.1.1.255\t4809\tCN\tgood",
		"only-one-field",
		"bad-ip\talso-bad\t4809\tCN\tbad addresses",
		"2.2.2.0\t2.2.2.255\tnot-a-number\tCN\tbad asn",
		"3.3.3.0\t3.3.3.255\t58453\tHK\tgood",
	}, "\n")

	got, err := parseBulk(strings.NewReader(rows), map[string]bool{"AS4809": true, "AS58453": true})
	if err != nil {
		t.Fatalf("parseBulk: %v", err)
	}
	if len(got["AS4809"]) != 1 {
		t.Errorf("AS4809 got %v, want exactly the one good row", got["AS4809"])
	}
	if len(got["AS58453"]) != 1 {
		t.Errorf("AS58453 got %v, want exactly the one good row", got["AS58453"])
	}
}

// TestLoadRejectsNothingButReportsMissingASN 验证表里没有的 ASN 不会误报。
func TestLoadRejectsNothingButReportsMissingASN(t *testing.T) {
	server, _ := fakeBulkServer(t, sampleBulkRows())

	resolver := Load(context.Background(), Options{
		Dir:     t.TempDir(),
		BulkURL: server.URL,
		// AS99999 不在表里。
		ASNs:       []string{"AS4809", "AS99999"},
		HTTPClient: server.Client(),
	})

	if !resolver.Ready() {
		t.Fatal("resolver should still be ready from AS4809")
	}
	if got := resolver.Match("1.71.103.5"); len(got) != 1 {
		t.Errorf("Match = %v, want [AS4809]", got)
	}
	// 表里没有的 ASN 不该出现在结果里。
	for _, asn := range resolver.ASNs() {
		if asn == "AS99999" {
			t.Error("AS99999 has no data but appears as loaded")
		}
	}
}

// ---------------------------------------------------------------------------
// Resolver 行为
// ---------------------------------------------------------------------------

// TestResolverMatchReturnsAllLinesInStableOrder 验证匹配顺序稳定。
func TestResolverMatchReturnsAllLinesInStableOrder(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS10099": {"1.1.1.0/24"},
		"AS4809":  {"1.1.1.0/24"},
	}, "AS4809", "AS10099")

	first := resolver.Match("1.1.1.1")
	if len(first) != 2 {
		t.Fatalf("Match returned %v, want both lines", first)
	}
	if first[0] != "AS4809" || first[1] != "AS10099" {
		t.Errorf("Match = %v, want [AS4809 AS10099] (asnOrder)", first)
	}

	for i := 0; i < 20; i++ {
		got := resolver.Match("1.1.1.1")
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("Match order changed between calls: %v vs %v", got, first)
		}
	}
}

// TestResolverMatchSkipsPrivateAndInvalid 验证私有/非法地址不匹配。
func TestResolverMatchSkipsPrivateAndInvalid(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS4809": {"10.0.0.0/8", "192.168.0.0/16"},
	}, "AS4809")

	for _, ip := range []string{"", "not-an-ip", "10.1.1.1", "192.168.1.1", "127.0.0.1", "::1"} {
		if got := resolver.Match(ip); len(got) != 0 {
			t.Errorf("Match(%q) = %v, want empty (private or invalid)", ip, got)
		}
	}
}

// TestResolverResolvePathDedupesAdjacentOnly 验证按跳序只去相邻重复。
func TestResolverResolvePathDedupesAdjacentOnly(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS4809":  {"1.1.1.0/24"},
		"AS58453": {"2.2.2.0/24"},
	}, "AS4809", "AS58453")

	// CN2 -> CN2 -> CMI -> 超时 -> CN2：中间那次"绕回来"必须保留。
	hops := []string{"1.1.1.1", "1.1.1.2", "2.2.2.1", "", "1.1.1.3"}
	got := resolver.ResolvePath(hops)

	want := []string{"AS4809", "AS58453", "AS4809"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ResolvePath = %v, want %v (adjacent-only dedupe)", got, want)
	}
}

// TestResolverResolvePathHandlesUnknownHops 验证不认识的中转跳被跳过。
func TestResolverResolvePathHandlesUnknownHops(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS4809": {"1.1.1.0/24"},
	}, "AS4809")

	hops := []string{"192.168.1.1", "8.8.8.8", "1.1.1.1", "9.9.9.9", "1.1.1.2"}
	got := resolver.ResolvePath(hops)

	if len(got) != 1 || got[0] != "AS4809" {
		t.Errorf("ResolvePath = %v, want [AS4809]", got)
	}
}

// TestResolverReadyAndSize 验证就绪判定与规模统计。
func TestResolverReadyAndSize(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS4809": {"1.1.1.0/24", "2400::/32"},
	}, "AS4809")

	if !resolver.Ready() {
		t.Error("Ready() = false, want true")
	}
	asns, prefixes, v4, v6 := resolver.Size()
	if asns != 1 || prefixes != 2 || v4 != 1 || v6 != 1 {
		t.Errorf("Size = (%d,%d,%d,%d), want (1,2,1,1)", asns, prefixes, v4, v6)
	}

	var empty *Resolver
	if empty.Ready() {
		t.Error("a nil resolver reported Ready()")
	}
	if got := empty.Match("1.1.1.1"); got != nil {
		t.Errorf("a nil resolver matched %v", got)
	}
}

// TestPathWithNames 验证线路串的展示形式。
func TestPathWithNames(t *testing.T) {
	names := map[string]string{
		"AS4809":  "CN2",
		"AS58453": "CMI",
	}
	namer := func(asn string) string { return names[asn] }

	got := PathWithNames([]string{"AS4809", "AS58453", "AS99999"}, namer)
	want := "CN2(AS4809) > CMI(AS58453) > AS99999"
	if got != want {
		t.Errorf("PathWithNames = %q, want %q", got, want)
	}

	if got := PathWithNames(nil, namer); got != "" {
		t.Errorf("PathWithNames(nil) = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// resolverWith 用手工数据构造 Resolver（不联网）。
func resolverWith(t *testing.T, data map[string][]string, order ...string) *Resolver {
	t.Helper()

	resolver := &Resolver{
		byASN:    make(map[string]*Set, len(data)),
		asnOrder: order,
		opts:     withDefaults(Options{}),
	}
	for asn, prefixes := range data {
		set, _, err := NewSet(prefixes)
		if err != nil {
			t.Fatalf("NewSet(%s): %v", asn, err)
		}
		resolver.byASN[asn] = set
	}
	return resolver
}

// mustAddr 解析一个必须合法的 IP。
func mustAddr(t *testing.T, ip string) netip.Addr {
	t.Helper()

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", ip, err)
	}
	return addr
}

// containsAny 报告日志里是否出现过某个子串。
func containsAny(logs []string, want string) bool {
	for _, line := range logs {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// 让 json 在本文件被引用（部分测试间接使用）。
var _ = json.Marshal

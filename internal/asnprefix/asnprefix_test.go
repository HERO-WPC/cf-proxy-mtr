package asnprefix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件锁定前缀匹配的正确性。
//
// 为什么值得单独测：匹配错了不会报错，只会让线路名**悄悄不对**
// ——比如把"走了 CN2"认成"没走"，或者把 163 认成 CN2。
// 那种错误从日志上看完全正常。

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
//
// 不规范化的话 start 会算成 1.2.3.4，于是 1.2.3.1 落不进这个段——
// 而 RouteViews 的数据里确实出现过这种写法。
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
//
// RouteViews 数据里同一线路会同时宣告 /18 和它内部的 /24。
// 二分查找只检查"最后一个 start <= target"的条目，嵌套时
// 那一条不一定是覆盖 target 的那一条。
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
	if set.Contains(mustAddr(t, "1.1.1.1")) != true {
		t.Error("v4 lookup failed")
	}
	if set.Contains(mustAddr(t, "2400::1")) != true {
		t.Error("v6 lookup failed")
	}
}

// TestResolverMatchReturnsAllLinesInStableOrder 验证匹配顺序稳定。
//
// 固定顺序是必须的：同一个 IP 落在多条线路的段里时，
// 谁先出现必须每次一样，否则同一份数据两次运行会给出不同线路名。
func TestResolverMatchReturnsAllLinesInStableOrder(t *testing.T) {
	resolver := resolverWith(t, map[string][]string{
		"AS10099": {"1.1.1.0/24"}, // 故意让编号大的排在 map 前面
		"AS4809":  {"1.1.1.0/24"},
	}, "AS4809", "AS10099")

	first := resolver.Match("1.1.1.1")
	if len(first) != 2 {
		t.Fatalf("Match returned %v, want both lines", first)
	}
	// 顺序必须是 asnOrder 的顺序。
	if first[0] != "AS4809" || first[1] != "AS10099" {
		t.Errorf("Match = %v, want [AS4809 AS10099] (asnOrder)", first)
	}

	// 反复查必须一致。
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

	// 中间那些是内网/未知 IP，不该产出任何线路，也不该打断序列。
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
// 抓取与缓存
// ---------------------------------------------------------------------------

// TestLoadFetchesAndCaches 验证首次抓取并写缓存。
func TestLoadFetchesAndCaches(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 路径形如 /4809，af 是查询参数。
		asn := strings.TrimPrefix(r.URL.Path, "/")
		var prefixes []string
		switch {
		case asn == "4809" && r.URL.Query().Get("af") == "4":
			prefixes = []string{"1.1.1.0/24", "2.2.2.0/24"}
		case asn == "4809" && r.URL.Query().Get("af") == "6":
			prefixes = []string{"2400::/32"}
		}
		_ = json.NewEncoder(w).Encode(prefixes)
	}))
	defer server.Close()

	dir := t.TempDir()
	resolver := Load(context.Background(), Options{
		Dir:        dir,
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})

	if !resolver.Ready() {
		t.Fatal("resolver is not ready after fetching")
	}
	if got := resolver.Match("1.1.1.1"); len(got) != 1 || got[0] != "AS4809" {
		t.Errorf("Match = %v, want [AS4809]", got)
	}
	if got := resolver.Match("2400::1"); len(got) != 1 {
		t.Errorf("IPv6 Match = %v, want [AS4809]", got)
	}

	// 缓存文件必须落盘。
	if _, err := os.Stat(filepath.Join(dir, "AS4809.json")); err != nil {
		t.Errorf("cache file was not written: %v", err)
	}

	fetchesAfterFirst := hits.Load()
	if fetchesAfterFirst < 2 {
		t.Errorf("expected both address families to be fetched, got %d requests", fetchesAfterFirst)
	}

	// 第二次加载应当**不联网**。
	second := Load(context.Background(), Options{
		Dir:        dir,
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})
	if !second.Ready() {
		t.Error("second load is not ready")
	}
	if hits.Load() != fetchesAfterFirst {
		t.Errorf("second load made %d extra requests; a fresh cache should be offline",
			hits.Load()-fetchesAfterFirst)
	}
}

// TestLoadRefetchesExpiredCache 验证过期缓存会重新抓取。
func TestLoadRefetchesExpiredCache(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	dir := t.TempDir()
	base := Options{Dir: dir, BaseURL: server.URL, ASNs: []string{"AS4809"}, HTTPClient: server.Client()}

	Load(context.Background(), base)
	first := hits.Load()

	// 用**注入时钟**把"现在"推到未来，而不是把 TTL 设成 1ns。
	//
	// 靠极小 TTL 是不稳的：过期判断是 `now - mtime > ttl`，
	// 而 mtime 来自文件系统，Windows 上的时间戳有约 100ns 粒度
	// 且可能滞后。TTL=1ns 时这个比较会偶发地判成"未过期"，
	// 于是测试随机失败——而失败信息看起来像产品缺陷。
	// 注入时钟让"过期"成为确定的事实。
	expired := base
	expired.Now = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	Load(context.Background(), expired)

	if hits.Load() == first {
		t.Error("an expired cache was used without refetching")
	}
}

// TestCacheExpiryUsesInjectedClock 验证过期判断走的是注入的时钟。
//
// 这条测试存在的理由：另外两个 TTL 测试曾经用 "TTL=1ns" 来制造过期，
// 结果因为文件系统时间戳粒度而偶发失败。把"过期由时钟决定"这件事
// 显式钉住，就不会再有人退回去依赖真实时间戳。
func TestCacheExpiryUsesInjectedClock(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	dir := t.TempDir()
	base := Options{Dir: dir, BaseURL: server.URL, ASNs: []string{"AS4809"}, HTTPClient: server.Client()}
	Load(context.Background(), base)
	first := hits.Load()
	if first == 0 {
		t.Fatal("the first load did not fetch anything")
	}

	// 时钟推后 30 天：缓存必定过期，必须重新抓取。
	future := base
	future.Now = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	Load(context.Background(), future)

	if hits.Load() == first {
		t.Error("a cache that expired by the injected clock was reused")
	}
}

// TestLoadFallsBackToStaleCacheOnFetchFailure 验证抓取失败时用过期缓存。'
//
// 骨干线路的 IP 段变化很慢，过期的前缀仍然比没有强。
func TestLoadFallsBackToStaleCacheOnFetchFailure(t *testing.T) {
	healthy := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	dir := t.TempDir()
	base := Options{Dir: dir, BaseURL: server.URL, ASNs: []string{"AS4809"}, HTTPClient: server.Client()}
	Load(context.Background(), base)

	// 服务坏掉 + 缓存过期（用注入时钟让过期确定发生，理由同上）。
	healthy = false
	stale := base
	stale.Now = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	var logs []string
	stale.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	resolver := Load(context.Background(), stale)
	if !resolver.Ready() {
		t.Fatal("resolver gave up instead of using the stale cache")
	}
	if got := resolver.Match("1.1.1.1"); len(got) != 1 {
		t.Errorf("Match = %v, want the stale data to still work", got)
	}
	if !containsAny(logs, "过期缓存") {
		t.Errorf("logs do not mention the stale cache fallback: %v", logs)
	}
}

// TestLoadSurvivesTotalFailure 验证全盘失败时不 panic、不阻塞。
//
// 线路名是锦上添花：拿不到不该让整轮测量失败。
func TestLoadSurvivesTotalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var logs []string
	resolver := Load(context.Background(), Options{
		Dir:        t.TempDir(),
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809", "AS4134"},
		HTTPClient: server.Client(),
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})

	if resolver.Ready() {
		t.Error("resolver claims to be ready with no data")
	}
	if got := resolver.Match("1.1.1.1"); got != nil {
		t.Errorf("Match = %v, want nil", got)
	}
	if !containsAny(logs, "不可用") {
		t.Errorf("logs do not report the failure: %v", logs)
	}
}

// TestFetchHandlesMissingAddressFamily 验证某个地址族缺失不算失败。
//
// 纯 IPv4 网络里 af=6 返回 404 完全正常。
func TestFetchHandlesMissingAddressFamily(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("af") == "6" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	resolver := Load(context.Background(), Options{
		Dir:        t.TempDir(),
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})

	if !resolver.Ready() {
		t.Fatal("a missing IPv6 family should not count as failure")
	}
	if asns, _, v4, v6 := resolver.Size(); asns != 1 || v4 != 1 || v6 != 0 {
		t.Errorf("Size = (%d,%d,%d), want 1 ASN with 1 v4 and 0 v6", asns, v4, v6)
	}
}

// TestCacheWriteIsAtomic 验证缓存写入不留半截文件。
func TestCacheWriteIsAtomic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	dir := t.TempDir()
	Load(context.Background(), Options{
		Dir: dir, BaseURL: server.URL, ASNs: []string{"AS4809"}, HTTPClient: server.Client(),
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

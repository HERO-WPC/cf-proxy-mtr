package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// testJSONBody 是内容合法的 all.json 载荷。
func testJSONBody(t *testing.T, ips ...string) []byte {
	t.Helper()

	items := make([]map[string]any, 0, len(ips))
	for _, ip := range ips {
		items = append(items, jsonItem(ip, []int{443}, realMeta()))
	}
	return jsonDoc(t, items, nil)
}

// testLoader 构造一个使用测试 HTTP 客户端的 Loader。
//
// 关键点：RetryBackoff 被压到 1ms，避免重试测试变慢；
// 超时设为 2s，保证"服务器挂起"的测试不会拖太久。
func testLoader(t *testing.T, cfg Config) *Loader {
	t.Helper()

	cfg.Timeout = 2 * time.Second
	cfg.RetryBackoff = time.Millisecond
	if cfg.Retries == 0 {
		cfg.Retries = DefaultRetries
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}

	l, err := NewLoader(cfg)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return l
}

// jsonServer 启动一个固定返回 body 的测试服务器。
func jsonServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// 网络路径
// ---------------------------------------------------------------------------

func TestLoadFromNetworkParsesAndWritesCache(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4", "5.6.7.8"))
	cachePath := filepath.Join(t.TempDir(), "all.json")

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})

	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(res.Targets))
	}
	if res.FromCache {
		t.Error("FromCache = true, want false on first run")
	}
	if !res.CacheWritten {
		t.Errorf("CacheWritten = false, want true (err=%v)", res.CacheError)
	}
	if res.Meta.URL != srv.URL {
		t.Errorf("Meta.URL = %q, want %q", res.Meta.URL, srv.URL)
	}
	if res.Meta.Format != "json" {
		t.Errorf("Meta.Format = %q, want json", res.Meta.Format)
	}
	if res.Bytes == 0 {
		t.Error("Bytes = 0, want downloaded size")
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero")
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

// TestLoadCacheFirstSkipsNetwork 验证**显式打开**缓存优先时不联网。
//
// 缓存优先现在是选项而不是默认（见 CacheFirst 的说明）：默认要每次
// 试着下载最新列表，否则使用者测的是一份最多 6 小时前的快照，
// 而且无从察觉。但"离线可复现"是真实需求（CI 用固定缓存跑固定目标），
// 因此这个开关必须仍然有效。
func TestLoadCacheFirstSkipsNetwork(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL, Format: "json"},
		ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{CacheFirst: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("network requests = %d, want 0 (cache-first must not touch the network)", got)
	}
	if !res.FromCache {
		t.Error("FromCache = false, want true")
	}
	if res.CacheWritten {
		t.Error("CacheWritten = true, want false when serving from cache")
	}
	if len(res.Targets) != 3 {
		t.Errorf("targets = %d, want 3", len(res.Targets))
	}
}

// TestLoadPrefersNetworkOverFreshCache 验证**默认**是网络优先。
//
// 这是本轮改动的核心：以前"缓存新鲜就直接返回"是默认行为，于是每次
// 运行拿到的都是最多 6 小时前的目标列表，而上游随时会增删目标。
// 新增的目标一个都测不到，且没有任何提示。
func TestLoadPrefersNetworkOverFreshCache(t *testing.T) {
	const freshTarget = "9.9.9.9"
	srv := jsonServer(t, testJSONBody(t, freshTarget))

	cachePath := filepath.Join(t.TempDir(), "all.json")
	// 刚写好的缓存，内容与服务器不同——如果它被用了，目标就是旧的。
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL, Format: "json"},
		ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.FromCache {
		t.Fatal("FromCache = true with a fresh cache present; the default must be network-first")
	}

	found := false
	for _, target := range res.Targets {
		if target.IP == freshTarget {
			found = true
		}
	}
	if !found {
		t.Errorf("downloaded targets do not contain the server's %s: %v", freshTarget, res.Targets)
	}

	// 下载成功后应当刷新缓存，让下一次失败时有更新的兜底。
	if !res.CacheWritten {
		t.Error("CacheWritten = false; a successful download should refresh the cache")
	}
}

// TestLoadFallsBackToCacheWhenNetworkFails 验证下载失败时退回缓存
// **不需要任何开关**。
//
// 断网、被墙、上游临时挂掉时，手上那份缓存仍然有用。这个兜底是
// 默认行为，因此零值 LoadOptions 就必须生效——用"否定式"字段
// （NoStaleFallback）的理由就在这里：新增调用方什么都不写也对。
func TestLoadFallsBackToCacheWhenNetworkFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	// 故意写一份很旧的缓存：兜底不看新鲜度，"能不能下到"才是问题。
	if err := writeCacheAt(t, cachePath, srv.URL, time.Now().Add(-72*time.Hour), sampleTargets(t)); err != nil {
		t.Fatalf("writeCacheAt: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load with a usable cache should not fail: %v", err)
	}

	if !res.FromCache {
		t.Error("FromCache = false, want true (fell back to cache)")
	}
	if len(res.Targets) != 3 {
		t.Errorf("targets = %d, want 3 from the cache", len(res.Targets))
	}
	// 必须把下载失败的原因带上，否则上层无法告诉使用者
	// "你看到的可能是旧数据"。
	if len(res.FetchErrors) == 0 {
		t.Error("FetchErrors is empty; the download failure reason must be preserved")
	}
}

// TestLoadNoStaleFallbackReportsFailure 验证可以显式要求"不兜底"。
func TestLoadNoStaleFallbackReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL, Format: "json"},
		ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})
	if _, err := l.Load(context.Background(), LoadOptions{NoStaleFallback: true}); err == nil {
		t.Fatal("Load succeeded although the cache fallback was disabled and the download failed")
	}
}

func TestLoadRefreshesExpiredCacheOverNetwork(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4"))
	cachePath := filepath.Join(t.TempDir(), "all.json")

	// 写入一份"很久以前"的缓存（内容与服务器不同）。
	if err := writeCacheAt(t, cachePath, srv.URL, time.Now().Add(-48*time.Hour), sampleTargets(t)); err != nil {
		t.Fatalf("writeCacheAt: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{CacheTTL: time.Hour})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.FromCache {
		t.Error("FromCache = true, want false (cache expired)")
	}
	if len(res.Targets) != 1 || res.Targets[0].ID != "1.2.3.4:443" {
		t.Errorf("targets = %+v, want fresh network data", res.Targets)
	}
}

func TestLoadRefreshFlagBypassesFreshCache(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4"))
	cachePath := filepath.Join(t.TempDir(), "all.json")

	// 缓存是新鲜的（刚写入），但 --refresh 必须忽略它。
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL}, ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{Refresh: true, ForceFetch: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.FromCache {
		t.Error("FromCache = true, want false with --refresh")
	}
	if len(res.Targets) != 1 {
		t.Errorf("targets = %d, want 1 from network", len(res.Targets))
	}
}

// TestLoadHonorsEmptyCache 验证"合法但空"的缓存会被当作有效缓存。
//
// 这是回归测试：早期实现把"缓存里 0 个目标"当成未命中，于是联网重下一次。
// 后果有两个：
//
//  1. 数据源确实返回空列表时，每次运行都要重新下载（慢，且与"缓存有效"的语义矛盾）；
//  2. 离线环境行为不可预期：明明有缓存，却因为"它是空的"而去联网。
//
// 空列表是否可用应当由调用方决定，而不是由缓存层替它猜。
func TestLoadHonorsEmptyCache(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write(testJSONBody(t, "1.2.3.4"))
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	// 写一份空目标列表的缓存（源地址与即将使用的源一致）。
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL, Format: "json"}, ParseStats{}, nil); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	// CacheFirst：这条测的是"空缓存算不算有效缓存"，
	// 因此必须显式走缓存那条路（默认已经是网络优先）。
	res, err := l.Load(context.Background(), LoadOptions{CacheFirst: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("network requests = %d, want 0 (an empty cache is still a valid cache)", got)
	}
	if !res.FromCache {
		t.Error("FromCache = false, want true")
	}
	if len(res.Targets) != 0 {
		t.Errorf("targets = %d, want 0", len(res.Targets))
	}
}

// TestLoadMalformedCacheFallsBackToNetwork 验证损坏的缓存仍然算未命中。
func TestLoadMalformedCacheFallsBackToNetwork(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4"))

	cachePath := filepath.Join(t.TempDir(), "all.json")
	if err := os.WriteFile(cachePath, []byte(`{"schema_version":2,"targets":[{"id":"1.2.3`), 0o644); err != nil {
		t.Fatal(err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.FromCache {
		t.Error("FromCache = true, want false for a corrupt cache")
	}
	if len(res.Targets) != 1 {
		t.Errorf("targets = %d, want 1 from the network", len(res.Targets))
	}
}

func TestLoadCacheFromDifferentSourceIsNotReused(t *testing.T) {
	// 缓存记录的是"另一个数据源"，即使新鲜也不能当作当前源的数据。
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4"))
	cachePath := filepath.Join(t.TempDir(), "all.json")

	if err := WriteCache(cachePath, SourceMeta{URL: "https://other.test/all.json"},
		ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.FromCache {
		t.Error("FromCache = true, want false (cache belongs to another source)")
	}
	if res.Meta.URL != srv.URL {
		t.Errorf("Meta.URL = %q, want %q", res.Meta.URL, srv.URL)
	}
}

// TestLoadCacheFromFallbackSourceIsNotReusedForPrimary 是一个回归测试。
//
// 备用源（all.txt）与主源（all.json）的内容不同：如果缓存只与
// "主源或备用源"之一匹配就算命中，就会出现
// "--url all.json 却返回 all.txt 解析结果"的静默数据替换。
//
// 约定：缓存只与当前**主源**匹配。
func TestLoadCacheFromFallbackSourceIsNotReusedForPrimary(t *testing.T) {
	primary := jsonServer(t, testJSONBody(t, "1.2.3.4"))
	var fallbackHits int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallbackHits, 1)
		_, _ = w.Write([]byte("9.9.9.9:2053#JP\n"))
	}))
	defer fallback.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	l := testLoader(t, Config{URL: primary.URL, FallbackURL: fallback.URL, CachePath: cachePath})

	// 第一次：主源返回 500，走备用源，缓存记录备用源地址。
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	brokenLoader := testLoader(t, Config{URL: broken.URL, FallbackURL: fallback.URL, CachePath: cachePath, Retries: 0})
	seed, err := brokenLoader.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("seed Load: %v", err)
	}
	if seed.Meta.URL != fallback.URL {
		t.Fatalf("seed source = %q, want fallback", seed.Meta.URL)
	}
	if got := atomic.LoadInt32(&fallbackHits); got != 1 {
		t.Fatalf("fallback hits = %d, want 1", got)
	}

	// 第二次：主源可用，缓存属于备用源，必须重新下载主源。
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.FromCache {
		t.Error("FromCache = true: fallback-source cache must not satisfy a primary-source request")
	}
	if res.Meta.URL != primary.URL {
		t.Errorf("Meta.URL = %q, want primary %q", res.Meta.URL, primary.URL)
	}
	if res.Meta.Format != "json" {
		t.Errorf("Format = %q, want json (not the text fallback)", res.Meta.Format)
	}
	if got := atomic.LoadInt32(&fallbackHits); got != 1 {
		t.Errorf("fallback hits = %d, want still 1 (primary must succeed)", got)
	}
}

// ---------------------------------------------------------------------------
// 备用数据源与降级
// ---------------------------------------------------------------------------

func TestLoadFallsBackToSecondarySource(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not here"))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("9.9.9.9:2053#JP\n8.8.8.8:443#US\n"))
	}))
	defer fallback.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	l := testLoader(t, Config{
		URL:         primary.URL,
		FallbackURL: fallback.URL,
		CachePath:   cachePath,
	})

	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if res.Meta.URL != fallback.URL {
		t.Errorf("Meta.URL = %q, want fallback %q", res.Meta.URL, fallback.URL)
	}
	if res.Meta.Format != "text" {
		t.Errorf("Meta.Format = %q, want text", res.Meta.Format)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(res.Targets))
	}
	if res.Targets[0].Port != 2053 {
		t.Errorf("port = %d, want 2053 (must not be forced to 443)", res.Targets[0].Port)
	}
	// 主源失败的原因必须被保留，便于诊断。
	if len(res.FetchErrors) == 0 || !strings.Contains(strings.Join(res.FetchErrors, " "), primary.URL) {
		t.Errorf("FetchErrors = %v, want primary failure recorded", res.FetchErrors)
	}
}

func TestLoadMalformedPrimaryFallsBackToSecondary(t *testing.T) {
	// 主源返回 200 但内容是 HTML（典型的中间层错误页）：
	// 必须判定为失败并继续尝试备用源，而不是把它当数据解析。
	primary := jsonServer(t, []byte("<!DOCTYPE html><html><body>maintenance</body></html>"))
	fallback := jsonServer(t, testJSONBody(t, "1.2.3.4"))

	cachePath := filepath.Join(t.TempDir(), "all.json")
	l := testLoader(t, Config{URL: primary.URL, FallbackURL: fallback.URL, CachePath: cachePath})

	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Meta.URL != fallback.URL {
		t.Errorf("Meta.URL = %q, want fallback", res.Meta.URL)
	}
	if len(res.Targets) != 1 {
		t.Errorf("targets = %d, want 1", len(res.Targets))
	}
}

func TestLoadAllowsStaleCacheWhenNetworkFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	writtenAt := time.Now().Add(-30 * 24 * time.Hour)
	if err := writeCacheAt(t, cachePath, srv.URL, writtenAt, sampleTargets(t)); err != nil {
		t.Fatalf("writeCacheAt: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !res.FromCache {
		t.Error("FromCache = false, want true")
	}
	if !res.Stale {
		t.Error("Stale = false, want true for 30-day-old cache")
	}
	if len(res.Targets) != 3 {
		t.Errorf("targets = %d, want 3", len(res.Targets))
	}
	// 使用过期缓存时必须给出网络失败的原因。
	if len(res.FetchErrors) == 0 {
		t.Error("FetchErrors is empty, want recorded fetch failure")
	}
}

func TestLoadRejectsStaleCacheWhenNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL}, ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})
	if _, err := l.Load(context.Background(), LoadOptions{NoStaleFallback: true, Refresh: true}); err == nil {
		t.Fatal("Load succeeded, want error when network fails and stale cache is disallowed")
	}
}

func TestLoadFailsWhenNetworkAndCacheUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "missing.json")
	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})

	_, err := l.Load(context.Background(), LoadOptions{})
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	// 失败信息必须包含每个数据源地址与原因，做到"下载失败明确提示"。
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error = %v, want source url included", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want http status included", err)
	}
}

func TestLoadMalformedResponseReportsFormatIssue(t *testing.T) {
	// 200 + 非空但无法解析的内容：必须报出解析错误，并且不覆盖已有缓存。
	srv := jsonServer(t, []byte(`{"data": [{"ip": "1.2.3.4"`)) // 截断的 JSON
	cachePath := filepath.Join(t.TempDir(), "all.json")

	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL}, ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath, Retries: 0})
	if _, err := l.Load(context.Background(), LoadOptions{Refresh: true, NoStaleFallback: true}); err == nil {
		t.Fatal("Load succeeded, want parse error")
	}

	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("cache was overwritten by a failed parse; existing cache must be preserved")
	}
}

func TestLoadCacheWriteFailureIsNotFatal(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4"))

	// 把缓存路径指向一个"已存在的目录"，写入必然失败，
	// 但目标列表仍然必须可用（缓存只是加速手段）。
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: blocked})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(res.Targets) != 1 {
		t.Errorf("targets = %d, want 1 even when cache write fails", len(res.Targets))
	}
	if res.CacheWritten {
		t.Error("CacheWritten = true, want false")
	}
	if res.CacheError == nil {
		t.Error("CacheError = nil, want recorded write failure")
	}
}

// ---------------------------------------------------------------------------
// 重试
// ---------------------------------------------------------------------------

func TestFetchRetriesTransientFailure(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway) // 5xx 应当被重试
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(testJSONBody(t, "1.2.3.4"))
	}))
	defer srv.Close()

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: filepath.Join(t.TempDir(), "all.json")})
	res, err := l.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Errorf("requests = %d, want 3 (two failures then success)", got)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if len(res.Targets) != 1 {
		t.Errorf("targets = %d, want 1", len(res.Targets))
	}
}

func TestFetchDoesNotRetryPermanentFailure(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.NotFound(w, r) // 4xx 不应重试
	}))
	defer srv.Close()

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: filepath.Join(t.TempDir(), "all.json")})
	if _, err := l.Load(context.Background(), LoadOptions{}); err == nil {
		t.Fatal("Load succeeded, want error")
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("requests = %d, want 1 (4xx must not be retried)", got)
	}
}

func TestFetchStopsAfterExhaustingRetries(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	l := testLoader(t, Config{
		URL:          srv.URL,
		FallbackURL:  "",
		CachePath:    filepath.Join(t.TempDir(), "all.json"),
		Retries:      3,
		RetryBackoff: time.Millisecond,
	})
	_, err := l.Load(context.Background(), LoadOptions{})
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}

	if got := atomic.LoadInt32(&requests); got != 4 { // 1 次 + 3 次重试
		t.Errorf("requests = %d, want 4", got)
	}
	if !strings.Contains(err.Error(), "all data sources failed") {
		t.Errorf("error = %v, want aggregated failure message", err)
	}
}

func TestFetchCancellationStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 挂起直到客户端放弃，触发可重试的超时/取消错误。
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	l := testLoader(t, Config{
		URL:          srv.URL,
		FallbackURL:  "",
		CachePath:    filepath.Join(t.TempDir(), "all.json"),
		Retries:      5,
		RetryBackoff: 50 * time.Millisecond,
		Timeout:      5 * time.Second,
	})

	start := time.Now()
	if _, err := l.Load(ctx, LoadOptions{Refresh: true}); err == nil {
		t.Fatal("Load succeeded, want error after cancellation")
	}
	// 取消后必须迅速返回，而不是继续等待 5 次重试。
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Load took %s after cancel, want fast return", elapsed)
	}
}

// ---------------------------------------------------------------------------
// FetchOnly / LoadRaw
// ---------------------------------------------------------------------------

func TestFetchOnlyIgnoresCache(t *testing.T) {
	srv := jsonServer(t, testJSONBody(t, "1.2.3.4", "5.6.7.8"))
	cachePath := filepath.Join(t.TempDir(), "all.json")

	// 预置一份"新鲜但内容不同"的缓存。
	if err := WriteCache(cachePath, SourceMeta{URL: srv.URL}, ParseStats{}, sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	l := testLoader(t, Config{URL: srv.URL, FallbackURL: "", CachePath: cachePath})
	res, err := l.FetchOnly(context.Background())
	if err != nil {
		t.Fatalf("FetchOnly: %v", err)
	}

	if res.FromCache {
		t.Error("FromCache = true, want false")
	}
	if res.CacheWritten {
		t.Error("CacheWritten = true, want false")
	}
	if len(res.Targets) != 2 {
		t.Errorf("targets = %d, want 2 from network", len(res.Targets))
	}
	if len(res.Raw) == 0 {
		t.Error("Raw is empty, want raw body in FetchOnly mode")
	}

	// 缓存不能被 FetchOnly 修改。
	_, _, cached, _, err := ReadCache(cachePath)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if len(cached) != 3 {
		t.Errorf("cached targets = %d, want 3 (unchanged)", len(cached))
	}
}

func TestLoadRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	raw, err := LoadRaw(path)
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if raw.Source != sampleMeta().URL {
		t.Errorf("Source = %q, want %q", raw.Source, sampleMeta().URL)
	}
	if raw.WrittenAt.IsZero() {
		t.Error("WrittenAt is zero")
	}
	if !json.Valid(raw.JSON) {
		t.Error("Raw.JSON is not valid json")
	}

	if _, err := LoadRaw(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadRaw succeeded for missing file, want error")
	}
}

// ---------------------------------------------------------------------------
// 配置校验
// ---------------------------------------------------------------------------

func TestNewLoaderValidatesConfig(t *testing.T) {
	if _, err := NewLoader(Config{URL: "://bad"}); err == nil {
		t.Error("NewLoader accepted invalid url, want error")
	}
	if _, err := NewLoader(Config{URL: "https://ok.test/x", Proxy: "://bad"}); err == nil {
		t.Error("NewLoader accepted invalid proxy, want error")
	}
}

func TestConfigNormalizeFillsDefaults(t *testing.T) {
	cfg := Config{}
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.URL != DefaultURL || cfg.CachePath != DefaultCachePath {
		t.Errorf("cfg = %+v, want defaults", cfg)
	}
	if cfg.Timeout != DefaultTimeout || cfg.RetryBackoff != DefaultRetryBackoff {
		t.Errorf("cfg = %+v, want default timeouts", cfg)
	}
	if cfg.Retries != 0 {
		// 零值被规范化为 0（不重试），默认重试次数由 DefaultConfig 提供。
		t.Errorf("Retries = %d, want 0", cfg.Retries)
	}

	def := DefaultConfig()
	if def.Retries != DefaultRetries {
		t.Errorf("DefaultConfig().Retries = %d, want %d", def.Retries, DefaultRetries)
	}
}

func TestConfigURLsOrderAndDedup(t *testing.T) {
	cfg := Config{URL: "https://a.test/x", FallbackURL: "https://b.test/x"}
	if got := cfg.urls(); len(got) != 2 || got[0] != cfg.URL || got[1] != cfg.FallbackURL {
		t.Errorf("urls() = %v, want [primary, fallback]", got)
	}

	same := Config{URL: "https://a.test/x", FallbackURL: "https://a.test/x"}
	if got := same.urls(); len(got) != 1 {
		t.Errorf("urls() = %v, want single entry when primary == fallback", got)
	}

	nofb := Config{URL: "https://a.test/x"}
	if got := nofb.urls(); len(got) != 1 {
		t.Errorf("urls() = %v, want single entry when no fallback", got)
	}
}

func TestContentLengthHint(t *testing.T) {
	cases := map[int]string{
		10:       "10 B",
		2048:     "2.00 KiB",
		10415698: "9.93 MiB",
	}
	for in, want := range cases {
		if got := ContentLengthHint(in); got != want {
			t.Errorf("ContentLengthHint(%d) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// writeCacheAt 写入缓存，但把 written_at 覆盖为指定时间。
//
// 用于构造"过期缓存"场景，而不必真的等待。
func writeCacheAt(t *testing.T, path, sourceURL string, writtenAt time.Time, targets []model.Target) error {
	t.Helper()

	if err := WriteCache(path, SourceMeta{URL: sourceURL, Format: "json"}, ParseStats{}, targets); err != nil {
		return err
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		return err
	}
	doc["written_at"] = writtenAt.UTC().Format(time.RFC3339Nano)

	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("rewrite cache: %w", err)
	}
	return nil
}

package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchDoesNotReuseCacheFromAnotherSource 验证缓存来源与当前主数据源不一致时
// 不会复用缓存。
//
// 这是 bug 修复后的回归测试，覆盖两种"来源不同"的情形：
//
//  1. 缓存来自另一个主源（缓存只与当前主源比较）；
//  2. 缓存来自本次运行的**备用源**（绝不能把它当成主源的缓存命中，
//     否则会出现 "--url all.json 却返回 all.txt 解析结果" 的静默替换）。
func TestFetchDoesNotReuseCacheFromAnotherSource(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixture()))
	}))
	defer srv.Close()

	// 备用源：内容与主源不同（缺少坐标等信息），用于检测"静默替换"。
	var fallbackRequests int
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackRequests++
		_, _ = w.Write([]byte("9.9.9.9:2053#JP\n"))
	}))
	defer fallback.Close()

	cachePath := filepath.Join(t.TempDir(), "cache.json")

	// 步骤 1：主源不可用，备用源成功 -> 缓存里记录的是备用源地址。
	code, _, stderr := runCLI("fetch",
		"--url", "http://127.0.0.1:1/all.json",
		"--fallback-url", fallback.URL,
		"--cache", cachePath,
		"--retries", "0",
		"--timeout", "1s")
	if code != ExitCodeOK {
		t.Fatalf("seed run exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if fallbackRequests == 0 {
		t.Fatal("fallback source was never used; test setup is wrong")
	}

	// 步骤 2：主源与备用源都可用，缓存路径不变。
	// 缓存记录的是备用源，不是主源，因此必须重新下载主源数据。
	code, stdout, stderr := runCLI("fetch",
		"--url", srv.URL,
		"--fallback-url", fallback.URL,
		"--cache", cachePath,
		"--retries", "0",
		"--timeout", "5s")
	if code != ExitCodeOK {
		t.Fatalf("second run exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	if requests == 0 {
		t.Error("no request reached the primary source; a cache from another source was reused")
	}
	if !strings.Contains(stdout, srv.URL) {
		t.Errorf("stdout should report the primary source url:\n%s", stdout)
	}
	if strings.Contains(stdout, "origin:       cache") {
		t.Errorf("stdout reports a cache hit, but the cache belongs to the fallback source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "format:       json") {
		t.Errorf("stdout should report json format, not the text source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "origin:       network") {
		t.Errorf("stdout should report network origin:\n%s", stdout)
	}
}

// TestFetchReusesCacheFromSamePrimary 验证来源匹配时缓存仍然生效（修复不能过度）。
func TestFetchReusesCacheFromSamePrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fetchFixture()))
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "cache.json")
	// --source-cache-first：这条测的就是"缓存能被复用"。
	// 默认已经是网络优先（fetch 的语义就是去下载），
	// 因此要验缓存复用必须显式要求。
	args := []string{"fetch", "--url", srv.URL, "--fallback-url", "", "--cache", cachePath,
		"--retries", "0", "--timeout", "5s", "--source-cache-first"}

	if code, _, stderr := runCLI(args...); code != ExitCodeOK {
		t.Fatalf("first run exit code = %d, want 0 (stderr=%q)", code, stderr)
	}

	code, stdout, stderr := runCLI(args...)
	if code != ExitCodeOK {
		t.Fatalf("second run exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "origin:       cache") {
		t.Errorf("second run should hit the cache from the same source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "targets:      3") {
		t.Errorf("cached target count wrong:\n%s", stdout)
	}
}

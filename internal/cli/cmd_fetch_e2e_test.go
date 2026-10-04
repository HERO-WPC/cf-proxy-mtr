package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fetchFixture 返回一份最小的 all.json 内容。
//
// 结构与生产数据一致：port 是数组、meta 含 colo、经纬度是字符串。
func fetchFixture() string {
	return `{
  "generated_at": "2026-10-03T03:52:35.771190",
  "list": {"country": {"US": 2}, "ips": 2},
  "data": [
    {
      "ip": "1.2.3.4",
      "port": [443, 8443],
      "meta": {
        "hostname": "speed.cloudflare.com",
        "asn": 35280,
        "country": "US",
        "city": "Chicago",
        "region": "Illinois",
        "latitude": "41.85003",
        "longitude": "-87.65005",
        "colo": {"iata": "ORD", "lat": 41.9786, "lon": -87.9048, "cca2": "US", "region": "North America", "city": "Chicago"},
        "_port": 443,
        "country_en": "United States"
      }
    },
    {
      "ip": "5.6.7.8",
      "port": [2053],
      "meta": {
        "country": "JP",
        "city": "Tokyo",
        "latitude": "35.6895",
        "longitude": "139.69171",
        "colo": {"iata": "NRT", "lat": 35.764702, "lon": 140.386002, "cca2": "JP", "region": "Asia Pacific", "city": "Tokyo"},
        "_port": 2053,
        "country_en": "Japan"
      }
    }
  ]
}`
}

// TestFetchEndToEndAgainstLocalServer 用本地 HTTP 服务器验证整条链路：
//
//	HTTP GET -> 解析 -> 缓存写入 -> 命令行输出
//
// 这样 CLI 层的参数解析、错误处理与输出格式都被真实执行到，
// 而不需要访问外网。
func TestFetchEndToEndAgainstLocalServer(t *testing.T) {
	var gotPath, gotUserAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fetchFixture()))
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")

	code, stdout, stderr := runCLI("fetch",
		"--url", srv.URL+"/all.json",
		"--fallback-url", "",
		"--cache", cachePath,
		"--retries", "0",
		"--timeout", "5s")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on success", stderr)
	}

	// 输出必须包含关键事实，便于用户核对。
	for _, want := range []string{
		"source:",
		srv.URL + "/all.json",
		"format:       json",
		"origin:       network",
		"targets:      3",
		"generated_at: 2026-10-03T03:52:35Z",
		"cache:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\ngot:\n%s", want, stdout)
		}
	}

	if gotPath != "/all.json" {
		t.Errorf("requested path = %q, want /all.json", gotPath)
	}
	if !strings.HasPrefix(gotUserAgent, "cf-route-tester") {
		t.Errorf("user-agent = %q, want cf-route-tester prefix", gotUserAgent)
	}

	// 缓存必须真实落盘，且能被第二次调用命中。
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// --source-cache-first：这里要验的是"第二次能命中缓存"。
	code2, stdout2, _ := runCLI("fetch",
		"--url", srv.URL+"/all.json",
		"--fallback-url", "",
		"--cache", cachePath,
		"--timeout", "5s",
		"--source-cache-first")
	if code2 != ExitCodeOK {
		t.Fatalf("second run exit code = %d, want 0", code2)
	}
	if !strings.Contains(stdout2, "origin:       cache") {
		t.Errorf("second run should hit cache, got:\n%s", stdout2)
	}
	if !strings.Contains(stdout2, "targets:      3") {
		t.Errorf("cached target count wrong:\n%s", stdout2)
	}
}

// TestFetchFallbackPathAgainstLocalServers 验证主源失败时自动使用备用源。
func TestFetchFallbackPathAgainstLocalServers(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("9.9.9.9:2053#JP\n8.8.8.8:443#US\n"))
	}))
	defer fallback.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	code, stdout, stderr := runCLI("fetch",
		"--url", primary.URL,
		"--fallback-url", fallback.URL,
		"--cache", cachePath,
		"--retries", "0",
		"--timeout", "5s")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "format:       text") {
		t.Errorf("stdout should report text format:\n%s", stdout)
	}
	if !strings.Contains(stdout, fallback.URL) {
		t.Errorf("stdout should report fallback source url:\n%s", stdout)
	}
	if !strings.Contains(stdout, "targets:      2") {
		t.Errorf("stdout target count wrong:\n%s", stdout)
	}
}

// TestFetchNoCacheDoesNotWrite 验证 --no-cache 不落盘。
func TestFetchNoCacheDoesNotWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fetchFixture()))
	}))
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), "all.json")
	code, stdout, stderr := runCLI("fetch",
		"--url", srv.URL,
		"--fallback-url", "",
		"--cache", cachePath,
		"--retries", "0",
		"--timeout", "5s",
		"--no-cache")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr)
	}
	if _, err := os.Stat(cachePath); err == nil {
		t.Error("cache file was written despite --no-cache")
	}
	if !strings.Contains(stdout, "disabled (--no-cache)") {
		t.Errorf("stdout should mention disabled cache:\n%s", stdout)
	}
}

// TestFetchMalformedJSONFromServerIsRuntimeError 验证格式错误有明确提示。
func TestFetchMalformedJSONFromServerIsRuntimeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"ip": "1.2.3.4"`)) // 截断
	}))
	defer srv.Close()

	code, _, stderr := runCLI("fetch",
		"--url", srv.URL,
		"--fallback-url", "",
		"--cache", filepath.Join(t.TempDir(), "all.json"),
		"--retries", "0",
		"--timeout", "5s",
		"--no-cache")

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeError)
	}
	if !strings.Contains(stderr, "parse json") {
		t.Errorf("stderr = %q, want parse error mention", stderr)
	}
	if !strings.Contains(stderr, srv.URL) {
		t.Errorf("stderr = %q, want source url included", stderr)
	}
}

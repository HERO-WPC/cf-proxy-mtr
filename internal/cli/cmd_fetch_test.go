package cli

import (
	"strings"
	"testing"
)

func TestFetchHelpListsFlags(t *testing.T) {
	code, stdout, stderr := runCLI("fetch", "--help")

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	// 帮助里的参数说明直接来自 FlagSet，必须覆盖关键参数。
	for _, want := range []string{"Flags:", "-url", "-cache", "-refresh", "-proxy", "-timeout", "-retries", "-no-cache"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("fetch help missing %q\ngot:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "cf-route-tester fetch [flags]") {
		t.Errorf("fetch help missing usage line\ngot:\n%s", stdout)
	}
}

func TestFetchUnknownFlagIsUsageError(t *testing.T) {
	code, stdout, stderr := runCLI("fetch", "--nope")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "-nope") {
		t.Errorf("stderr = %q, want the offending flag name", stderr)
	}
}

func TestFetchUnexpectedPositionalArgumentIsUsageError(t *testing.T) {
	code, _, stderr := runCLI("fetch", "extra-arg")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("stderr = %q, want unexpected argument message", stderr)
	}
}

func TestFetchInvalidURLIsRuntimeError(t *testing.T) {
	// 配置非法属于全局错误：退出码 1（运行期错误），而不是 2（用法错误）。
	code, _, stderr := runCLI("fetch", "--url", "://bad", "--no-cache")

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if !strings.Contains(stderr, "invalid source url") {
		t.Errorf("stderr = %q, want invalid url message", stderr)
	}
}

func TestFetchInvalidProxyIsRuntimeError(t *testing.T) {
	code, _, stderr := runCLI("fetch",
		"--url", "https://example.test/all.json",
		"--proxy", "://bad",
		"--no-cache")

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if !strings.Contains(stderr, "invalid proxy url") {
		t.Errorf("stderr = %q, want invalid proxy message", stderr)
	}
}

// TestFetchNetworkFailureIsRuntimeError 用一个必然失败的地址验证：
// 下载失败会返回明确的错误提示与退出码 1，而不是 panic 或静默成功。
//
// 使用 127.0.0.1 上的保留端口并显式关闭重试，避免测试变慢或依赖外网。
func TestFetchNetworkFailureIsRuntimeError(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped in short mode")
	}

	code, stdout, stderr := runCLI("fetch",
		"--url", "http://127.0.0.1:1/all.json",
		"--fallback-url", "",
		"--cache", t.TempDir()+"/all.json",
		"--retries", "0",
		"--timeout", "1s",
		"--no-cache",
		"--allow-stale=false")

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, ExitCodeError, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "all data sources failed") {
		t.Errorf("stderr = %q, want aggregated fetch failure", stderr)
	}
}

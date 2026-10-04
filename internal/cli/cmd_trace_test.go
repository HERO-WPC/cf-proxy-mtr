package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖 `trace` 命令的**命令行表面**。
//
// 注意分工：
//
//	internal/trace        引擎实现、参数构造、JSON 解析、失败分类
//	（真实输出夹具在 internal/trace/testdata/）
//	本文件                 参数校验、错误提示、汇总输出、引擎缺失时的行为
//
// 这些测试刻意不依赖真实的 nexttrace 二进制：
// CI 里没有它，而"命令能否正确报错/报提示"不该依赖外部程序是否安装。

func TestTraceHelpExplainsEngineAndPermissions(t *testing.T) {
	code, stdout, stderr := runCLI("trace", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"-target",
		"-binary",
		"-mode",
		"-workers",
		"-timeout",
		"-json",
		"-verbose",
		// 引擎调用方式必须写清楚：端口取自目标本身。
		"--port",
		"不会固定成 443",
		// 权限要求必须提前说明，否则用户会把它当成线路问题。
		"管理员权限",
		"WinDivert",
		"--init",
		// 失败分类的说明。
		"permission_denied",
		"timeout",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("trace help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// emptyCachePath 写一份**合法但为空**的目标列表缓存。
//
// 必须用"合法空缓存"而不是"不存在的缓存文件"：后者会让
// source.Loader 认为缓存不可用，转而联网下载（并在失败后重试），
// 测试于是变得又慢又依赖网络。这是本文件最初踩过的坑——
// 两个测试因此跑了 3 秒与 200 秒。
func emptyCachePath(t *testing.T) string {
	t.Helper()
	return writeRawCacheFromJSON(t, `{"generated_at":"2026-10-03T00:00:00","list":{"ips":0},"data":[]}`)
}

// TestTraceRequiresTargets 验证不给目标时是用法错误，而不是静默什么都不做。
func TestTraceRequiresTargets(t *testing.T) {
	code, _, stderr := runCLI("trace",
		"--cache", emptyCachePath(t),
		"--url", sourceDefaultURL(),
		"--fallback-url", "",
		"--source-retries", "0",
		"--quiet")

	if code == ExitCodeOK {
		t.Fatal("trace succeeded without any target")
	}
	// 明确说明怎么给目标，而不是一句含糊的失败。
	if !strings.Contains(stderr, "--target") {
		t.Errorf("stderr = %q, want it to explain how to supply a target", stderr)
	}
}

// TestTraceRejectsBadMode 验证非法模式是用法错误。
func TestTraceRejectsBadMode(t *testing.T) {
	code, _, stderr := runCLI("trace",
		"--target", "1.1.1.1:443",
		"--mode", "gre",
		"--quiet")

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
	}
	if !strings.Contains(stderr, "trace mode") {
		t.Errorf("stderr = %q, want it to mention the trace mode", stderr)
	}
	// 必须列出可用的模式。
	for _, mode := range []string{"tcp", "icmp", "udp"} {
		if !strings.Contains(stderr, mode) {
			t.Errorf("stderr = %q, want %q listed as a valid mode", stderr, mode)
		}
	}
}

// TestTraceRejectsInvalidTarget 验证非法目标被明确拒绝。
func TestTraceRejectsInvalidTarget(t *testing.T) {
	cases := []string{"not-an-ip", "1.2.3.4:99999", "1.2.3.4:0"}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			code, _, stderr := runCLI("trace", "--target", bad, "--quiet")
			if code == ExitCodeOK {
				t.Fatalf("trace accepted the invalid target %q", bad)
			}
			if !strings.Contains(stderr, bad) && !strings.Contains(stderr, "invalid") {
				t.Errorf("stderr = %q, want it to name the bad target", stderr)
			}
		})
	}
}

// TestTraceMissingEnginePointsAtInstallation 验证没有 NextTrace 时
// 给出可操作的提示，而不是一句 "not found"。
//
// 这条路径在 CI 与大多数用户机器上都会走到（他们没装 NextTrace），
// 因此提示的措辞很重要。
func TestTraceMissingEnginePointsAtInstallation(t *testing.T) {
	code, _, stderr := runCLI("trace",
		"--target", "1.1.1.1:443",
		"--binary", "definitely-not-installed-nexttrace-xyz",
		// 同上：测试不下载。
		"--no-download",
		"--quiet")

	if code == ExitCodeOK {
		t.Fatal("trace succeeded without a trace engine")
	}
	// 必须是运行期错误（1），不是用法错误（2）：
	// 参数没写错，是环境缺东西。
	if code != ExitCodeError {
		t.Errorf("exit code = %d, want %d (a missing engine is an environment problem, not a usage error)",
			code, ExitCodeError)
	}
	for _, want := range []string{"NextTrace not found.", "install NextTrace", "nexttrace.binary"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}

// TestTraceSummaryReportsZeroAttempts 验证引擎可用但目标全部无法跟踪时，
// 汇总仍然给出完整的结构（而不是空白输出）。
//
// 这里用一个必然失败的引擎路径（不存在的二进制），
// 因此实际上走的是"引擎缺失"分支——真正"引擎在但目标不通"的
// 行为由 internal/trace 的测试覆盖。
func TestTraceJSONOutputIsSuppressedOnFailure(t *testing.T) {
	code, stdout, _ := runCLI("trace",
		"--target", "1.1.1.1:443",
		"--binary", "definitely-not-installed-nexttrace-xyz",
		// 同上：测试不下载。
		"--no-download",
		"--json", "--quiet")

	if code == ExitCodeOK {
		t.Fatal("trace succeeded without a trace engine")
	}
	// 失败时不该输出半截 JSONL（那会让下游解析到不完整的批次）。
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

// TestTraceAcceptsMultipleTargets 验证 --target 可重复。
func TestTraceAcceptsMultipleTargets(t *testing.T) {
	// 只验证解析层接受多个 --target：用不存在的二进制让它在
	// "构造引擎"这一步就失败，从而不真的启动任何进程。
	code, _, stderr := runCLI("trace",
		"--target", "1.1.1.1:443",
		"--target", "8.8.8.8:53",
		"--binary", "definitely-not-installed-nexttrace-xyz",
		// 同上：测试不下载。
		"--no-download",
		"--quiet")

	if code == ExitCodeUsage {
		t.Fatalf("multiple --target values were rejected as a usage error: %s", stderr)
	}
	// 失败原因必须是"引擎不存在"，而不是"参数不对"。
	if !strings.Contains(stderr, "NextTrace not found.") {
		t.Errorf("stderr = %q, want the missing-engine message", stderr)
	}
}

// TestTraceAndScanShareEngineOptions 验证 scan --trace 与 trace
// 用同一套引擎选项（模式校验一致）。
func TestScanTraceRejectsBadMode(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runCLI("scan",
		"--cache", emptyCachePath(t),
		"--url", sourceDefaultURL(),
		"--fallback-url", "",
		"--source-retries", "0",
		"--out", filepath.Join(dir, "results.csv"),
		"--identity", filepath.Join(dir, "c.json"),
		"--trace", "--trace-mode", "gre",
		"--quiet")

	if code == ExitCodeOK {
		t.Fatal("scan accepted an invalid --trace-mode")
	}
	if !strings.Contains(stderr, "trace mode") {
		t.Errorf("stderr = %q, want it to mention the trace mode", stderr)
	}
}

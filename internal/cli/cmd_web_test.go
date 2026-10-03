package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖 `web` 命令的**命令行表面**。
//
// 刻意不真的启动服务：那需要占用端口、开浏览器，而且会让测试
// 依赖网络与图形环境。服务的真实行为由 internal/webui 的测试覆盖
// （那里会真的起监听、真的发 HTTP 请求）。

// TestWebHelpExplainsSecurityAndLogs 验证帮助里说明了
// 安全边界与日志位置。
//
// 这两件事都是"用户不问就不会知道、出事时才发现"的类型：
// 把服务暴露到局域网、以及脱离终端后日志在哪。
func TestWebHelpExplainsSecurityAndLogs(t *testing.T) {
	code, stdout, stderr := runCLI("web", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		// 参数
		"-listen", "-db", "-identity", "-log-dir", "-log-file",
		"-no-browser", "-verbose",
		// 数据源参数（与 probe/scan 共用）
		"-url", "-cache", "-proxy",
		// 安全说明
		"令牌",
		"127.0.0.1",
		"局域网",
		// 日志说明
		"日志会写入",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("web help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

// TestWebDefaultListenIsLoopback 验证默认只绑本机。
//
// 默认值就是安全边界：改成 0.0.0.0 会把"从本机发起大量网络连接"
// 的能力暴露给整个局域网。这个默认值不该被随手改掉。
func TestWebDefaultListenIsLoopback(t *testing.T) {
	var p webParams
	webFlagSet(&p)

	if p.listen != "127.0.0.1:0" {
		t.Errorf("default --listen = %q, want 127.0.0.1:0", p.listen)
	}
	if !strings.HasPrefix(p.listen, "127.0.0.1:") {
		t.Errorf("default --listen = %q, which is not loopback", p.listen)
	}
}

// TestWebDefaultLogDirIsSet 验证默认会写日志文件。
//
// 脱离命令行运行时没有 stderr，日志文件是唯一的排查入口，
// 因此它必须是**默认开启**的，而不是需要用户额外指定。
func TestWebDefaultLogDirIsSet(t *testing.T) {
	var p webParams
	webFlagSet(&p)

	if strings.TrimSpace(p.logDir) == "" {
		t.Error("default --log-dir is empty; a GUI started without a terminal would leave no trace")
	}
	if strings.TrimSpace(p.logFile) == "" {
		t.Error("default --log-file is empty")
	}
}

// TestWebRejectsUnknownFlag 验证未知参数是用法错误。
func TestWebRejectsUnknownFlag(t *testing.T) {
	code, _, stderr := runCLI("web", "--definitely-not-a-flag")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
	}
	if !strings.Contains(stderr, "not defined") {
		t.Errorf("stderr = %q, want it to mention the unknown flag", stderr)
	}
}

// TestWebRejectsEmptyDB 验证空 --db 被拒绝。
//
// 扫描结果必须落库，空路径会让数据无处可去。
func TestWebRejectsEmptyDB(t *testing.T) {
	// 用一个必然空闲的端口并且不打开浏览器，避免真的弹窗；
	// 但校验发生在起服务之前，因此这里不会真的监听。
	code, _, stderr := runCLI("web",
		"--db", "",
		"--no-browser",
		"--listen", "127.0.0.1:0",
		"--log-dir", filepath.Join(t.TempDir(), "logs"))
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
	}
	if !strings.Contains(stderr, "--db") {
		t.Errorf("stderr = %q, want it to name --db", stderr)
	}
}

// TestWebIsNotInRoadmap 验证 web 已经实现，
// 不再出现在"规划中的命令"里。
//
// 命令在两处同时出现会让用户无法判断它到底能不能用——
// 这个错误在 query 上已经犯过一次。
func TestWebIsNotInRoadmap(t *testing.T) {
	for _, name := range roadmapOrder {
		if name == "web" {
			t.Error("web is listed as a planned command, but it is implemented")
		}
	}

	found := false
	for _, name := range helpOrder {
		if name == "web" {
			found = true
		}
	}
	if !found {
		t.Error("web is missing from helpOrder; it would not appear in --help")
	}

	// 命令表里必须真的有 Run（而不是占位实现）。
	cmd, ok := newCommands()["web"]
	if !ok {
		t.Fatal("web is not registered")
	}
	if cmd.Run == nil {
		t.Error("web is registered without a Run implementation")
	}
	if cmd.Flags == nil {
		t.Error("web has no Flags renderer; --help would not list its flags")
	}
}

// TestWebReadyMessageIsActionable 验证"服务就绪"提示里带上了完整地址。
//
// 用户需要点击那个地址才能进入界面；只打印端口是不够的。
func TestWebReadyMessageIsActionable(t *testing.T) {
	// webReadyMessage 需要一个已启动的 Server，因此这里直接检查
	// 它的组成部分是否完整（由 internal/webui 的测试验证真实输出）。
	var p webParams
	webFlagSet(&p)

	if !strings.Contains(p.listen, ":") {
		t.Errorf("--listen default %q does not contain a port", p.listen)
	}
}

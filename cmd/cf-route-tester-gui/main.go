// Command cf-route-tester-gui 是 cf-route-tester 的**无控制台**入口。
//
// 它存在的唯一理由：Windows 上双击一个控制台程序会先弹出一个黑窗口，
// 而"图形界面"的使用者不该看到它。用
// `-ldflags "-H=windowsgui"` 构建本命令即可去掉那个窗口。
//
// 与 cmd/cf-route-tester 的关系：
//
//	cmd/cf-route-tester       完整命令行（web 是一个子命令）
//	cmd/cf-route-tester-gui   不带参数直接启动图形界面，无控制台窗口
//
// 两个入口共用 internal/cli 与 internal/service，
// 因此"界面里跑的东西"与"命令行里跑的东西"是同一份实现。
//
// == 为什么必须留一个窗口 ==
//
// 没有控制台的程序在后台跑起来之后，用户**看不到任何东西**：
// 不知道它是否启动成功、不知道界面地址、也没有正常途径关掉它，
// 只能去任务管理器杀进程。实测下来这比"多一个小窗口"糟糕得多。
//
// 因此这里创建一个常驻小窗口（见 window_windows.go），
// 显示服务状态与地址，并提供「打开界面」「退出」；
// 关掉窗口即退出程序。
//
// == 日志仍然完整 ==
//
// 窗口不是日志的替代品：真正的排查信息（含带令牌的完整地址）
// 依旧写进日志文件，因为窗口可能被关掉、被截图、或根本看不清。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cf-route-tester/cf-route-tester/internal/cli"
	"github.com/cf-route-tester/cf-route-tester/internal/webui"
)

func main() {
	// 双击启动时的工作目录可能是 C:\Windows\System32 之类：
	// 那样数据库与日志会散落在莫名其妙的地方，用户永远找不到自己的数据。
	// 因此先把工作目录切到可执行文件所在目录。
	if err := chdirToExecutableDir(); err != nil {
		// 切不过去不算致命：仍然启动，只是相对路径会落在当前目录。
		fmt.Fprintln(os.Stderr, "warning: cannot use the executable directory: "+err.Error())
	}

	env := cli.NewEnv()

	// 标记"没有可用的控制台"。
	//
	// 本二进制以 -H=windowsgui 构建，os.Stderr 指向无效句柄，
	// 写进去的内容会静默丢失。web 命令据此改为只写日志文件——
	// 否则双击启动后日志里一条记录都没有，用户完全无从排查。
	env.Detached = true

	// 服务起来之后创建常驻窗口，并把它作为"何时该退出"的来源之一。
	//
	// 这个钩子必须存在而不是"另起一个 goroutine 建窗口"：
	// 窗口的消息循环要占用一个线程，而 web 命令必须继续跑服务，
	// 两者由同一个 select 协调才不会出现"窗口关了服务还在跑"。
	env.OnServerReady = startWindow

	// 不带参数（双击）时默认启动图形界面。
	//
	// 命令行用户仍然可以传参，例如：
	//   cf-route-tester-gui.exe --listen 127.0.0.1:8123
	//   cf-route-tester-gui.exe --no-browser
	// 也能当作普通 CLI 使用（子命令仍然可用，见 cli.RunDefault）。
	code := cli.RunDefault(env, os.Args)
	os.Exit(code)
}

// startWindow 在服务开始监听后创建窗口。
//
// 返回的通道在窗口关闭时关闭；web 命令会等它，因此
// "关掉窗口 = 程序退出"。
func startWindow(server *webui.Server) <-chan struct{} {
	closed, err := runWindow(server.PageURL(), server.URL())
	if err != nil {
		// 建窗口失败**不能**让程序退出：服务本身是好的，
		// 用户仍然可以手动打开地址。如实写进日志，
		// 并返回一个永不关闭的通道（于是只能靠 Ctrl+C 或杀进程退出）。
		fmt.Fprintf(os.Stderr, "warning: cannot create the window: %v\n", err)
		return nil
	}
	return closed
}

// chdirToExecutableDir 把工作目录切到可执行文件所在目录。
func chdirToExecutableDir() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// 解析符号链接：否则在某些安装方式下会切到一个临时目录。
	resolved, err := filepath.EvalSymlinks(executable)
	if err == nil {
		executable = resolved
	}
	return os.Chdir(filepath.Dir(executable))
}

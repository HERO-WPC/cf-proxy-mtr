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
// 关键设计：**没有控制台，所以启动失败必须留下痕迹**。
// 这里不把错误只写到 stderr（双击启动时 stderr 无处可去），
// 而是先尝试打开日志文件所在目录并弹出一个可见的提示；
// 日志本身由 web 命令在启动早期就写好。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cf-route-tester/cf-route-tester/internal/cli"
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
	//
	// Windows 上只有确实处于 GUI 子系统时才这样标记；从已有终端
	// 启动本入口时控制台是可用的，但那种用法极少见，
	// 且"多写一份日志文件"总比"什么都没写"安全。
	env.Detached = true

	// 不带参数（双击）时默认启动图形界面。
	//
	// 命令行用户仍然可以传参，例如：
	//   cf-route-tester-gui.exe --listen 127.0.0.1:8123
	//   cf-route-tester-gui.exe --no-browser
	// 也能当作普通 CLI 使用（子命令仍然可用，见 cli.RunDefault）。
	code := cli.RunDefault(env, os.Args)
	os.Exit(code)
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

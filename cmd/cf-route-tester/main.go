// Command cf-route-tester 是众测网络线路测量工具的入口。
//
// main 包只做三件事：构造运行环境、调用 CLI、把退出码返回给操作系统。
// 所有业务逻辑都在 internal/ 下，保证 main 可以被单元测试之外的代码复用。
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/cf-route-tester/cf-route-tester/internal/cli"
)

func main() {
	env := cli.NewEnv()
	code := cli.Run(env, os.Args)

	// 双击运行时的兜底提示。
	//
	// 这个文件是**命令行版**（CONSOLE 子系统）。双击它等于"不带参数
	// 运行"，于是它打印用法后立刻退出，窗口随之关闭——使用者看到的
	// 只有一个一闪而过的黑框，连有提示都不知道，更谈不上照着做。
	//
	// 图形界面是**另一个文件**（cf-route-tester-gui-*.exe），而下载页上
	// 两个名字很像。因此这里在窗口即将消失前把话说完，并停住等按键。
	//
	// 只在"没有参数"且"控制台是专门为本进程创建的"时才停：
	// 在已有终端里运行（或跑脚本）时不停，否则会打断自动化。
	if len(os.Args) <= 1 && cli.OwnsConsole() {
		printDoubleClickHint(env)
		waitForEnter()
	}

	os.Exit(code)
}

// printDoubleClickHint 说明"你下的是命令行版"以及该怎么做。
func printDoubleClickHint(env *cli.Env) {
	const hint = `
────────────────────────────────────────────────────────────
这个文件是【命令行版】，不能双击使用。

想要图形界面，请下载并双击：
    cf-route-tester-gui-windows-amd64.exe

想在终端里用命令行版：
    1. 在本文件夹的地址栏输入 cmd 并回车
    2. 执行  %s --help
────────────────────────────────────────────────────────────
`
	// 用程序名而不是写死路径：使用者可能改了文件名。
	fmt.Fprintf(env.Stdout, hint, env.Info.Client)
}

// waitForEnter 停住窗口，直到使用者按键。
//
// 读的是标准输入：双击场景下控制台就是我们的，用户按回车即可。
func waitForEnter() {
	fmt.Fprint(os.Stdout, "按回车键关闭…")

	// 忽略错误：管道里没有输入（例如被脚本调用）时不该因此失败，
	// 窗口会正常关闭。
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

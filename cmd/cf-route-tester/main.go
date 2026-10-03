// Command cf-route-tester 是众测网络线路测量工具的入口。
//
// main 包只做三件事：构造运行环境、调用 CLI、把退出码返回给操作系统。
// 所有业务逻辑都在 internal/ 下，保证 main 可以被单元测试之外的代码复用。
package main

import (
	"os"

	"github.com/cf-route-tester/cf-route-tester/internal/cli"
)

func main() {
	env := cli.NewEnv()
	os.Exit(cli.Run(env, os.Args))
}

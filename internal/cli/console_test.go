package cli

import "testing"

// TestOwnsConsoleDoesNotPanic 验证这个控制台查询在各平台都能安全调用。
//
// 这条测试看着单薄，挡的却是一类真实事故：用 syscall.NewLazyDLL 取
// 函数时，**函数名或 DLL 名字写错会在第一次调用时 panic**，而不是
// 编译期报错。本项目已经踩过一次——GetStockObject 其实在 gdi32 里，
// 却写成了 user32，结果程序一启动就崩。
//
// 因此这里真的调用一次。注意不能断言返回值：测试运行在 CI 或终端里，
// "控制台是否为本进程独占"取决于环境，两种都正常。
func TestOwnsConsoleDoesNotPanic(t *testing.T) {
	// 不关心真假，只要求能返回。
	_ = OwnsConsole()
}

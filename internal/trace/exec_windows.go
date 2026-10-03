//go:build windows

package trace

import (
	"os/exec"
	"syscall"
)

// hideSubprocessWindow 让子进程不弹出控制台窗口。
//
// 为什么必须做：NextTrace 是一个**控制台程序**。在没有控制台的宿主
// （例如以 -H=windowsgui 构建的图形界面）里启动它时，Windows 会为它
// 新建一个控制台窗口——跟踪几十个目标就会看到几十个黑框一闪而过，
// 或者更糟：每个目标都留一个窗口。
//
// CREATE_NO_WINDOW 让子进程没有自己的控制台，输出仍然可以通过
// 重定向的 stdout/stderr 拿到（这里本来就用管道读取 JSON），
// 因此不会影响功能。
//
// 只在图形界面下需要；从终端运行时那个窗口本来就在，
// 但即使加了这个标志也不会有副作用（子进程本来也不该弹新窗口）。
func hideSubprocessWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	// CREATE_NO_WINDOW：不分配控制台。
	cmd.SysProcAttr.CreationFlags |= 0x08000000
}

//go:build !windows

package trace

import "os/exec"

// hideSubprocessWindow 在非 Windows 平台上是空操作。
//
// Unix 下"启动子进程"本身不会创建终端窗口，因此不需要做任何事。
// 保留这个函数是为了让调用点不必写平台分支。
func hideSubprocessWindow(_ *exec.Cmd) {}

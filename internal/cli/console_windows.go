//go:build windows

package cli

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// OwnsConsole 报告"这个控制台是专门为本进程创建的"。
//
// 判据是 GetConsoleProcessList 里只有自己：
//
//   - **双击 exe** 时 Windows 会为它新建一个控制台，里面只有它自己。
//     进程一退出，窗口立刻消失——使用者看到的就是"闪一个黑框"，
//     连提示都来不及读。
//   - 在**已有的终端**里运行时，那个终端（cmd / Windows Terminal /
//     pwsh）与我们在同一个控制台上，因此进程数大于 1。
//
// 只有前一种情况才值得停下等按键：在终端里等人按回车会打断脚本。
func OwnsConsole() bool {
	// 四个槽足够：双击场景只有 1 个进程。
	// 槽不足时该函数返回所需的个数，同样不为 1，因此判断依然成立。
	var pids [4]uint32
	count, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return count == 1
}

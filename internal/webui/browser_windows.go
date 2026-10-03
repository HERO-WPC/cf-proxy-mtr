//go:build windows

package webui

import (
	"fmt"
	"os/exec"
	"syscall"
)

// openBrowser 用系统默认浏览器打开 url。
//
// Windows 上的两个细节：
//
//  1. 必须走 `cmd /c start`，因为 Go 没有"用默认程序打开 URL"的 API；
//  2. `start` 是 cmd 的内建命令，且它会把第一个参数当成窗口标题，
//     因此要传一个空标题（""）再接 URL——否则 URL 会被当成标题，
//     结果什么都不打开。
//
// 刻意**不**用 syscall.Syscall 直接调 ShellExecute：那需要
// unsafe 与平台细节，而 cmd /c start 已经足够，且更易读。
func openBrowser(url string) error {
	cmd := exec.Command("cmd", "/c", "start", "", url)
	// CREATE_NO_WINDOW：不要为这次调用闪出一个控制台窗口。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	// 不 Wait：浏览器是长驻进程，等它等于永远不返回。
	return cmd.Process.Release()
}

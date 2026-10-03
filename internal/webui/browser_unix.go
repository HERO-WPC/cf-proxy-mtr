//go:build !windows && !darwin

package webui

import (
	"fmt"
	"os/exec"
)

// openBrowser 用系统默认浏览器打开 url。
//
// Linux 与其它类 Unix 系统上没有一个统一可靠的"打开默认程序"命令，
// 因此按普及度依次尝试：xdg-open（桌面环境标准）、
// gio open（GNOME）、x-www-browser（Debian 替代机制）。
//
// 全部失败时返回错误，由调用方降级为"把地址打印出来让用户自己点"——
// 无头服务器上这是常态，不该被当成故障。
func openBrowser(url string) error {
	candidates := [][]string{
		{"xdg-open", url},
		{"gio", "open", url},
		{"x-www-browser", url},
		{"sensible-browser", url},
	}

	var lastErr error
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate[0])
		if err != nil {
			lastErr = err
			continue
		}
		cmd := exec.Command(path, candidate[1:]...)
		if err := cmd.Start(); err != nil {
			lastErr = err
			continue
		}
		return cmd.Process.Release()
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no browser opener found")
	}
	return fmt.Errorf("start browser: %w", lastErr)
}

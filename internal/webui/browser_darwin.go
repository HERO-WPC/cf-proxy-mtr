//go:build darwin

package webui

import (
	"fmt"
	"os/exec"
)

// openBrowser 用系统默认浏览器打开 url。
func openBrowser(url string) error {
	cmd := exec.Command("open", url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	// 不 Wait：浏览器是长驻进程。
	return cmd.Process.Release()
}

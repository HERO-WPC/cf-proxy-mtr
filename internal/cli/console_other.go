//go:build !windows

package cli

// OwnsConsole 在非 Windows 上恒为 false。
//
// 这些平台上双击不会凭空造出一个"退出即消失"的终端：终端模拟器
// 自己决定窗口的去留，因此不需要我们插一脚。
func OwnsConsole() bool { return false }

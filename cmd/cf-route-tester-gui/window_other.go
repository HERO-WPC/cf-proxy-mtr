//go:build !windows

// 非 Windows 平台上没有"常驻小窗口"这个需求。
//
// 原因：Linux/macOS 的启动器（.desktop / .app）不会给程序附一个控制台
// 窗口，因此图形入口在那边本来就是"后台服务 + 浏览器界面"，
// 不需要额外窗口来解释"怎么关掉它"。
//
// 这里提供同名函数只是为了让 cmd/cf-route-tester-gui 的 main.go
// 能在所有平台编译（源码要能交叉编译，否则 CI 的交叉编译步骤会失败）。
package main

import "errors"

// runWindow 在非 Windows 平台上不可用。
//
// 返回错误而不是空实现：main.go 会把它写进日志并继续提供服务，
// 而"静默什么都不做"会让调用方以为窗口已经存在。
func runWindow(_, _ string) (<-chan struct{}, error) {
	return nil, errors.New("the resident window is only implemented on Windows; " +
		"use 'cf-route-tester web' instead")
}

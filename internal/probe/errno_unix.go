//go:build !windows

package probe

import "syscall"

// platformErrnos 是 Unix（Linux / macOS / *BSD）的错误码规则表。
//
// 与 Windows 版本保持同样的分类语义与优先级：更具体的原因在前
// （拒绝/重置优先于不可达，不可达优先于无路由），
// 这样同一个网络现象在不同平台会得到同一个分类。
var platformErrnos = []errnoEntry{
	// ---- 连接被拒绝 ----
	{int(syscall.ECONNREFUSED), ErrorTypeConnectionRefused},

	// ---- 连接被重置 ----
	{int(syscall.ECONNRESET), ErrorTypeConnectionReset},
	{int(syscall.ECONNABORTED), ErrorTypeConnectionReset},
	{int(syscall.EPIPE), ErrorTypeConnectionReset},

	// ---- 网络/主机不可达 ----
	{int(syscall.ENETUNREACH), ErrorTypeNetworkUnreachable},
	{int(syscall.EHOSTUNREACH), ErrorTypeNetworkUnreachable},
	{int(syscall.ENETDOWN), ErrorTypeNetworkUnreachable},
	{int(syscall.ENETRESET), ErrorTypeNetworkUnreachable},
	{int(syscall.EHOSTDOWN), ErrorTypeNetworkUnreachable},

	// ---- 超时 ----
	{int(syscall.ETIMEDOUT), ErrorTypeTimeout},

	// ---- 本机策略拒绝 ----
	{int(syscall.EACCES), ErrorTypePermissionDenied},
	{int(syscall.EPERM), ErrorTypePermissionDenied},

	// ---- 地址不可用 ----
	{int(syscall.EADDRNOTAVAIL), ErrorTypeAddressNotAvailable},
	{int(syscall.EAFNOSUPPORT), ErrorTypeAddressNotAvailable},

	// ---- 无路由 ----
	// Linux 上"没有到主机的路由"通常是 EHOSTUNREACH（已归入不可达）；
	// ESRCH 出现在部分内核的 sendto 路径上。
	{int(syscall.ESRCH), ErrorTypeNoRoute},
}

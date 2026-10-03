//go:build windows

package probe

import "syscall"

// platformErrnos 是 Windows 平台的错误码规则表。
//
// Windows 的坑（这是本文件存在的全部理由）：
//
//  1. Go 的 syscall 包在 Windows 上使用 **WSA 错误码**，
//     而不是 POSIX errno：WSAECONNREFUSED = 10061（不是 111），
//     WSAETIMEDOUT = 10060，WSAENETUNREACH = 10051。
//  2. Go 还会把一部分 WSA 错误**归一化**成伪 errno，
//     例如 WSAEWOULDBLOCK(10035) -> syscall.EWOULDBLOCK(140)，
//     WSAEINVAL 等也有类似映射。因此同一语义可能出现两种数字，
//     两套都必须列出来。
//  3. 还有一些语义在 Unix 上是一个 errno、在 Windows 上拆成多个
//     （例如"没有到主机的路由"在 Windows 上是 WSAEHOSTUNREACH 10065，
//     而不是 ENETUNREACH 的等价物）。
//
// 顺序要求：更具体的原因排在前面（拒绝/重置优先于不可达），
// 与 Unix 表保持一致的优先级，避免同一现象在两个平台得到不同分类。
var platformErrnos = []errnoEntry{
	// ---- 连接被拒绝 ----
	{10061, ErrorTypeConnectionRefused}, // WSAECONNREFUSED
	{int(syscall.ECONNREFUSED), ErrorTypeConnectionRefused},

	// ---- 连接被重置 ----
	{10054, ErrorTypeConnectionReset}, // WSAECONNRESET
	{10053, ErrorTypeConnectionReset}, // WSAECONNABORTED（本端认为连接已中止）
	{int(syscall.ECONNRESET), ErrorTypeConnectionReset},

	// ---- 网络/主机不可达 ----
	{10051, ErrorTypeNetworkUnreachable}, // WSAENETUNREACH
	{10065, ErrorTypeNetworkUnreachable}, // WSAEHOSTUNREACH
	{10050, ErrorTypeNetworkUnreachable}, // WSAENETDOWN
	{10052, ErrorTypeNetworkUnreachable}, // WSAENETRESET
	{10064, ErrorTypeNetworkUnreachable}, // WSAEHOSTDOWN
	{int(syscall.ENETUNREACH), ErrorTypeNetworkUnreachable},
	{int(syscall.EHOSTUNREACH), ErrorTypeNetworkUnreachable},
	{int(syscall.ENETDOWN), ErrorTypeNetworkUnreachable},

	// ---- 超时 ----
	{10060, ErrorTypeTimeout}, // WSAETIMEDOUT
	{int(syscall.ETIMEDOUT), ErrorTypeTimeout},

	// ---- 本机策略拒绝 ----
	{10013, ErrorTypePermissionDenied}, // WSAEACCES（防火墙/安全软件拒绝）
	{int(syscall.EACCES), ErrorTypePermissionDenied},
	{int(syscall.EPERM), ErrorTypePermissionDenied},

	// ---- 地址不可用 ----
	{10049, ErrorTypeAddressNotAvailable}, // WSAEADDRNOTAVAIL
	{10047, ErrorTypeAddressNotAvailable}, // WSAEAFNOSUPPORT（系统不支持该地址族）
	{int(syscall.EADDRNOTAVAIL), ErrorTypeAddressNotAvailable},
	{int(syscall.EAFNOSUPPORT), ErrorTypeAddressNotAvailable},
}

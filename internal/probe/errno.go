package probe

import (
	"errors"
	"syscall"
)

// ---------------------------------------------------------------------------
// 平台错误码 -> 分类
// ---------------------------------------------------------------------------
//
// 为什么必须分平台处理：
//
//	syscall.Errno 在 Windows 上是 WSA 错误码（例如 10061 = WSAECONNREFUSED），
//	在 Unix 上是 POSIX errno（例如 111 = ECONNREFUSED）。两套数字完全不同。
//	如果把数字直接写进分类逻辑，就会出现"Linux 上分类正确、
//	Windows 上全落到 other"这种只在某个平台暴露的缺陷。
//
// 因此规则表放在 platformErrnos（各平台文件分别定义），
// 分类逻辑本身与平台无关。
//
// 另一个坑：Go 在 Windows 上会把一部分 WSA 错误映射成伪 errno
// （例如 WSAEWOULDBLOCK -> syscall.EWOULDBLOCK）。
// 因此我们在 Windows 上同时检查**两套**数字，见 errno_windows.go。

// errnoEntry 把一个数值错误码绑定到分类。
type errnoEntry struct {
	code int
	kind ErrorType
}

// classifyErrno 在平台错误码表里查找分类。
//
// 返回 ok=false 表示"这个错误码没有定义分类"，
// 由上层继续尝试其它判断（例如文本特征），而不是直接归入 other。
func classifyErrno(err error) (ErrorType, bool) {
	var se syscall.Errno
	if !errors.As(err, &se) {
		return ErrorTypeNone, false
	}

	code := int(se)
	for _, entry := range platformErrnos {
		if entry.code == code {
			return entry.kind, true
		}
	}
	return ErrorTypeNone, false
}

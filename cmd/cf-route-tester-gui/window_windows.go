//go:build windows

// 本文件实现图形入口的"常驻小窗口"。
//
// 为什么需要它：以 -H=windowsgui 构建的程序没有控制台，双击启动后
// 进程在后台跑，用户**看不到任何东西**，也就没有任何正常的关闭途径
// ——只能去任务管理器里杀进程。这既难用又容易让人以为程序没启动。
//
// 因此这里创建一个极小的原生窗口，显示服务地址与状态，并提供
// 「打开界面」与「退出」两个按钮。关掉窗口即退出程序。
//
// 刻意直接调用 Win32（syscall + user32/kernel32），不引入 GUI 框架：
// 需求只有"一个窗口 + 一个按钮 + 一个标题"，而任何框架都会带来
// CGO 或跨平台抽象，破坏本项目 CGO_ENABLED=0 的交叉编译。
package main

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// Win32 常量。
const (
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsTabStop          = 0x00010000
	wsBorder           = 0x00800000
	bsPushButton       = 0x00000000
	bsDefPushButton    = 0x00000001
	swShow             = 5
	swRestore          = 9
	swShownormal       = 1
	wmDestroy          = 0x0002
	wmClose            = 0x0010
	wmCommand          = 0x0111
	wmSetfont          = 0x0030
	wmCreate           = 0x0001
	bnClicked          = 0

	cwUseDefault = 0x80000000

	// 控件 ID
	idOpenButton = 1001
	idQuitButton = 1002

	// 窗口尺寸（像素）。够放下两行文字与两个按钮即可。
	windowWidth  = 460
	windowHeight = 190
)

// 按钮/文本位置与尺寸。
const (
	textX = 18
	textY = 16

	openButtonX = 18
	quitButtonX = 168
	buttonY     = 108
	buttonW     = 140
	buttonH     = 34
)

// Win32 过程调用约定。
var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	// 字体相关 API 在 gdi32 里。早先误写在 user32 上，
	// 结果是启动时 panic（"Failed to find GetStockObject procedure"），
	// 而且因为发生在窗口创建过程中，表现为"进程直接消失、什么都没留下"。
	gdi32 = syscall.NewLazyDLL("gdi32.dll")

	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procSetWindowText    = user32.NewProc("SetWindowTextW")
	procSendMessage      = user32.NewProc("SendMessageW")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetForegroundWnd = user32.NewProc("SetForegroundWindow")
	procMessageBox       = user32.NewProc("MessageBoxW")
	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
	procShellExecute     = shell32.NewProc("ShellExecuteW")
)

// wndClassEx 对应 WNDCLASSEXW 结构。
//
// 字段顺序与大小必须与 Win32 完全一致：结构体布局错了会直接崩溃，
// 而且这种错误在编译期看不出来。
type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   syscall.Handle
	Icon       syscall.Handle
	Cursor     syscall.Handle
	Background syscall.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  syscall.Handle
}

// msg 对应 MSG 结构。
type msg struct {
	HWnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// rect 对应 RECT 结构。
type rect struct {
	Left, Top, Right, Bottom int32
}

// windowState 是窗口运行期的共享状态。
//
// WndProc 由系统在**窗口线程**上回调，而退出信号要交给主 goroutine，
// 因此这里用互斥量保护。
type windowState struct {
	mu        sync.Mutex
	hwnd      syscall.Handle
	url       string
	closed    chan struct{}
	closeOnce sync.Once
	className *uint16
}

// runWindow 创建并运行窗口，阻塞直到窗口关闭。
//
// 返回的通道在窗口关闭时被关闭，供调用方等待。
// title/subtitle 是窗口里显示的两行文字。
func runWindow(url, subtitle string) (<-chan struct{}, error) {
	className, err := syscall.UTF16PtrFromString("CfRouteTesterWindow")
	if err != nil {
		return nil, err
	}

	state := &windowState{
		url:       url,
		closed:    make(chan struct{}),
		className: className,
	}

	instance, _, _ := procGetModuleHandle.Call(0)

	wndProc := syscall.NewCallback(state.windowProc)

	class := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   wndProc,
		Instance:  syscall.Handle(instance),
		ClassName: className,
	}
	if ret, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); ret == 0 {
		return nil, fmt.Errorf("RegisterClassEx failed: %v", callErr)
	}

	title, err := syscall.UTF16PtrFromString("cf-route-tester")
	if err != nil {
		return nil, err
	}

	// 水平居中、垂直偏上：放正中间会挡住用户正在看的浏览器内容。
	screenW, _, _ := procGetSystemMetrics.Call(0)
	x := (int(screenW) - windowWidth) / 2
	if x < 0 {
		x = 0
	}

	hwnd, _, callErr := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(x), 80,
		windowWidth, windowHeight,
		0, 0,
		instance,
		0,
	)
	if hwnd == 0 {
		return nil, fmt.Errorf("CreateWindowEx failed: %v", callErr)
	}

	state.mu.Lock()
	state.hwnd = syscall.Handle(hwnd)
	state.mu.Unlock()

	procShowWindow.Call(hwnd, swShow)
	procUpdateWindow.Call(hwnd)

	// 把窗口带到前台：用户刚双击完图标，期望看到的就是它。
	procSetForegroundWnd.Call(hwnd)

	// 消息循环必须在**创建窗口的那个线程**上跑。
	var message msg
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}

	state.markClosed()
	return state.closed, nil
}

// markClosed 关闭"窗口已关闭"通道（幂等）。
func (s *windowState) markClosed() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// windowProc 是窗口消息处理函数。
func (s *windowState) windowProc(hwnd syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmCreate:
		s.createControls(hwnd)
		return 0

	case wmCommand:
		switch uint16(wParam & 0xFFFF) {
		case idOpenButton:
			s.openBrowser()
			return 0
		case idQuitButton:
			// 走正常的关闭流程：发 WM_CLOSE 而不是直接 DestroyWindow，
			// 这样"点按钮"与"点右上角 ×"的行为完全一致。
			procSendMessage.Call(uintptr(hwnd), wmClose, 0, 0)
			return 0
		}

	case wmClose:
		// 关闭窗口即退出程序：用户关掉这个窗口时，
		// 他期望的是"程序结束了"，而不是"服务还在后台跑"。
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		s.markClosed()
		procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return ret
}

// createControls 在窗口里创建文字与按钮。
func (s *windowState) createControls(hwnd syscall.Handle) {
	instance, _, _ := procGetModuleHandle.Call(0)

	// 统一字体。必须逐个控件发送 WM_SETFONT：
	// 发给顶层窗口**不会**影响子控件（字体不继承），
	// 那是这段代码最初写错、且不会报错的地方。
	font, _, _ := procGetStockObject.Call(17) // 17 = DEFAULT_GUI_FONT

	// 状态文字：告诉用户服务在跑、地址是什么。
	s.createLabel(hwnd, instance, font,
		"服务已启动，界面在浏览器中打开。",
		textX, textY, windowWidth-2*textX, 22)

	s.createLabel(hwnd, instance, font,
		"关闭这个窗口即退出程序。",
		textX, textY+24, windowWidth-2*textX, 22)

	// 地址单独一行（可能较长，按钮可以重新打开它）。
	s.createLabel(hwnd, instance, font,
		"http://"+shortHost(s.url),
		textX, textY+52, windowWidth-2*textX, 22)

	s.createButton(hwnd, instance, font, "打开界面", idOpenButton, openButtonX, buttonY, bsDefPushButton)
	s.createButton(hwnd, instance, font, "退出", idQuitButton, quitButtonX, buttonY, bsPushButton)

}

// createLabel 创建一个静态文本控件。
func (s *windowState) createLabel(hwnd syscall.Handle, instance, font uintptr, text string, x, y, w, h int) {
	classPtr, _ := syscall.UTF16PtrFromString("STATIC")
	textPtr, _ := syscall.UTF16PtrFromString(text)

	control, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)),
		wsChild|wsVisible,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		uintptr(hwnd), 0, instance, 0,
	)
	if control != 0 && font != 0 {
		procSendMessage.Call(control, wmSetfont, font, 1)
	}
}

// createButton 创建一个按钮控件。
func (s *windowState) createButton(hwnd syscall.Handle, instance, font uintptr, text string, id int, x, y, style int) {
	classPtr, _ := syscall.UTF16PtrFromString("BUTTON")
	textPtr, _ := syscall.UTF16PtrFromString(text)

	control, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(wsChild|wsVisible|wsTabStop|style),
		uintptr(x), uintptr(y), buttonW, buttonH,
		uintptr(hwnd), uintptr(id), instance, 0,
	)
	if control != 0 && font != 0 {
		procSendMessage.Call(control, wmSetfont, font, 1)
	}
}

// openBrowser 用默认浏览器打开服务地址。
func (s *windowState) openBrowser() {
	verb, _ := syscall.UTF16PtrFromString("open")
	target, _ := syscall.UTF16PtrFromString(s.url)

	// ShellExecuteW 的返回值 <= 32 表示失败。
	ret, _, _ := procShellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(target)),
		0, 0,
		swShownormal,
	)
	if ret <= 32 {
		message, _ := syscall.UTF16PtrFromString(
			"无法自动打开浏览器。\n\n请手动复制这个地址：\n" + s.url)
		title, _ := syscall.UTF16PtrFromString("cf-route-tester")
		procMessageBox.Call(0,
			uintptr(unsafe.Pointer(message)),
			uintptr(unsafe.Pointer(title)),
			0x00000030) // MB_ICONWARNING
	}
}

// shortHost 从完整 URL 里截出 "host:port"，避免长令牌撑破窗口。
func shortHost(url string) string {
	// 只去掉 scheme 与查询串；令牌不显示在窗口上，
	// 免得被截图或录屏时带走。
	stripped := url
	for _, prefix := range []string{"http://", "https://"} {
		if len(stripped) >= len(prefix) && stripped[:len(prefix)] == prefix {
			stripped = stripped[len(prefix):]
			break
		}
	}
	if idx := indexByte(stripped, '/'); idx >= 0 {
		stripped = stripped[:idx]
	}
	return stripped
}

// indexByte 返回 b 在 s 中首次出现的下标（-1 表示没有）。
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

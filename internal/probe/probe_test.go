package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// fakeConn 是一个"永远关闭成功"的空连接，用于模拟握手成功。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (fakeConn) Write([]byte) (int, error)        { return 0, errors.New("not implemented") }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// dialerFunc 把函数适配成 DialContextFunc。
func dialerFunc(fn func(ctx context.Context, network, address string) (net.Conn, error)) DialContextFunc {
	return fn
}

// okDialer 模拟 TCP 握手成功，并记录收到的 network / address。
func okDialer(delay time.Duration, gotNetwork, gotAddress *string) DialContextFunc {
	return dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		if gotNetwork != nil {
			*gotNetwork = network
		}
		if gotAddress != nil {
			*gotAddress = address
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return fakeConn{}, nil
	})
}

// errDialer 模拟固定的连接错误。
func errDialer(err error) DialContextFunc {
	return dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, err
	})
}

// waitCtxDialer 一直等待，直到 ctx 结束，然后返回 ctx.Err()。
// 用于确定性地制造"单目标超时"。
func waitCtxDialer() DialContextFunc {
	return dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
}

// mustTarget 构造一个合法的测试目标。
func mustTarget(t *testing.T, ip string, port int) model.Target {
	t.Helper()
	target, err := model.NewTargetFromStrings(ip, port)
	if err != nil {
		t.Fatalf("NewTargetFromStrings(%q, %d): %v", ip, port, err)
	}
	return target
}

// ---------------------------------------------------------------------------
// 错误分类：按错误码
// ---------------------------------------------------------------------------

// TestClassifyErrno 覆盖**当前平台会真实出现**的错误码。
//
// 分类表是分平台的（errno_windows.go / errno_unix.go），因此测试
// 也必须分平台，否则会在 Linux 上要求注册 Windows 的 WSA 码——
// 而 Linux 内核永远不会返回 10061，那种断言只能是错的。
//
// 两张表的实际约定：
//
//   - POSIX errno：**两套表都注册**。Windows 侧刻意也注册它们，
//     因为 Go 会把一部分 WSA 错误归一化成伪 errno（见
//     errno_windows.go 的说明），同一语义可能出现两种数字。
//   - WSA 码：**只在 Windows 表里注册**。它们是 Windows 网络栈特有的
//     取值，在 Unix 表里注册只会得到一条永远匹配不到的死条目。
//
// 因此在非 Windows 上跳过 Windows 专属用例不是"少测了"：
// 那些编号在当前平台上本来就**不该**被注册，跳过它们正是断言了
// 这一点。跨平台一致性靠的是"两张表用同样的语义与优先级"，
// 而不是"每张表都收录所有平台的编号"。
func TestClassifyErrno(t *testing.T) {
	cases := []struct {
		name string
		code syscall.Errno
		want ErrorType

		// windowsOnly 为真表示该编号只可能来自 Windows 网络栈。
		windowsOnly bool
	}{
		// ---- Windows WSA 错误码（仅 Windows 表注册） ----
		{"windows refused", 10061, ErrorTypeConnectionRefused, true},
		{"windows reset", 10054, ErrorTypeConnectionReset, true},
		{"windows aborted", 10053, ErrorTypeConnectionReset, true},
		{"windows net unreachable", 10051, ErrorTypeNetworkUnreachable, true},
		{"windows host unreachable", 10065, ErrorTypeNetworkUnreachable, true},
		{"windows net down", 10050, ErrorTypeNetworkUnreachable, true},
		{"windows timed out", 10060, ErrorTypeTimeout, true},
		{"windows access denied", 10013, ErrorTypePermissionDenied, true},
		{"windows addr not avail", 10049, ErrorTypeAddressNotAvailable, true},
		{"windows af not supported", 10047, ErrorTypeAddressNotAvailable, true},

		// ---- POSIX errno（两套表都注册） ----
		{"unix refused", syscall.ECONNREFUSED, ErrorTypeConnectionRefused, false},
		{"unix reset", syscall.ECONNRESET, ErrorTypeConnectionReset, false},
		{"unix net unreachable", syscall.ENETUNREACH, ErrorTypeNetworkUnreachable, false},
		{"unix host unreachable", syscall.EHOSTUNREACH, ErrorTypeNetworkUnreachable, false},
		{"unix timed out", syscall.ETIMEDOUT, ErrorTypeTimeout, false},
		{"unix perm", syscall.EPERM, ErrorTypePermissionDenied, false},
		{"unix addr not avail", syscall.EADDRNOTAVAIL, ErrorTypeAddressNotAvailable, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.windowsOnly && runtime.GOOS != "windows" {
				t.Skipf("errno %d is a Windows-only network code; the Unix table "+
					"deliberately does not register it", int(tc.code))
			}

			// 先单独验证注册表本身（不依赖分类路径）。
			got, ok := classifyErrno(tc.code)
			if !ok {
				t.Fatalf("errno %d is not registered in platformErrnos", int(tc.code))
			}
			if got != tc.want {
				t.Errorf("classifyErrno(%d) = %q, want %q", int(tc.code), got, tc.want)
			}

			// 再验证它在真实错误链里的表现：
			// net.OpError -> os.SyscallError -> syscall.Errno 是 Go 的典型包装。
			opErr := &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &syscallErrorShim{Err: tc.code},
			}
			errType, msg := classify(opErr, context.Background())
			if errType != tc.want {
				t.Errorf("classify(%v) = %q, want %q", opErr, errType, tc.want)
			}
			if msg == "" {
				t.Error("classify returned an empty message")
			}

			// 直接用裸 errno 也必须分类正确。
			if errType, _ := classify(tc.code, context.Background()); errType != tc.want {
				t.Errorf("classify(bare errno %d) = %q, want %q", int(tc.code), errType, tc.want)
			}
		})
	}
}

// TestClassifyErrnoRegistryHasNoDuplicates 保证注册表里没有重复且冲突的项。
//
// 重复项会让"后写的规则生效"，从而让前面的注释与意图失效——
// 这种问题在 code review 里几乎看不出来，所以用测试锁住。
func TestClassifyErrnoRegistryHasNoDuplicates(t *testing.T) {
	seen := make(map[int]ErrorType, len(platformErrnos))
	for i, entry := range platformErrnos {
		if prev, dup := seen[entry.code]; dup {
			if prev == entry.kind {
				t.Errorf("platformErrnos[%d] duplicates code %d with the same kind %q", i, entry.code, prev)
				continue
			}
			t.Errorf("platformErrnos[%d] gives code %d kind %q, but it was already registered as %q",
				i, entry.code, entry.kind, prev)
			continue
		}
		if !entry.kind.Valid() {
			t.Errorf("platformErrnos[%d] has invalid kind %q for code %d", i, entry.kind, entry.code)
		}
		if entry.kind == ErrorTypeNone {
			t.Errorf("platformErrnos[%d] maps code %d to ErrorTypeNone", i, entry.code)
		}
		seen[entry.code] = entry.kind
	}

	// 关键语义必须被注册（否则跨平台会退化成 other）。
	required := map[ErrorType]bool{
		ErrorTypeConnectionRefused:   false,
		ErrorTypeConnectionReset:     false,
		ErrorTypeNetworkUnreachable:  false,
		ErrorTypeTimeout:             false,
		ErrorTypePermissionDenied:    false,
		ErrorTypeAddressNotAvailable: false,
	}
	for _, entry := range platformErrnos {
		if _, ok := required[entry.kind]; ok {
			required[entry.kind] = true
		}
	}
	for kind, found := range required {
		if !found {
			t.Errorf("no errno is registered for %q", kind)
		}
	}
}

// syscallErrorShim 复制 *os.SyscallError 的包装行为：
// net.OpError -> syscallErrorShim -> syscall.Errno。
//
// 不用 *os.SyscallError 本体是为了让"错误链形状"在测试里显式可见：
// 分类逻辑必须能穿透多层包装，这正是它在真实环境里要做的事。
type syscallErrorShim struct{ Err error }

func (e *syscallErrorShim) Error() string { return "syscall: " + e.Err.Error() }
func (e *syscallErrorShim) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// 错误分类：超时 / 取消 / 兜底
// ---------------------------------------------------------------------------

func TestClassifyTimeoutAndCancel(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		parent context.Context
		want   ErrorType
	}{
		{
			name: "deadline exceeded",
			err:  fmt.Errorf("dial tcp: %w", context.DeadlineExceeded),
			want: ErrorTypeTimeout,
		},
		{
			name: "net error timeout",
			err: &net.OpError{
				Op: "dial", Net: "tcp",
				Err: fakeTimeoutError{},
			},
			want: ErrorTypeTimeout,
		},
		{
			name:   "parent canceled",
			err:    fmt.Errorf("dial tcp: %w", context.Canceled),
			parent: canceledContext(),
			want:   ErrorTypeCanceled,
		},
		{
			name: "plain canceled without parent cancel",
			err:  context.Canceled,
			want: ErrorTypeOther,
		},
		{
			name: "unknown error falls back to other",
			err:  errors.New("something strange happened"),
			want: ErrorTypeOther,
		},
		{
			name: "nil parent is tolerated",
			err:  context.DeadlineExceeded,
			want: ErrorTypeTimeout,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := tc.parent
			if parent == nil {
				parent = context.Background()
			}
			got, msg := classify(tc.err, parent)
			if got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
			if msg == "" {
				t.Error("classify returned an empty message")
			}
		})
	}
}

// TestClassifyMessageFallback 覆盖"结构化判断全失败、只能看文本"的兜底路径。
//
// 这类情况真实存在：不同 Go 版本、不同平台的错误包装层数不同，
// syscall.Errno 可能被包在自定义错误里。兜底必须能识别最常见的几种。
func TestClassifyMessageFallback(t *testing.T) {
	cases := []struct {
		message string
		want    ErrorType
	}{
		{"dial tcp 1.2.3.4:443: connect: connection refused", ErrorTypeConnectionRefused},
		{"read tcp: connection reset by peer", ErrorTypeConnectionReset},
		{"dial tcp: connect: network is unreachable", ErrorTypeNetworkUnreachable},
		{"dial tcp: connect: no route to host", ErrorTypeNoRoute},
		{"dial tcp: permission denied", ErrorTypePermissionDenied},
		{"dial tcp: Access is denied.", ErrorTypePermissionDenied},
		{"dial tcp: cannot assign requested address", ErrorTypeAddressNotAvailable},
		{"dial tcp: i/o timeout", ErrorTypeTimeout},
		{"totally unrecognizable failure", ErrorTypeOther},
	}

	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			got, _ := classify(errors.New(tc.message), context.Background())
			if got != tc.want {
				t.Errorf("classify(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

// TestClassifyTruncatesLongMessages 确认错误消息不会无限长。
func TestClassifyTruncatesLongMessages(t *testing.T) {
	long := strings.Repeat("x", 1000)
	_, msg := classify(errors.New(long), context.Background())
	if len(msg) > maxErrorMessage+len("...") {
		t.Errorf("message length = %d, want <= %d", len(msg), maxErrorMessage+3)
	}
	if !strings.HasSuffix(msg, "...") {
		t.Errorf("message = %q, want truncation marker", msg[len(msg)-10:])
	}
}

func TestMilliseconds(t *testing.T) {
	cases := map[time.Duration]float64{
		0:                       0,
		time.Millisecond:        1,
		1500 * time.Microsecond: 1.5,
		time.Second:             1000,
	}
	for in, want := range cases {
		if got := milliseconds(in); got != want {
			t.Errorf("milliseconds(%s) = %v, want %v", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// ErrorType 语义
// ---------------------------------------------------------------------------

func TestErrorTypeSemantics(t *testing.T) {
	if !ErrorTypeNone.Valid() {
		t.Error("ErrorTypeNone.Valid() = false, want true")
	}
	if ErrorType("nonsense").Valid() {
		t.Error("unknown error type reported as valid")
	}
	for _, kind := range AllErrorTypes() {
		if !kind.Valid() {
			t.Errorf("AllErrorTypes() contains invalid %q", kind)
		}
		if !kind.IsFailure() {
			t.Errorf("%q.IsFailure() = false, want true", kind)
		}
	}
	if ErrorTypeNone.IsFailure() {
		t.Error("ErrorTypeNone.IsFailure() = true, want false")
	}

	// 只有"不是本机主动放弃、也不是数据问题"的分类才算线路丢包证据。
	lossTypes := map[ErrorType]bool{
		ErrorTypeTimeout:             true,
		ErrorTypeConnectionRefused:   true,
		ErrorTypeNetworkUnreachable:  true,
		ErrorTypeNoRoute:             true,
		ErrorTypeConnectionReset:     true,
		ErrorTypePermissionDenied:    true,
		ErrorTypeAddressNotAvailable: true,
		ErrorTypeOther:               true,
		ErrorTypeCanceled:            false,
		ErrorTypeInvalidTarget:       false,
		ErrorTypeNone:                false,
	}
	for kind, want := range lossTypes {
		if got := kind.CountsTowardLoss(); got != want {
			t.Errorf("%q.CountsTowardLoss() = %v, want %v", kind, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// ProbeResult 自检
// ---------------------------------------------------------------------------

func TestProbeResultValid(t *testing.T) {
	now := time.Now().UTC()

	ok := ProbeResult{TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, LatencyMS: 12.3, Timestamp: now}
	if err := ok.Valid(); err != nil {
		t.Errorf("valid success result rejected: %v", err)
	}

	failed := ProbeResult{TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, ErrorType: ErrorTypeTimeout, Timestamp: now}
	if err := failed.Valid(); err != nil {
		t.Errorf("valid failure result rejected: %v", err)
	}

	ipv6 := ProbeResult{TargetID: "[2001:db8::1]:8443", IP: "2001:db8::1", Port: 8443, Success: true}
	if err := ipv6.Valid(); err != nil {
		t.Errorf("valid ipv6 result rejected: %v", err)
	}

	cases := map[string]ProbeResult{
		"empty target id":  {IP: "1.2.3.4", Port: 443, Success: true},
		"bad port":         {TargetID: "1.2.3.4:0", IP: "1.2.3.4", Port: 0, Success: true},
		"bad ip":           {TargetID: "nope:443", IP: "nope", Port: 443, Success: true},
		"success with err": {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, ErrorType: ErrorTypeTimeout},
		"failure no type":  {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443},
		"unknown type":     {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, ErrorType: "weird"},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if err := r.Valid(); err == nil {
				t.Error("Valid() = nil, want error")
			}
		})
	}
}

func TestProbeResultAddrPortOf(t *testing.T) {
	v4 := ProbeResult{IP: "1.2.3.4", Port: 2053, TargetID: "1.2.3.4:2053"}
	if got := v4.AddrPortOf(); got != "1.2.3.4:2053" {
		t.Errorf("AddrPortOf() = %q, want 1.2.3.4:2053", got)
	}

	v6 := ProbeResult{IP: "2001:db8::1", Port: 8443, TargetID: "[2001:db8::1]:8443"}
	if got := v6.AddrPortOf(); got != "[2001:db8::1]:8443" {
		t.Errorf("AddrPortOf() = %q, want [2001:db8::1]:8443", got)
	}
}

// ---------------------------------------------------------------------------
// Probe：拨号参数与实际目标端口
// ---------------------------------------------------------------------------

// TestProbeUsesActualTargetPort 是需求第 13 条的回归测试。
//
// 数据库的主键维度是 IP × Port，所以绝不能把端口统一成 443。
func TestProbeUsesActualTargetPort(t *testing.T) {
	cases := []struct {
		ip   string
		port int
		want string
	}{
		{"1.2.3.4", 443, "1.2.3.4:443"},
		{"1.2.3.4", 2053, "1.2.3.4:2053"},
		{"1.2.3.4", 8443, "1.2.3.4:8443"},
		{"1.2.3.4", 2087, "1.2.3.4:2087"},
		{"2001:db8::1", 2053, "[2001:db8::1]:2053"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			var gotNetwork, gotAddress string
			p := New(Config{Dialer: okDialer(0, &gotNetwork, &gotAddress)})

			result := p.Probe(context.Background(), mustTarget(t, tc.ip, tc.port))

			if !result.Success {
				t.Fatalf("Probe failed: %+v", result)
			}
			if gotAddress != tc.want {
				t.Errorf("dialed address = %q, want %q", gotAddress, tc.want)
			}
			if result.Port != tc.port {
				t.Errorf("result port = %d, want %d", result.Port, tc.port)
			}
			if result.TargetID != tc.want {
				t.Errorf("result target id = %q, want %q", result.TargetID, tc.want)
			}
			if err := result.Valid(); err != nil {
				t.Errorf("result invalid: %v", err)
			}
		})
	}
}

// TestProbeUsesMatchingNetworkFamily 确认按目标地址族拨号。
//
// 强制 tcp4/tcp6 可以避免"系统把 IPv6 目标解析成 IPv4"这类
// 静默偏差——那会让测量结果与目标不符。
func TestProbeUsesMatchingNetworkFamily(t *testing.T) {
	cases := []struct {
		ip      string
		port    int
		network string
	}{
		{"1.2.3.4", 443, "tcp4"},
		{"2001:db8::1", 443, "tcp6"},
		{"::ffff:1.2.3.4", 443, "tcp4"}, // IPv4-mapped 归一化后按 IPv4 处理
	}

	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			var gotNetwork string
			p := New(Config{Dialer: okDialer(0, &gotNetwork, nil)})

			result := p.Probe(context.Background(), mustTarget(t, tc.ip, tc.port))
			if !result.Success {
				t.Fatalf("Probe failed: %+v", result)
			}
			if gotNetwork != tc.network {
				t.Errorf("dial network = %q, want %q", gotNetwork, tc.network)
			}
		})
	}
}

func TestProbeMeasuresLatency(t *testing.T) {
	const delay = 25 * time.Millisecond

	p := New(Config{Timeout: time.Second, Dialer: okDialer(delay, nil, nil)})
	result := p.Probe(context.Background(), mustTarget(t, "1.2.3.4", 443))

	if !result.Success {
		t.Fatalf("Probe failed: %+v", result)
	}
	// 允许较大的下限余量（Windows 上 time.Since 精度与调度抖动都更大），
	// 但必须能体现出 dialer 里刻意加入的延迟。
	if result.LatencyMS < 20 {
		t.Errorf("LatencyMS = %v, want >= 20 (dialer slept %s)", result.LatencyMS, delay)
	}
	if result.LatencyMS > 500 {
		t.Errorf("LatencyMS = %v, looks wrong for a %s sleep", result.LatencyMS, delay)
	}
	if result.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
	if !result.Timestamp.Before(time.Now().UTC().Add(time.Second)) {
		t.Error("Timestamp is in the future")
	}
}

// TestProbeInvalidTargetIsClassifiedNotError 确认数据问题与网络问题分开。
func TestProbeInvalidTargetIsClassifiedNotError(t *testing.T) {
	p := New(Config{Dialer: okDialer(0, nil, nil)})

	// 直接构造一个非法目标（绕过构造函数）。
	broken := model.Target{ID: "1.2.3.4:99999", IP: "1.2.3.4", Port: 99999, IPVersion: model.IPVersionIPv4}
	result := p.Probe(context.Background(), broken)

	if result.Success {
		t.Fatal("Probe succeeded for an invalid target")
	}
	if result.ErrorType != ErrorTypeInvalidTarget {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, ErrorTypeInvalidTarget)
	}
	// 数据问题不能算作线路丢包证据。
	if result.ErrorType.CountsTowardLoss() {
		t.Error("invalid_target must not count toward packet loss")
	}
}

// TestProbeSingleTargetTimeout 确定性地验证单目标超时分类与耗时上限。
func TestProbeSingleTargetTimeout(t *testing.T) {
	const timeout = 80 * time.Millisecond

	p := New(Config{Timeout: timeout, Dialer: waitCtxDialer()})

	start := time.Now()
	result := p.Probe(context.Background(), mustTarget(t, "1.2.3.4", 443))
	elapsed := time.Since(start)

	if result.Success {
		t.Fatal("Probe succeeded, want timeout")
	}
	if result.ErrorType != ErrorTypeTimeout {
		t.Errorf("ErrorType = %q, want %q (message=%q)", result.ErrorType, ErrorTypeTimeout, result.ErrorMessage)
	}
	// 必须在 timeout 附近返回，不能无限等待。
	if elapsed > timeout*10 {
		t.Errorf("Probe took %s for timeout %s", elapsed, timeout)
	}
	if result.ErrorMessage == "" {
		t.Error("ErrorMessage is empty for a failed probe")
	}
}

// TestProbeParentCancelIsNotTimeout 确认 Ctrl+C 不会被误判成线路超时。
//
// 这是数据质量的关键：把"用户中断"记成"超时"会直接污染丢包率。
func TestProbeParentCancelIsNotTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := New(Config{Timeout: time.Second, Dialer: waitCtxDialer()})
	result := p.Probe(ctx, mustTarget(t, "1.2.3.4", 443))

	if result.Success {
		t.Fatal("Probe succeeded with a canceled context")
	}
	if result.ErrorType != ErrorTypeCanceled {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, ErrorTypeCanceled)
	}
	if result.ErrorType.CountsTowardLoss() {
		t.Error("canceled must not count toward packet loss")
	}
}

// TestProbeClassifiesDialErrors 端到端验证错误被带到结果里。
func TestProbeClassifiesDialErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorType
	}{
		{"refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, ErrorTypeConnectionRefused},
		{"reset", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}, ErrorTypeConnectionReset},
		{"unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}, ErrorTypeNetworkUnreachable},
		{"permission", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}, ErrorTypePermissionDenied},
		{"other", errors.New("mysterious"), ErrorTypeOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{Dialer: errDialer(tc.err)})
			result := p.Probe(context.Background(), mustTarget(t, "1.2.3.4", 2053))

			if result.Success {
				t.Fatal("Probe succeeded, want failure")
			}
			if result.ErrorType != tc.want {
				t.Errorf("ErrorType = %q, want %q (message=%q)", result.ErrorType, tc.want, result.ErrorMessage)
			}
			if result.Port != 2053 {
				t.Errorf("Port = %d, want 2053", result.Port)
			}
			if err := result.Valid(); err != nil {
				t.Errorf("result invalid: %v", err)
			}
		})
	}
}

func TestProbeConfigNormalization(t *testing.T) {
	p := New(Config{})
	cfg := p.Config()
	if cfg.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want %d", cfg.Workers, DefaultWorkers)
	}
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %s, want %s", cfg.Timeout, DefaultTimeout)
	}

	p = New(Config{Workers: -5, Timeout: -time.Second})
	cfg = p.Config()
	if cfg.Workers != DefaultWorkers || cfg.Timeout != DefaultTimeout {
		t.Errorf("negative values not normalized: %+v", cfg)
	}

	// 并发必须被截断在安全上限内，避免误配置把本机网络打崩。
	p = New(Config{Workers: 1 << 20})
	if got := p.Config().Workers; got != MaxWorkers {
		t.Errorf("Workers = %d, want clamped to %d", got, MaxWorkers)
	}
}

// ---------------------------------------------------------------------------
// 真实 TCP 行为（本机）
// ---------------------------------------------------------------------------

// TestProbeRealLocalListener 用真实的本机 TCP 监听验证整条路径：
// net.Dialer -> 真实握手 -> 结果。
//
// 前面的测试都用注入的 dialer 覆盖分类逻辑，这个测试负责确认
// "默认 dialer 的参数拼装是对的"（地址、地址族、超时）。
func TestProbeRealLocalListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	// 接受连接并立刻关闭：探测只需完成握手。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	addrPort, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", listener.Addr())
	}
	ip := addrPort.IP.String()
	port := addrPort.Port

	p := New(Config{Timeout: 2 * time.Second})
	result := p.Probe(context.Background(), mustTarget(t, ip, port))

	if !result.Success {
		t.Fatalf("Probe to a live listener failed: %+v", result)
	}
	if result.ErrorType != ErrorTypeNone {
		t.Errorf("ErrorType = %q, want empty", result.ErrorType)
	}
	if result.LatencyMS < 0 || result.LatencyMS > 2000 {
		t.Errorf("LatencyMS = %v, want [0, 2000] for loopback", result.LatencyMS)
	}
	if err := result.Valid(); err != nil {
		t.Errorf("result invalid: %v", err)
	}

	_ = listener.Close()
	<-done
}

// TestProbeRealRefusedPort 用真实的本机端口验证 refused 分类。
//
// 做法：先监听拿到一个确定空闲的端口，然后关闭监听并立刻探测。
// 在没有服务监听的端口上，TCP 握手会得到 RST
// （Windows: WSAECONNREFUSED 10061，Unix: ECONNREFUSED）。
func TestProbeRealRefusedPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addrPort, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", listener.Addr())
	}
	ip := addrPort.IP.String()
	port := addrPort.Port

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	p := New(Config{Timeout: 2 * time.Second})
	result := p.Probe(context.Background(), mustTarget(t, ip, port))

	if result.Success {
		t.Skipf("port %d is unexpectedly accepting connections again; skipping", port)
	}
	// 本机上关闭的端口通常立刻回 RST；极少数情况下会被丢弃导致 timeout。
	// 两种都属于"确实测不到"，因此这里接受 refused/timeout，
	// 但拒绝其它分类（那说明分类逻辑有问题）。
	switch result.ErrorType {
	case ErrorTypeConnectionRefused, ErrorTypeTimeout:
		// ok
	default:
		t.Errorf("ErrorType = %q (%s), want connection_refused or timeout",
			result.ErrorType, result.ErrorMessage)
	}
	if err := result.Valid(); err != nil {
		t.Errorf("result invalid: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ParseTarget
// ---------------------------------------------------------------------------

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in          string
		defaultPort int
		wantID      string
		wantErr     bool
	}{
		{"1.2.3.4:443", 0, "1.2.3.4:443", false},
		{"1.2.3.4:2053", 0, "1.2.3.4:2053", false},
		{" 1.2.3.4:8443 ", 0, "1.2.3.4:8443", false},
		{"[2001:db8::1]:443", 0, "[2001:db8::1]:443", false},
		{"1.2.3.4", 443, "1.2.3.4:443", false},
		{"1.2.3.4", 2053, "1.2.3.4:2053", false},
		{"1.2.3.4:0", 0, "", true},
		{"1.2.3.4:65536", 0, "", true},
		{"1.2.3.4:99999", 0, "", true},
		{"1.2.3.4:abc", 0, "", true},
		{"1.2.3.4:", 0, "", true},
		{"not-an-ip:443", 0, "", true},
		{"", 443, "", true},
		{"   ", 443, "", true},
		{"1.2.3.4", 0, "", true},       // 默认端口非法
		{"2001:db8::1", 443, "", true}, // 不带方括号的 IPv6 有歧义，必须报错而不是猜
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			target, err := ParseTarget(tc.in, tc.defaultPort)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTarget(%q, %d) succeeded, want error", tc.in, tc.defaultPort)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q, %d): %v", tc.in, tc.defaultPort, err)
			}
			if target.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", target.ID, tc.wantID)
			}
			if err := target.Validate(); err != nil {
				t.Errorf("parsed target invalid: %v", err)
			}
		})
	}
}

// TestParseTargetDoesNotOverrideExplicitPort 是需求第 13 条的另一个角度：
// 即使用户给了默认端口，也不能覆盖目标里**已经写明**的端口。
func TestParseTargetDoesNotOverrideExplicitPort(t *testing.T) {
	target, err := ParseTarget("1.2.3.4:2053", 443)
	if err != nil {
		t.Fatal(err)
	}
	if target.Port != 2053 {
		t.Errorf("Port = %d, want 2053 (explicit port must win over the default)", target.Port)
	}
}

func TestParsePort(t *testing.T) {
	cases := map[string]int{
		"1":     1,
		"443":   443,
		"65535": 65535,
		"0443":  443,
	}
	for in, want := range cases {
		got, err := parsePort(in)
		if err != nil {
			t.Errorf("parsePort(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parsePort(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"", " ", "0", "65536", "-1", "abc", "44a", "99999999"} {
		if _, err := parsePort(in); err == nil {
			t.Errorf("parsePort(%q) succeeded, want error", in)
		} else if !errors.Is(err, model.ErrInvalidTarget) {
			t.Errorf("parsePort(%q) error = %v, want ErrInvalidTarget", in, err)
		}
	}
}

// ---------------------------------------------------------------------------
// ProbeAddr 便捷入口
// ---------------------------------------------------------------------------

func TestProbeAddr(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	addrPort := listener.Addr().(*net.TCPAddr)

	result := ProbeAddr(context.Background(), Config{Timeout: 2 * time.Second},
		addrPort.IP.String(), addrPort.Port)
	if !result.Success {
		t.Fatalf("ProbeAddr failed: %+v", result)
	}

	// 非法 IP 必须返回 invalid_target 结果，而不是 error。
	bad := ProbeAddr(context.Background(), DefaultConfig(), "not-an-ip", 443)
	if bad.ErrorType != ErrorTypeInvalidTarget {
		t.Errorf("ErrorType = %q, want %q", bad.ErrorType, ErrorTypeInvalidTarget)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// fakeTimeoutError 实现 net.Error 且 Timeout() 为真。
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

// canceledContext 返回一个已经取消的 context。
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// ensure netip is used (AddrPortOf relies on it); keeps the import honest.
var _ = netip.AddrPortFrom

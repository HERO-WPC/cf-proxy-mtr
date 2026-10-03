// Package probe 实现项目的第一级测量：TCP 连通性与延迟探测。
//
// 职责边界（必须与线路跟踪严格分开）：
//
//	probe    回答"这个 IP:Port 能不能连、连上要多少毫秒"
//	trace    回答"去这个 IP:Port 的路径经过哪里"
//
// 本包只做前者，不调用任何外部进程，也不写数据库。
//
// 关键约束（对应需求第 12、13、14 条）：
//
//   - 测量的是目标**自身的端口**，例如 "1.2.3.4:2053" 测 TCP 2053，
//     绝不固定成 443；
//   - 单个目标失败绝不能影响整体，失败结果与成功结果一样必须返回；
//   - 并发严格受限（worker pool），绝不每个目标起一个 goroutine；
//   - 不采集、不返回任何本机网络信息（本机 IP、接口、MAC 等）。
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 默认参数（需求第 12 条）。
const (
	// DefaultTimeout 是单个 TCP 连接的超时时间。
	DefaultTimeout = 3 * time.Second

	// DefaultWorkers 是默认并发数。
	//
	// 100 是刻意的选择：项目瓶颈是网络 IO 与超时，不是 CPU。
	// 更高的并发会让本机连接表、家用路由器 NAT 会话表先饱和，
	// 结果是"测出来的延迟是排队延迟"，数据反而失去意义。
	DefaultWorkers = 100

	// MaxWorkers 是允许配置的并发上限。
	//
	// 目的是防止误配置（例如 --workers 100000）导致
	// 文件描述符耗尽或把家用网络打崩。
	MaxWorkers = 2000

	// maxErrorMessage 限制错误消息长度，避免把整个错误链写进数据库。
	maxErrorMessage = 256
)

// ErrorType 是探测失败的分类。
//
// 分类是给后续分析用的：数据库里保存的是"哪一种失败"，
// 而不是一段无法聚合的报错文本。
// 例如"示例省移动对 1.2.3.4:443 的 connection_refused 占 88%"
// 本身就是有价值的线路信息。
type ErrorType string

const (
	// ErrorTypeNone 表示成功（没有错误）。
	ErrorTypeNone ErrorType = ""

	// ErrorTypeTimeout 表示超时（含整体 context 超时）。
	ErrorTypeTimeout ErrorType = "timeout"

	// ErrorTypeConnectionRefused 表示对端明确拒绝（TCP RST）。
	//
	// 通常意味着"IP 可达，但该端口没有服务"，
	// 与 timeout 的含义完全不同，必须区分。
	ErrorTypeConnectionRefused ErrorType = "connection_refused"

	// ErrorTypeNetworkUnreachable 表示网络/主机不可达（含无路由、路由黑洞）。
	ErrorTypeNetworkUnreachable ErrorType = "network_unreachable"

	// ErrorTypePermissionDenied 表示本机策略拒绝（防火墙、安全软件、
	// 或受限环境不允许建立该连接）。
	ErrorTypePermissionDenied ErrorType = "permission_denied"

	// ErrorTypeConnectionReset 表示连接被对端重置（RST）。
	//
	// 与 connection_refused 的区别：refused 是握手阶段被拒，
	// reset 是连接建立后（或握手中途）被重置。
	ErrorTypeConnectionReset ErrorType = "connection_reset"

	// ErrorTypeNoRoute 表示本机路由表没有可用路由。
	ErrorTypeNoRoute ErrorType = "no_route"

	// ErrorTypeAddressNotAvailable 表示本机没有匹配该地址族的地址
	// （例如纯 IPv4 环境探测 IPv6 目标）。
	ErrorTypeAddressNotAvailable ErrorType = "address_not_available"

	// ErrorTypeCanceled 表示本次探测因为整体取消（例如 Ctrl+C）而放弃。
	//
	// 它**不是**线路问题的证据，因此单独分类，
	// 聚合时不应把它算成丢包。
	ErrorTypeCanceled ErrorType = "canceled"

	// ErrorTypeInvalidTarget 表示目标本身不合法（IP 无法解析等），
	// 属于数据问题而不是网络问题。
	ErrorTypeInvalidTarget ErrorType = "invalid_target"

	// ErrorTypeOther 是兜底分类：无法归入以上的连接错误。
	ErrorTypeOther ErrorType = "other"
)

// AllErrorTypes 返回所有可能的错误分类（不含"成功"），便于测试与统计。
func AllErrorTypes() []ErrorType {
	return []ErrorType{
		ErrorTypeTimeout,
		ErrorTypeConnectionRefused,
		ErrorTypeNetworkUnreachable,
		ErrorTypePermissionDenied,
		ErrorTypeConnectionReset,
		ErrorTypeNoRoute,
		ErrorTypeAddressNotAvailable,
		ErrorTypeCanceled,
		ErrorTypeInvalidTarget,
		ErrorTypeOther,
	}
}

// Valid 报告分类是否是已定义的取值。
func (t ErrorType) Valid() bool {
	if t == ErrorTypeNone {
		return true
	}
	for _, known := range AllErrorTypes() {
		if t == known {
			return true
		}
	}
	return false
}

// IsFailure 报告该分类是否代表一次失败（用于统计成功率）。
//
// canceled 与 invalid_target 都被算作"失败"：它们确实没有测到结果。
// 但调用方在聚合"丢包率"时应当排除 canceled，
// 因为那是本机主动放弃，不是线路证据。
func (t ErrorType) IsFailure() bool {
	return t != ErrorTypeNone
}

// CountsTowardLoss 报告该分类是否可以作为"线路丢包"的证据。
//
// canceled 是主动放弃，invalid_target 是数据问题：
// 两者都不能算成目标不可达，否则会污染众测数据库。
func (t ErrorType) CountsTowardLoss() bool {
	switch t {
	case ErrorTypeNone, ErrorTypeCanceled, ErrorTypeInvalidTarget:
		return false
	default:
		return true
	}
}

// ProbeResult 是一次 TCP 探测的结果。
//
// 字段刻意保持扁平：它会直接进入 SQLite 的 measurements 表，
// 也会进入公开的 JSONL 导出数据。
//
// 注意 Success 与 ErrorType 的关系：
//
//	Success = true   则 ErrorType == ErrorTypeNone
//	Success = false  则 ErrorType != ErrorTypeNone（一定带有具体分类）
//
// 失败结果**必须保存**：失败本身也是线路信息（需求第 39 条）。
type ProbeResult struct {
	// TargetID 是 "IP:Port" 形式的稳定标识。
	TargetID string

	// IP 是规范化后的目标 IP（不含端口）。
	IP string

	// Port 是被探测的**目标端口**。
	//
	// 例如 1.2.3.4:2053 这里就是 2053，绝不是 443。
	Port int

	// Success 表示 TCP 握手是否成功。
	Success bool

	// LatencyMS 是 TCP 握手耗时（毫秒，带小数）。
	//
	// 成功时是真实握手耗时；失败时记录"失败发生前等待了多久"，
	// 便于区分"立即被拒"与"等到超时"。
	LatencyMS float64

	// ErrorType 是失败分类；成功时为空。
	ErrorType ErrorType

	// ErrorMessage 是给人类看的简短原因；成功时为空。
	ErrorMessage string

	// Timestamp 是本次探测开始的时间（UTC）。
	Timestamp time.Time
}

// Failed 报告该结果是否是失败。
func (r ProbeResult) Failed() bool { return !r.Success }

// Valid 检查结果的内部一致性。
//
// 用于测试与"结果入库前"的自检：
// 一个结果不能既成功又带错误分类，也不能既失败又没有分类。
func (r ProbeResult) Valid() error {
	if r.TargetID == "" {
		return errors.New("probe: empty target id")
	}
	if !model.ValidPort(r.Port) {
		return fmt.Errorf("probe: port %d out of range", r.Port)
	}
	if _, ok := model.ParseAddr(r.IP); !ok {
		return fmt.Errorf("probe: invalid ip %q", r.IP)
	}
	if r.Success {
		if r.ErrorType != ErrorTypeNone {
			return fmt.Errorf("probe: successful result carries error type %q", r.ErrorType)
		}
		return nil
	}
	if r.ErrorType == ErrorTypeNone {
		return errors.New("probe: failed result has no error type")
	}
	if !r.ErrorType.Valid() {
		return fmt.Errorf("probe: unknown error type %q", r.ErrorType)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// Stats 是一批探测的汇总统计。
//
// 它是"观察结果"，不参与数据库写入：数据库保存的是逐条 measurement，
// 统计是给用户看进度与健康度的。
type Stats struct {
	// Total 是提交给 worker pool 的目标数。
	Total int

	// Completed 是实际得到结果的目标数。
	//
	// 被 Ctrl+C 中断时 Completed < Total，这是断点续测的判据之一。
	Completed int

	// Success 是连接成功数。
	Success int

	// Failed 是连接失败数。
	Failed int

	// Dropped 是"已经测到、但没能交给消费者"的结果数。
	//
	// 只会在消费者跟不上（或提前退出）时出现。必须显式汇报：
	// 否则汇总里的 Completed 会莫名小于 Total，而上层
	// 无法分辨"目标本来就少"还是"结果被丢了"。
	Dropped int

	// ErrorCounts 是各失败分类的计数。
	ErrorCounts map[ErrorType]int

	// LatencyMS 是成功探测的延迟样本（毫秒），用于计算分位数。
	LatencyMS []float64

	// Interrupted 表示本次运行被整体取消（例如 Ctrl+C）而没有跑完。
	Interrupted bool
}

// SuccessRate 返回成功率。没有样本时返回 0。
func (s Stats) SuccessRate() float64 {
	if s.Completed == 0 {
		return 0
	}
	return float64(s.Success) / float64(s.Completed)
}

// FailureRate 返回失败率。没有样本时返回 0。
func (s Stats) FailureRate() float64 {
	if s.Completed == 0 {
		return 0
	}
	return float64(s.Failed) / float64(s.Completed)
}

// LossRate 返回可作为线路证据的失败占比。
//
// 与 FailureRate 的区别：排除 canceled 与 invalid_target，
// 因为这两个分类不是线路质量的证据（见 ErrorType.CountsTowardLoss）。
func (s Stats) LossRate() float64 {
	if s.Completed == 0 {
		return 0
	}
	loss := 0
	for errType, n := range s.ErrorCounts {
		if errType.CountsTowardLoss() {
			loss += n
		}
	}
	return float64(loss) / float64(s.Completed)
}

// ---------------------------------------------------------------------------
// 探测实现
// ---------------------------------------------------------------------------

// DialContextFunc 是与 net.Dialer.DialContext 签名一致的可注入拨号函数。
//
// 之所以把它抽出来：TCP 错误分类是本包最需要被验证的逻辑，
// 但"让操作系统真的产生 connection refused / unreachable"既慢又不稳定。
// 注入拨号函数后，每一种错误都能被确定性地构造出来测试
// （包括 Windows 特有的 WSA 错误码）。
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config 是探测配置。
type Config struct {
	// Workers 是并发上限。<=0 时使用 DefaultWorkers，>MaxWorkers 时被截断。
	Workers int

	// Timeout 是单个连接的超时。<=0 时使用 DefaultTimeout。
	Timeout time.Duration

	// Dialer 允许注入自定义拨号实现（测试使用）。
	//
	// 为 nil 时使用默认 net.Dialer。
	Dialer DialContextFunc

	// KeepAlive 是建立连接后是否启用 TCP keep-alive。
	//
	// 默认关闭是刻意的：本探测只关心"能不能握手"，
	// 探测结束立即关闭连接，开启 keep-alive 没有任何收益，
	// 反而可能让连接在系统里多存活一段时间。
	KeepAlive bool
}

// DefaultConfig 返回默认探测配置。
func DefaultConfig() Config {
	return Config{
		Workers: DefaultWorkers,
		Timeout: DefaultTimeout,
	}
}

// normalize 补齐零值并把并发限制在安全范围内。
func (c Config) normalize() Config {
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Workers > MaxWorkers {
		c.Workers = MaxWorkers
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// Prober 执行单个目标的 TCP 探测。
//
// Prober 是并发安全的：所有字段在构造后只读，
// 因此可以被任意多个 worker 同时使用。
type Prober struct {
	cfg    Config
	dialer DialContextFunc
}

// New 创建 Prober。
func New(cfg Config) *Prober {
	cfg = cfg.normalize()

	dialer := cfg.Dialer
	if dialer == nil {
		d := &net.Dialer{
			Timeout: cfg.Timeout,
			// 不做本地地址绑定：我们不关心、也不记录本机地址。
		}
		if !cfg.KeepAlive {
			d.KeepAlive = -1 // 负数表示禁用 keep-alive
		}
		dialer = d.DialContext
	}

	return &Prober{cfg: cfg, dialer: dialer}
}

// Config 返回生效后的配置。
func (p *Prober) Config() Config { return p.cfg }

// Probe 探测单个目标。
//
// 重要的实现约定：
//
//  1. 使用 target.Network()（tcp4/tcp6）而不是笼统的 "tcp"。
//     这样目标声明的地址族会被强制遵守，避免系统把 IPv6 目标
//     解析成 IPv4，也避免"测了另一个地址"这种静默偏差。
//  2. 使用 target.Address()（IPv6 带方括号）作为拨号地址。
//  3. 端口来自 target.Port，不做任何替换。
//  4. 任何错误都被分类后返回，**不返回 error**：
//     单个目标失败不是"函数失败"，调用方不需要区分处理。
//
// ctx 用于整体取消（Ctrl+C）；单个目标的超时由 Config.Timeout 控制。
func (p *Prober) Probe(ctx context.Context, target model.Target) ProbeResult {
	start := time.Now().UTC()

	result := ProbeResult{
		TargetID:  target.String(),
		IP:        target.IP,
		Port:      target.Port,
		Timestamp: start,
	}

	// 目标合法性检查：数据问题必须与网络问题区分开。
	if err := target.Validate(); err != nil {
		result.ErrorType = ErrorTypeInvalidTarget
		result.ErrorMessage = truncateMessage(err.Error())
		return result
	}

	// 已经取消：不浪费一次连接尝试。
	if err := ctx.Err(); err != nil {
		result.ErrorType = ErrorTypeCanceled
		result.ErrorMessage = truncateMessage(err.Error())
		return result
	}

	network := target.Network()
	address := target.Address()

	// 单目标超时：在该目标的探测上再加一层 deadline，
	// 与整体取消（ctx）合并——两者任一触发都会终止本次连接。
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	conn, err := p.dialer(probeCtx, network, address)
	latency := time.Since(start)

	result.LatencyMS = milliseconds(latency)
	if err != nil {
		result.ErrorType, result.ErrorMessage = classify(err, ctx)
		return result
	}

	// 连接成功：立刻关闭，不留任何连接。
	// 关闭失败无补救手段，也不影响本次测量结论，因此显式忽略。
	_ = conn.Close()
	result.Success = true
	return result
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// classify 把连接错误映射为分类 + 简短说明。
//
// parent 是**整体** context：用它区分"用户取消"与"单目标超时"，
// 因为两者都会在错误链里表现为 context 错误，但含义完全不同——
// 把 Ctrl+C 当成线路超时会污染数据。
func classify(err error, parent context.Context) (ErrorType, string) {
	message := truncateMessage(err.Error())

	// 1) 整体取消优先判断：这是本机主动放弃，不是线路问题。
	if parent != nil && parent.Err() != nil && errors.Is(parent.Err(), context.Canceled) {
		if errors.Is(err, context.Canceled) {
			return ErrorTypeCanceled, message
		}
	}

	// 2) 超时：包含 context deadline 与 net.Error.Timeout()。
	//    Windows 上超时通常表现为 WSAETIMEDOUT，同样满足 net.Error.Timeout()。
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTypeTimeout, message
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrorTypeTimeout, message
	}

	// 3) 逐层取出底层错误码做精确分类。
	//
	//    顺序很重要：先判断更具体的原因（拒绝 > 重置 > 不可达）。
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if t, ok := classifyErrno(err); ok {
			return t, message
		}
	}

	// 4) DNS 解析失败：目标是 IP，正常不会出现；
	//    出现说明输入不是合法地址，属于数据问题。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrorTypeOther, message
	}

	// 5) 兜底：按文本特征再试一次。
	//
	//    为什么还要看文本：不同平台/不同 Go 版本的错误包装层数不同，
	//    syscall.Errno 可能被包在自定义错误里而不满足上面的类型断言。
	//    这是一种"宁可比对字符串也不要错误分类"的取舍，
	//    但只在前面所有结构化判断都失败时才生效。
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "refused"):
		return ErrorTypeConnectionRefused, message
	case strings.Contains(lower, "reset by peer"):
		return ErrorTypeConnectionReset, message
	case strings.Contains(lower, "network is unreachable"):
		return ErrorTypeNetworkUnreachable, message
	case strings.Contains(lower, "no route to host"):
		return ErrorTypeNoRoute, message
	case strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "access is denied"),
		strings.Contains(lower, "forbidden"):
		return ErrorTypePermissionDenied, message
	case strings.Contains(lower, "cannot assign requested address"),
		strings.Contains(lower, "address not available"),
		strings.Contains(lower, "no suitable address"):
		return ErrorTypeAddressNotAvailable, message
	case strings.Contains(lower, "i/o timeout"):
		return ErrorTypeTimeout, message
	default:
		return ErrorTypeOther, message
	}
}

// milliseconds 把时长换算成毫秒（保留小数，便于体现亚毫秒差异）。
func milliseconds(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}

// truncateMessage 限制错误消息长度。
func truncateMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorMessage {
		return s
	}
	return s[:maxErrorMessage] + "..."
}

// ---------------------------------------------------------------------------
// 便捷入口
// ---------------------------------------------------------------------------

// ProbeAddr 探测一个 "IP:Port"，返回结果。
//
// 供不便构造 model.Target 的场景使用（例如命令行 --target 参数）。
// 目标非法时返回 invalid_target 分类的结果，而不是 error。
func ProbeAddr(ctx context.Context, cfg Config, ip string, port int) ProbeResult {
	target, err := model.NewTargetFromStrings(ip, port)
	if err != nil {
		return ProbeResult{
			TargetID:     model.TargetID(ip, port),
			IP:           ip,
			Port:         port,
			ErrorType:    ErrorTypeInvalidTarget,
			ErrorMessage: truncateMessage(err.Error()),
			Timestamp:    time.Now().UTC(),
		}
	}
	return New(cfg).Probe(ctx, target)
}

// ParseTarget 解析 "IP:Port" 或 "IP" + 默认端口的写法。
//
// 支持的输入形式：
//
//	1.2.3.4:443
//	[2001:db8::1]:443
//	1.2.3.4          （端口使用 defaultPort）
//
// 之所以支持最后一种：用户排查问题时经常只想测 443，
// 不必强制写端口。但绝不因此把已写明的端口覆盖掉。
func ParseTarget(s string, defaultPort int) (model.Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return model.Target{}, fmt.Errorf("%w: empty target", model.ErrInvalidTarget)
	}

	host, portStr, err := splitHostPort(s)
	if err != nil {
		// 没有端口部分：用默认端口。
		addr, ok := model.ParseAddr(s)
		if !ok {
			return model.Target{}, fmt.Errorf("%w: %q is not an ip or ip:port", model.ErrInvalidTarget, s)
		}
		if !model.ValidPort(defaultPort) {
			return model.Target{}, fmt.Errorf("%w: default port %d out of range", model.ErrInvalidTarget, defaultPort)
		}
		target, ok := model.NewTarget(addr, defaultPort)
		if !ok {
			return model.Target{}, fmt.Errorf("%w: %q", model.ErrInvalidTarget, s)
		}
		return target, nil
	}

	port, err := parsePort(portStr)
	if err != nil {
		return model.Target{}, err
	}
	target, err := model.NewTargetFromStrings(host, port)
	if err != nil {
		return model.Target{}, err
	}
	return target, nil
}

// splitHostPort 拆分 host:port，支持 IPv6 的方括号写法。
//
// 返回 err 表示"没有端口部分"，而不是"格式错误"。
func splitHostPort(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", fmt.Errorf("missing ]")
		}
		host = s[1:end]
		rest := s[end+1:]
		if !strings.HasPrefix(rest, ":") || len(rest) < 2 {
			return "", "", fmt.Errorf("no port")
		}
		return host, rest[1:], nil
	}

	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port")
	}
	// IPv6 不带方括号且没有端口时会走到这里（例如 "2001:db8::1"），
	// LastIndex 切出来的"端口"不是数字，parsePort 会报错，
	// 这是可接受的：这种写法本身有歧义，应当报错而不是猜测。
	return s[:i], s[i+1:], nil
}

// parsePort 解析端口字符串。
func parsePort(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty port", model.ErrInvalidTarget)
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: port %q is not a number", model.ErrInvalidTarget, s)
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return 0, fmt.Errorf("%w: port %s out of range", model.ErrInvalidTarget, s)
		}
	}
	if !model.ValidPort(n) {
		return 0, fmt.Errorf("%w: port %d out of range", model.ErrInvalidTarget, n)
	}
	return n, nil
}

// AddrPortOf 返回结果的 "IP:Port" 地址（IPv6 带方括号）。
//
// 供日志与后续 NextTrace 调用复用，避免各处重复拼接。
func (r ProbeResult) AddrPortOf() string {
	if addr, ok := model.ParseAddr(r.IP); ok {
		return netip.AddrPortFrom(addr, uint16(r.Port)).String()
	}
	return r.TargetID
}

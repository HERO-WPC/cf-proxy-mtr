package trace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ErrorType 是跟踪失败的分类。
//
// 与 probe.ErrorType 分开定义：两者的失败原因集合不同
// （跟踪会遇到"引擎不存在""原始 socket 权限不足"这类 probe 不会有的问题），
// 混在一起会逼着两边都接受对方的取值。
type ErrorType string

const (
	// ErrorTypeNone 表示成功。
	ErrorTypeNone ErrorType = ""

	// ErrorTypeNotFound 表示找不到可执行文件。
	//
	// 这不是线路问题，而是环境问题——必须与跟踪失败区分开，
	// 否则用户会去排查网络。
	ErrorTypeNotFound ErrorType = "engine_not_found"

	// ErrorTypeTimeout 表示超时。
	ErrorTypeTimeout ErrorType = "timeout"

	// ErrorTypeCanceled 表示本次运行被中断（不计入线路质量）。
	ErrorTypeCanceled ErrorType = "canceled"

	// ErrorTypePermission 表示权限不足。
	//
	// traceroute 的 ICMP 模式与原始 socket 在多数系统上需要
	// 管理员权限；TCP 模式通常不需要。
	ErrorTypePermission ErrorType = "permission_denied"

	// ErrorTypeExitCode 表示进程以非零退出码结束。
	ErrorTypeExitCode ErrorType = "engine_failed"

	// ErrorTypeParse 表示输出无法解析。
	ErrorTypeParse ErrorType = "parse_error"

	// ErrorTypeInvalidTarget 表示目标本身不合法。
	ErrorTypeInvalidTarget ErrorType = "invalid_target"

	// ErrorTypeOther 是兜底分类。
	ErrorTypeOther ErrorType = "other"
)

// AllErrorTypes 返回所有分类（不含"成功"）。
func AllErrorTypes() []ErrorType {
	return []ErrorType{
		ErrorTypeNotFound,
		ErrorTypeTimeout,
		ErrorTypeCanceled,
		ErrorTypePermission,
		ErrorTypeExitCode,
		ErrorTypeParse,
		ErrorTypeInvalidTarget,
		ErrorTypeOther,
	}
}

// Valid 报告分类是否已定义。
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

// CountsTowardPathQuality 报告该分类是否可以作为"路径质量"的证据。
//
// 环境问题（引擎不存在、权限、解析失败、主动取消）都不能算：
// 它们说明"我们没测成"，而不是"这条路径不好"。
func (t ErrorType) CountsTowardPathQuality() bool {
	switch t {
	case ErrorTypeNone, ErrorTypeNotFound, ErrorTypePermission,
		ErrorTypeParse, ErrorTypeCanceled, ErrorTypeInvalidTarget:
		return false
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// 可用性检查
// ---------------------------------------------------------------------------

// Availability 描述 nexttrace 是否可用。
type Availability struct {
	// Found 表示可执行文件被找到。
	Found bool

	// Path 是解析出的绝对路径（Found 为真时有效）。
	Path string

	// Version 是报告的版本（可能为空）。
	Version string

	// Err 是"不可用"的原因（Found 为假时有效）。
	Err error
}

// CheckAvailability 检查 nexttrace 是否可用。
//
// 这是需求第 25 条的落点：
//
//	NextTrace not found.
//	Please install NextTrace or configure trace.nexttrace.binary.
//
// 关键约束：**绝不能**因为 nexttrace 不存在就让 TCP Probe 不可用。
// 因此这个函数只返回状态，不做任何全局副作用，
// 调用方（CLI）决定是提示还是报错。
func CheckAvailability(ctx context.Context, binaryPath string) Availability {
	path, err := resolveBinary(binaryPath)
	if err != nil {
		return Availability{Err: err}
	}

	// 真正执行一次 --version：只检查文件存在是不够的
	// （可能架构不匹配、可能没有执行权限、可能是坏文件）。
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Availability{
			Path: path,
			Err:  fmt.Errorf("running %q --version failed: %w", path, err),
		}
	}

	return Availability{
		Found:   true,
		Path:    path,
		Version: parseVersion(string(out)),
	}
}

// NotFoundMessage 返回"引擎不存在"时给用户的提示。
//
// 措辞固定，便于用户搜索，也便于测试断言。
const NotFoundMessage = "NextTrace not found.\n" +
	"Please install NextTrace or configure trace.nexttrace.binary."

// resolveBinary 解析可执行文件路径。
//
// 支持两种配置：
//   - 只写文件名（"nexttrace"）：从 PATH 查找；
//   - 绝对/相对路径：直接使用（Windows 上允许省略 .exe）。
func resolveBinary(binaryPath string) (string, error) {
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = DefaultBinary
	}

	// 含路径分隔符：当作文件路径处理。
	if strings.ContainsAny(binaryPath, `/\`) {
		candidate := binaryPath
		if _, err := os.Stat(candidate); err != nil {
			// Windows 上允许用户写 "C:/Tools/nexttrace" 而文件是 nexttrace.exe。
			if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(candidate), ".exe") {
				if _, errExe := os.Stat(candidate + ".exe"); errExe == nil {
					return filepath.Abs(candidate + ".exe")
				}
			}
			return "", fmt.Errorf("nexttrace binary %q is not usable: %w", candidate, err)
		}
		return filepath.Abs(candidate)
	}

	// 只有文件名：从 PATH 查找。
	if found, err := exec.LookPath(binaryPath); err == nil {
		return found, nil
	}

	// Windows 上 exec.LookPath 已经会补 .exe；这里再兜一次，
	// 因为用户可能把文件放在当前目录而不是 PATH 里。
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(binaryPath), ".exe") {
		if found, err := exec.LookPath(binaryPath + ".exe"); err == nil {
			return found, nil
		}
	}

	return "", fmt.Errorf("%w: %q not found in PATH", fs.ErrNotExist, binaryPath)
}

// versionPattern 从 --version 输出里提取版本号。
//
// 不同版本的输出格式不同（"NextTrace v1.7.3 ..."、"nexttrace 1.7.3" 等），
// 因此这里只抓第一个看起来像版本号的片段。
var versionPattern = regexp.MustCompile(`v?(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.\-]+)?)`)

// parseVersion 从 --version 输出里提取版本号，失败时返回原始首行。
func parseVersion(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}

	if match := versionPattern.FindStringSubmatch(output); len(match) > 1 {
		return match[1]
	}

	// 提取不到版本号时保留首行（截断），总比什么都没有强。
	first := strings.SplitN(output, "\n", 2)[0]
	first = strings.TrimSpace(first)
	if len(first) > 120 {
		first = first[:120]
	}
	return first
}

// ---------------------------------------------------------------------------
// NextTraceEngine
// ---------------------------------------------------------------------------

// NextTraceEngine 通过外部 nexttrace 可执行文件做线路跟踪。
type NextTraceEngine struct {
	// BinaryPath 是可执行文件路径或名字。
	BinaryPath string

	// Timeout 是单个进程的超时。
	Timeout time.Duration

	// Mode 是跟踪模式（默认 tcp）。
	Mode Mode

	// resolvedPath 是解析后的绝对路径（构造时解析一次）。
	resolvedPath string

	// version 是引擎版本（构造时查询一次）。
	version string

	// runCommand 允许注入命令执行（测试用）。
	//
	// 默认实现是 exec.CommandContext。注入点存在的原因：
	// "进程超时被 kill""stderr 有内容""退出码非零"这些路径
	// 用真实的 nexttrace 很难稳定复现。
	runCommand func(ctx context.Context, path string, args []string) (stdout []byte, stderr []byte, err error)
}

// EngineOptions 是构造 NextTraceEngine 的选项。
type EngineOptions struct {
	// BinaryPath 是可执行文件路径（空表示用默认名）。
	BinaryPath string

	// Timeout 是单个进程的超时（<=0 表示用默认值）。
	Timeout time.Duration

	// Mode 是跟踪模式（空表示 tcp）。
	Mode Mode

	// RunCommand 允许注入命令执行（测试用）。
	RunCommand func(ctx context.Context, path string, args []string) ([]byte, []byte, error)

	// SkipVersionCheck 为真时不查询版本（测试用）。
	SkipVersionCheck bool
}

// NewNextTraceEngine 构造引擎。
//
// 构造时会解析二进制路径并（默认）查询一次版本：
//   - 找不到二进制时返回带 NotFound 说明的 error；
//   - 版本查询失败不算致命（引擎可能仍能工作），只是 version 为空。
//
// 注意：返回 error 只表示"这个引擎不可用"，调用方应当据此
// **只禁用线路跟踪**，而不是让整个程序失败（需求第 25 条）。
func NewNextTraceEngine(ctx context.Context, opts EngineOptions) (*NextTraceEngine, error) {
	mode, err := opts.Mode.Normalize()
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	engine := &NextTraceEngine{
		BinaryPath: opts.BinaryPath,
		Timeout:    timeout,
		Mode:       mode,
		runCommand: opts.RunCommand,
	}

	// 注入自定义执行器时（测试）跳过真实解析。
	if opts.RunCommand != nil {
		engine.resolvedPath = opts.BinaryPath
		engine.version = "test"
		return engine, nil
	}

	path, err := resolveBinary(opts.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, NotFoundMessage)
	}
	engine.resolvedPath = path

	if !opts.SkipVersionCheck {
		if avail := CheckAvailability(ctx, path); avail.Found {
			engine.version = avail.Version
		}
		// 版本查不到不报错：有些构建的 --version 行为不同。
	}

	return engine, nil
}

// Name 实现 TraceEngine。
func (e *NextTraceEngine) Name() string { return "nexttrace" }

// Path 返回解析后的可执行文件路径。
func (e *NextTraceEngine) Path() string { return e.resolvedPath }

// Version 返回引擎版本（可能为空）。
func (e *NextTraceEngine) Version() string { return e.version }

// BuildArgs 构造 nexttrace 的命令行参数。
//
// 实测形态（NextTrace v1.7.3）：
//
//	nexttrace --tcp --port 443 --json <IP>
//	nexttrace --udp --port 2053 --json <IP>
//	nexttrace --icmp-mode 0 --json <IP>
//
// 三个必须遵守的点：
//
//  1. **显式写协议开关**（--tcp / --udp / --icmp-mode）。
//     v1.7.3 **没有 --traceroute 这个参数**，默认就是传统 traceroute。
//     早期文档关心的"未来默认会迁移到 MTR"在 v1.7.3 里的体现是
//     新增了 `--mtr` / `--report`（MTR 模式），且 `--report` 与 `--json`
//     互斥。因此本项目的做法是：不依赖默认值、也不碰 MTR，
//     而是显式指定探测协议，这样即使默认模式将来真的变了，
//     我们要的仍是"传统 traceroute + 指定协议 + 指定端口"。
//  2. **--port 用目标自己的端口**：1.2.3.4:2053 就传 2053，
//     绝不固定成 443——否则测的是另一个目标。
//  3. **--json**：程序只解析 JSON，不解析人类可读的终端表格
//     （那种输出会随终端宽度与颜色变化）。
func (e *NextTraceEngine) BuildArgs(target model.Target) ([]string, error) {
	if _, ok := target.Addr(); !ok {
		return nil, fmt.Errorf("%w: ip %q", model.ErrInvalidTarget, target.IP)
	}
	if !model.ValidPort(target.Port) {
		return nil, fmt.Errorf("%w: port %d", model.ErrInvalidTarget, target.Port)
	}

	// 只解析 JSON，不要颜色与表格。
	args := []string{"--json"}

	switch e.Mode {
	case ModeTCP:
		args = append(args, "--tcp", "--port", itoa(target.Port))
	case ModeUDP:
		args = append(args, "--udp", "--port", itoa(target.Port))
	case ModeICMP:
		// ICMP 模式没有端口概念。
		//
		// v1.7.3 的参数名是 --icmp-mode（后跟模式号），
		// 而不是 --icmp；模式 0 表示普通 ICMP Echo。
		args = append(args, "--icmp-mode", "0")
	default:
		return nil, fmt.Errorf("unsupported trace mode %q", e.Mode)
	}

	// 目标地址放在最后。
	//
	// 不加方括号：nexttrace 接受裸 IPv6 地址，
	// 而 "[::1]:443" 这种形式它并不认识（实测报 "unknown arguments"）。
	args = append(args, target.IP)
	return args, nil
}

// Trace 实现 TraceEngine。
//
// 单个目标失败**不返回 error**：返回带 ErrorType 的结果。
// 只有"构造参数失败"（目标非法）才走 error 之外的结果路径，
// 因此调用方只需处理"引擎整体不可用"这一种 error（在构造时已经暴露）。
func (e *NextTraceEngine) Trace(ctx context.Context, target model.Target) (*TraceResult, error) {
	start := time.Now().UTC()

	result := &TraceResult{
		TargetID:      target.String(),
		IP:            target.IP,
		Port:          target.Port,
		Engine:        e.Name(),
		EngineVersion: e.version,
		Mode:          e.Mode,
		Protocol:      string(e.Mode),
		Timestamp:     start,
	}

	// 目标非法：数据问题，与网络无关。
	if err := target.Validate(); err != nil {
		result.ErrorType = ErrorTypeInvalidTarget
		result.ErrorMessage = err.Error()
		return result, nil
	}

	args, err := e.BuildArgs(target)
	if err != nil {
		result.ErrorType = ErrorTypeInvalidTarget
		result.ErrorMessage = err.Error()
		return result, nil
	}

	// 已经取消：不做无谓的进程启动。
	if err := ctx.Err(); err != nil {
		result.ErrorType = ErrorTypeCanceled
		result.ErrorMessage = err.Error()
		return result, nil
	}

	traceCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()

	stdout, stderr, runErr := e.execute(traceCtx, args)
	result.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	result.Stderr = truncateBytes(stderr, maxStderrBytes)
	result.RawJSON = string(stdout)

	// 分类失败原因。顺序很重要：先看"环境/取消"，再看"输出内容"。
	if runErr != nil {
		result.ErrorType, result.ErrorMessage = classifyRunError(runErr, ctx, stderr, e.resolvedPath)
		return result, nil
	}

	if len(stdout) == 0 {
		result.ErrorType = ErrorTypeParse
		result.ErrorMessage = "engine produced no output"
		return result, nil
	}

	parsed, parseErr := ParseNextTraceJSON(stdout)
	if parseErr != nil {
		result.ErrorType = ErrorTypeParse
		result.ErrorMessage = parseErr.Error()
		return result, nil
	}

	// 解析成功：把路径与元数据填进结果。
	//
	// 以解析结果为准（而不是以退出码为准）是有意的：
	// 只要拿到了可解析的跳列表，这次跟踪就是有效的。
	result.Hops = parsed.Hops
	result.Success = true
	if parsed.Protocol != "" {
		result.Protocol = parsed.Protocol
	}
	return result, nil
}

// execute 运行引擎进程。
func (e *NextTraceEngine) execute(ctx context.Context, args []string) (stdout, stderr []byte, err error) {
	if e.runCommand != nil {
		return e.runCommand(ctx, e.resolvedPath, args)
	}

	cmd := exec.CommandContext(ctx, e.resolvedPath, args...)

	// 不用 CombinedOutput：stdout 是 JSON（必须干净），
	// stderr 是诊断信息（要分开收集，不能混进 JSON）。
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	return []byte(outBuf.String()), []byte(errBuf.String()), runErr
}

// classifyRunError 把进程执行失败映射成分类。
func classifyRunError(err error, parent context.Context, stderr []byte, path string) (ErrorType, string) {
	message := truncateBytes(stderr, maxStderrBytes)
	if message == "" {
		message = err.Error()
	}

	// 整体取消（Ctrl+C）优先：这是本机主动放弃，不是路径问题。
	if parent != nil && parent.Err() != nil && errors.Is(parent.Err(), context.Canceled) {
		return ErrorTypeCanceled, message
	}

	// 超时：可能是 context deadline，也可能是 exec 的 kill。
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrorTypeTimeout, message
		}
		return ErrorTypeCanceled, message
	}

	// 权限：traceroute 常常需要管理员权限。
	//
	// **必须同时匹配中英文字样**：NextTrace 在 Windows 上会用中文
	// 报出 WinDivert 权限问题（"依赖 WinDivert，但当前进程没有
	// 管理员权限"）。只匹配英文的话，中文环境下的用户会看到
	// "other" 这种毫无帮助的分类——而这恰恰是最常见的一种失败。
	lower := strings.ToLower(message + " " + err.Error())
	switch {
	case strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "access is denied"),
		strings.Contains(lower, "operation not permitted"),
		strings.Contains(lower, "requires root"),
		strings.Contains(lower, "administrator"),
		strings.Contains(lower, "windivert"),
		strings.Contains(message, "管理员权限"),
		strings.Contains(message, "权限不足"),
		strings.Contains(message, "拒绝访问"),
		strings.Contains(message, "需要以管理员"):
		return ErrorTypePermission, message
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, exec.ErrNotFound):
		return ErrorTypeNotFound, message
	}

	// 非零退出码。
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ErrorTypeExitCode, message
	}

	_ = path
	return ErrorTypeOther, message
}

// truncateBytes 截断过长的输出。
func truncateBytes(b []byte, limit int) string {
	s := string(b)
	if len(s) <= limit {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:limit]) + "..."
}

// itoa 是本文件内的短整数格式化。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

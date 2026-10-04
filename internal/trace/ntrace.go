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
	return checkAvailabilityIn(ctx, binaryPath, nil)
}

// checkAvailabilityIn 与 CheckAvailability 相同，但允许指定搜索目录。
//
// 单独一个内部函数是为了让测试能把自己的临时目录当成"同目录"，
// 而不必去动真实可执行文件所在的目录。
func checkAvailabilityIn(ctx context.Context, binaryPath string, searchDirs []string) Availability {
	path, err := resolveBinary(binaryPath, searchDirs)
	if err != nil {
		return Availability{Err: err}
	}

	// 真正执行一次 --version：只检查文件存在是不够的
	// （可能架构不匹配、可能没有执行权限、可能是坏文件）。
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, path, "--version")
	// 不弹控制台窗口：图形界面下每次跟踪都会调用引擎，
	// 一个黑框一闪而过会让人以为程序出了问题。
	hideSubprocessWindow(cmd)
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
func resolveBinary(binaryPath string, searchDirs []string) (string, error) {
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = DefaultBinary
	}

	// 未显式指定搜索目录时用默认顺序（同目录 -> data/bin -> cwd/data/bin）。
	if searchDirs == nil {
		searchDirs = SearchDirs()
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

	// 只有文件名：先在同目录与 data/bin 里找，最后才查 PATH。
	//
	// 顺序是刻意的：绿色版把 nexttrace 与本程序放在一起时，
	// 那个文件就是"使用者自己的那份"，比 PATH 里可能存在的
	// 另一个版本更该被选中。
	for _, dir := range searchDirs {
		if candidate, ok := lookInDir(dir, binaryPath); ok {
			return candidate, nil
		}
	}

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

	return "", fmt.Errorf("%w: %q not found in the program directory, data/bin, or PATH",
		fs.ErrNotExist, binaryPath)
}

// lookInDir 在指定目录里找一个可执行文件。
//
// 会补 `.exe`：使用者把文件命名为 nexttrace.exe 而配置写 "nexttrace"
// 是最常见的写法，不该因此找不到。
func lookInDir(dir, name string) (string, bool) {
	if strings.TrimSpace(dir) == "" {
		return "", false
	}

	candidate := filepath.Join(dir, name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		if absolute, absErr := filepath.Abs(candidate); absErr == nil {
			return absolute, true
		}
		return candidate, true
	}

	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		withExe := candidate + ".exe"
		if info, err := os.Stat(withExe); err == nil && !info.IsDir() {
			if absolute, absErr := filepath.Abs(withExe); absErr == nil {
				return absolute, true
			}
			return withExe, true
		}
	}

	return "", false
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

	// DataProvider 是 GeoIP 数据源（默认 NextTrace-API）。
	//
	// 影响每一跳的 ASN / 运营商 / 地理位置从哪儿来。
	// 它是本项目里**唯一**与 PoW 令牌绑定的选项：
	// 用 NextTrace-API 时令牌拿不到会整体失败。
	DataProvider DataProvider

	// PowProvider 是 NextTrace API v3 的 PoW 令牌来源（空表示不指定）。
	//
	// 中国大陆用户常用 sakura 避开默认源的限流。
	PowProvider PowProvider

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

	// DataProvider 是 GeoIP 数据源（空表示默认）。
	DataProvider DataProvider

	// PowProvider 是 PoW 令牌来源（空表示不显式指定）。
	PowProvider PowProvider

	// RunCommand 允许注入命令执行（测试用）。
	RunCommand func(ctx context.Context, path string, args []string) ([]byte, []byte, error)

	// SkipVersionCheck 为真时不查询版本（测试用）。
	SkipVersionCheck bool

	// AutoDownload 为真时，**完全没有找到** nexttrace 就自动下载一份。
	//
	// 默认关闭（零值），由生产路径显式打开：
	// 测试里构造一个不存在的引擎时**绝不能**偷偷联网下载
	// 32 MB 的东西——那会让单元测试依赖外网，还会拖慢 CI。
	AutoDownload bool

	// SearchDirs 覆盖查找目录（空表示用 SearchDirs()；测试用）。
	SearchDirs []string

	// DownloadVersion 是自动下载的版本（空表示 DefaultVersion）。
	DownloadVersion string

	// DownloadDir 是自动下载的落点（空表示 InstallDir()）。
	DownloadDir string

	// Logf 接收"正在下载"这类进度说明（可为 nil）。
	Logf func(format string, args ...any)
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

	// 数据源与 PoW 源在这里就校验。
	//
	// 这一步必须在构造时做：NextTrace 拿到不认识的数据源名时
	// 不报错，而是换一个源继续跑——那会让人以为在用 IPInfo，
	// 实际用的是别的，从结果上完全看不出来。
	dataProvider, err := opts.DataProvider.Normalize()
	if err != nil {
		return nil, err
	}
	powProvider, err := opts.PowProvider.Normalize()
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	engine := &NextTraceEngine{
		BinaryPath:   opts.BinaryPath,
		Timeout:      timeout,
		Mode:         mode,
		DataProvider: dataProvider,
		PowProvider:  powProvider,
		runCommand:   opts.RunCommand,
	}

	// 注入自定义执行器时（测试）跳过真实解析。
	if opts.RunCommand != nil {
		engine.resolvedPath = opts.BinaryPath
		engine.version = "test"
		return engine, nil
	}

	path, err := resolveBinary(opts.BinaryPath, opts.SearchDirs)
	if err != nil {
		// 找不到就**自动下载**（默认开启）。
		//
		// 只在"完全没找到"时下载，而且只在**没有显式指定路径**时：
		// 使用者明确说了用哪个文件，那就不该被一个自动下载顶掉——
		// 那会让人以为自己配的路径生效了，实际用的是别的。
		if !opts.AutoDownload || explicitPath(opts.BinaryPath) {
			return nil, fmt.Errorf("%w\n%s", err, NotFoundMessage)
		}

		downloaded, downloadErr := Download(ctx, DownloadOptions{
			Version: opts.DownloadVersion,
			Dir:     opts.DownloadDir,
			Logf:    opts.Logf,
		})
		if downloadErr != nil {
			// 下载失败时把两条信息都给出来：为什么没找到、
			// 以及为什么下载也没成功。只报一条会让人往复排查。
			return nil, fmt.Errorf("%w\n%s\n\ntrace: 自动下载也失败了: %v",
				err, NotFoundMessage, downloadErr)
		}
		path = downloaded
	}
	engine.resolvedPath = path

	if !opts.SkipVersionCheck {
		if avail := checkAvailabilityIn(ctx, path, opts.SearchDirs); avail.Found {
			engine.version = avail.Version
		}
		// 版本查不到不报错：有些构建的 --version 行为不同。
	}

	return engine, nil
}

// explicitPath 报告使用者是否**显式指定了文件路径**。
//
// 判据是"含路径分隔符"：写 "C:/Tools/nexttrace" 或 "./nt" 是明确
// 指向某个文件；写 "nexttrace" 只是给了一个名字，让程序去搜。
func explicitPath(binaryPath string) bool {
	return strings.ContainsAny(strings.TrimSpace(binaryPath), `/\`)
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

	// 数据源（以及可能需要的 PoW 源）。
	args = e.buildProviderArgs(args)

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

// buildProviderArgs 生成数据源相关的参数。
//
// 单独一个函数是为了让"哪些参数与数据源有关"一目了然，
// 也便于测试直接断言（不必构造整个 Target）。
//
// 显式传 `-d` 即使等于默认值：这样命令行日志本身就能证明
// 用的是哪个源。否则"没传"与"传了默认值"看起来一样，
// 排查时无法区分。
func (e *NextTraceEngine) buildProviderArgs(args []string) []string {
	provider, err := e.DataProvider.Normalize()
	if err != nil {
		// 理论上不可达（构造时已校验）；真发生了也不猜，
		// 让 nexttrace 用自己的默认值，并由构造期的错误暴露问题。
		return args
	}
	args = append(args, "--data-provider", string(provider))

	// PoW 源只在显式指定时传：默认值交给 nexttrace 自己决定，
	// 免得把上游的默认值固化进本项目。
	//
	// 也只在数据源真的会走 NextTrace API 时才传——
	// 用 IPInfo 等第三方源时这个参数没有意义。
	if !e.PowProvider.IsZero() && provider == ProviderNextTraceAPI {
		pow, powErr := e.PowProvider.Normalize()
		if powErr == nil && !pow.IsZero() {
			args = append(args, "--pow-provider", string(pow))
		}
	}

	return args
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

	// 同上：NextTrace 是控制台程序，在无控制台的宿主里启动它
	// 会为每个目标弹出（或留下）一个控制台窗口。
	hideSubprocessWindow(cmd)

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

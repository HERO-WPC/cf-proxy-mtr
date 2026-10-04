package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 本文件让引擎"自己把 nexttrace 准备好"。
//
// == 为什么需要 ==
//
// 线路跟踪依赖一个外部程序。此前"没装"就等于"跟踪不可用"，
// 而使用者看到的是一句"NextTrace not found"——他要自己去搜索、
// 挑对平台与架构、下对一个几十 MB 的文件、再放到对的位置。
// 这一步劝退的人比任何技术问题都多。
//
// == 搜索顺序（自定义路径优先，永远不被覆盖）==
//
//  1. 显式指定的路径（--binary / --trace-binary）：最高优先级，
//     而且**不会**被自动下载顶掉——使用者明确说了用哪个。
//  2. 与**本程序同目录**：`nexttrace[.exe]`。绿色版把两个文件
//     放一起就能用，这是最常见的用法。
//  3. 同目录下的 `data/bin/`：自动下载的落点，也是随包分发的位置。
//  4. 当前工作目录下的 `data/bin/`。
//  5. PATH。
//  6. 都没有 -> 自动下载（若启用），落到 `data/bin/`。
const (
	// DefaultVersion 是自动下载时取用的 nexttrace 版本。
	//
	// 固定版本而不是"latest"：本项目在测试里锁定过这个版本的
	// JSON 结构与参数行为（见 testdata/ 与 ntrace.go 的注释），
	// 跟随 latest 意味着某天上游改了输出格式，使用者的线路名
	// 会在毫无征兆的情况下变成空。想换版本请显式指定。
	DefaultVersion = "v1.7.3"

	// DefaultReleaseBase 是发布物下载地址前缀。
	DefaultReleaseBase = "https://github.com/nxtrace/NTrace-core/releases/download"

	// downloadTimeout 是整体下载超时。
	//
	// 实测 Windows amd64 的 nexttrace 约 32 MB，慢速网络下需要
	// 几分钟，因此给得比较宽松。
	downloadTimeout = 5 * time.Minute

	// maxDownloadBytes 限制下载体积（防止对端返回异常内容）。
	maxDownloadBytes = 128 << 20

	// minBinaryBytes 是"这看起来不像一个二进制"的下限。
	//
	// 用途：上游偶发返回 HTML 错误页（200 + 一小段 HTML）。
	// 32 MB 的程序不可能只有几十 KB，因此体积本身就能挡掉
	// 相当一部分坏下载；真正的判据仍是下面跑 --version。
	minBinaryBytes = 1 << 20
)

// DownloadOptions 是自动下载的参数。
type DownloadOptions struct {
	// Version 是要下载的版本标签（空表示 DefaultVersion）。
	Version string

	// ReleaseBase 是下载地址前缀（空表示 DefaultReleaseBase）。
	ReleaseBase string

	// Dir 是安装目录（空表示按 InstallDir 决定）。
	Dir string

	// GOOS / GOARCH 允许覆盖目标平台（空表示当前平台；测试用）。
	GOOS   string
	GOARCH string

	// HTTPClient 允许注入客户端（测试用）。
	HTTPClient *http.Client

	// Timeout 是整体超时（<=0 表示 downloadTimeout）。
	Timeout time.Duration

	// Logf 接收进度说明（可为 nil）。
	Logf func(format string, args ...any)
}

// AssetName 返回某个平台对应的发布物文件名。
//
// 上游的命名规则是 `nexttrace_<os>_<arch>[.exe]`（实测 v1.7.3
// 的 99 个资源都是这个形状），例如：
//
//	nexttrace_windows_amd64.exe
//	nexttrace_linux_arm64
//
// 只支持本项目实际发布的六个平台：写错平台名会得到一个 404，
// 而"提前报错说明不支持"比"下载失败"好排查。
func AssetName(goos, goarch string) (string, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	switch goos {
	case "windows", "linux", "darwin":
	default:
		return "", fmt.Errorf("asnprefix: 不支持自动下载 nexttrace 的平台 %q", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("trace: 不支持自动下载 nexttrace 的架构 %q", goarch)
	}

	name := "nexttrace_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}

// InstallDir 返回自动下载的落点。
//
// 优先用**本程序所在目录**下的 data/bin：绿色版双击运行时，
// 工作目录可能是 C:\Windows\System32 之类，写在那里使用者
// 永远找不到自己的东西。
func InstallDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, errEval := filepath.EvalSymlinks(exe); errEval == nil {
			exe = resolved
		}
		return filepath.Join(filepath.Dir(exe), "data", "bin")
	}
	return filepath.Join("data", "bin")
}

// SearchDirs 返回查找 nexttrace 的目录顺序。
//
// 与文档里的顺序一致：先同目录，再 data/bin。
func SearchDirs() []string {
	var dirs []string

	if exe, err := os.Executable(); err == nil {
		if resolved, errEval := filepath.EvalSymlinks(exe); errEval == nil {
			exe = resolved
		}
		base := filepath.Dir(exe)
		// 同目录优先：绿色版把两个文件放一起即可用。
		dirs = append(dirs, base, filepath.Join(base, "data", "bin"))
	}

	// 当前工作目录也看一眼（从终端在别处运行时常见）。
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(cwd, "data", "bin"))
	}

	return dirs
}

// Download 下载 nexttrace 到目标目录并做基本校验。
//
// 返回可执行文件的绝对路径。调用方应当把"下载"这件事明确告诉
// 使用者：几十 MB 的下载不该悄无声息地发生。
func Download(ctx context.Context, opts DownloadOptions) (string, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = DefaultVersion
	}
	base := strings.TrimSpace(opts.ReleaseBase)
	if base == "" {
		base = DefaultReleaseBase
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = InstallDir()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = downloadTimeout
	}

	asset, err := AssetName(opts.GOOS, opts.GOARCH)
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(base, "/") + "/" + version + "/" + asset
	opts.Logf("正在下载 nexttrace %s（%s）", version, asset)
	opts.Logf("来源：%s", url)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("trace: 创建安装目录 %s: %w", dir, err)
	}
	target := filepath.Join(dir, asset)

	downloadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cf-route-tester")

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("trace: 下载 nexttrace 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("trace: 下载 nexttrace 失败: HTTP %d（%s）", resp.StatusCode, url)
	}

	// 先写临时文件再改名：下载中断（断网、Ctrl+C）时不会留下
	// 一个半截的文件被下次启动当成"已经装好了"。
	temp, err := os.CreateTemp(dir, asset+".part*")
	if err != nil {
		return "", fmt.Errorf("trace: 创建临时文件: %w", err)
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }

	written, err := io.Copy(temp, io.LimitReader(resp.Body, maxDownloadBytes))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", fmt.Errorf("trace: 写入 nexttrace: %w", err)
	}

	if written < minBinaryBytes {
		cleanup()
		return "", fmt.Errorf("trace: 下载内容只有 %d 字节，不像一个可执行文件（上游可能返回了错误页）", written)
	}

	if err := os.Chmod(tempName, 0o755); err != nil && runtime.GOOS != "windows" {
		// Unix 上必须可执行；Windows 上权限位无意义。
		cleanup()
		return "", fmt.Errorf("trace: 设置可执行权限: %w", err)
	}

	if err := os.Rename(tempName, target); err != nil {
		cleanup()
		return "", fmt.Errorf("trace: 安装到 %s: %w", target, err)
	}

	opts.Logf("已下载到 %s（%.1f MB）", target, float64(written)/(1<<20))

	// 真正的校验：跑一次 --version。
	//
	// 体积检查只能挡住明显的坏下载；"下到了别的东西"或"下到了
	// 另一个平台的二进制"只有执行一次才知道。这一步失败就把
	// 文件删掉，免得留一个永远跑不起来的残骸让人困惑。
	if err := verifyInstalled(ctx, target); err != nil {
		_ = os.Remove(target)
		return "", fmt.Errorf("trace: 下载的 nexttrace 无法执行: %w", err)
	}
	opts.Logf("引擎已就绪：%s", target)

	absolute, err := filepath.Abs(target)
	if err != nil {
		return target, nil
	}
	return absolute, nil
}

// verifyInstalled 是"下载下来的东西真的能跑吗"的判据。
//
// 做成变量是为了给测试留一个接缝：要构造一个能在各平台都通过
// `--version` 的真实可执行文件并不现实，而"下载成功"这段恰恰最
// 需要被测——它决定了使用者拿到的是能用的引擎，还是一个残骸。
// 与 runCommand 的注入方式一致。
//
// 生产环境永远走 CheckAvailability：它真的执行一次 --version，
// 因此能同时发现"架构不匹配""没有执行权限""根本不是可执行文件"。
var verifyInstalled = func(ctx context.Context, path string) error {
	availability := CheckAvailability(ctx, path)
	if !availability.Found {
		return availability.Err
	}
	return nil
}

// ErrNotInstalled 表示"没有找到 nexttrace，且未启用自动下载"。
var ErrNotInstalled = errors.New("nexttrace is not installed")

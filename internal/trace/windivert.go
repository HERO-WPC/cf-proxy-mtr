package trace

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件负责 WinDivert 的两个文件。
//
// == 为什么需要 ==
//
// Windows 上 nexttrace 的 **TCP / UDP** 模式要靠 WinDivert 抓包：
// 需要 `WinDivert.dll`（用户态）与 `WinDivert64.sys`（内核驱动）
// 与 nexttrace 放在同一目录。ICMP 模式不需要它们。
//
// 上游 nexttrace 的发布物里**只有二进制本体**（实测 v1.7.3 的 99 个
// 资源全是可执行文件，没有任何压缩包），所以这两个文件必须从
// WinDivert 自己的发布里取。
//
// == 为什么要钉住哈希 ==
//
// 这是一个**内核驱动**：它会被以管理员权限加载。而 nexttrace 是
// 用户态程序，出问题最多是跟踪失败；驱动出问题的代价大得多。
// 上游不提供校验和（GitHub API 的 digest 字段为空），因此这里
// 记下我们核对过的 zip 哈希，下载后比对。
//
// 代理环境下这一点尤其重要：链路中间人换掉驱动是现实风险，
// 而 HTTPS 只能证明"来自 github.com"，不能证明"是未被改动的原件"。
const (
	// DefaultWindivertVersion 是自动下载的 WinDivert 版本。
	DefaultWindivertVersion = "v2.2.2"

	// DefaultWindivertBase 是下载地址前缀。
	DefaultWindivertBase = "https://github.com/basil00/WinDivert/releases/download"

	// windivertZipName 是发布物文件名。
	windivertZipName = "WinDivert-2.2.2-A.zip"

	// windivertZipSHA256 是 WinDivert-2.2.2-A.zip 的 SHA256。
	//
	// 实测值（下载后本地计算）。若上游将来重新上传同一个版本导致
	// 不一致，**不要**随手改这里：先自己核对新哈希的来源，
	// 或者用 --trace-no-download 关掉自动下载、手工放置文件。
	windivertZipSHA256 = "63cb41763bb4b20f600b6de04e991a9c2be73279e317d4d82f237b150c5f3f15"

	// windivertZipMaxBytes 限制压缩包体积。
	windivertZipMaxBytes = 8 << 20
)

// WindivertFiles 是 TCP/UDP 模式需要的文件名。
//
// 顺序即日志里的顺序；两个都必须与 nexttrace 同目录。
var WindivertFiles = []string{"WinDivert.dll", "WinDivert64.sys"}

// WindivertOptions 是下载 WinDivert 的参数。
type WindivertOptions struct {
	// Version / ReleaseBase / Dir / HTTPClient / Timeout 语义同 DownloadOptions。
	Version     string
	ReleaseBase string
	Dir         string
	HTTPClient  *http.Client
	Timeout     time.Duration

	// GOOS 允许覆盖目标平台（空表示当前平台；测试用）。
	GOOS string

	// SkipHashCheck 跳过钉住的哈希校验。
	//
	// 只在**使用了自定义镜像**时由调用方置位：镜像是使用者自己的
	// 选择，我们无从知道其内容。默认版本 + 默认地址一定校验。
	SkipHashCheck bool

	Logf func(format string, args ...any)
}

// MissingWindivert 返回目标目录里缺失的 WinDivert 文件。
//
// 返回空表示都在。
func MissingWindivert(dir string) []string {
	var missing []string
	for _, name := range WindivertFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// WindivertNeeded 报告某个跟踪模式是否需要 WinDivert。
//
// ICMP 不需要：它用系统自带的 ICMP 能力。把驱动下载给只用 ICMP
// 的人是不合适的——那是内核驱动，不该在不需要时装上。
func WindivertNeeded(mode Mode) bool {
	switch mode {
	case ModeTCP, ModeUDP:
		return true
	default:
		return false
	}
}

// DownloadWindivert 下载并解出 x64 的 WinDivert.dll 与 WinDivert64.sys。
//
// 返回实际安装的文件路径。
func DownloadWindivert(ctx context.Context, opts WindivertOptions) ([]string, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	goos := opts.GOOS
	if goos == "" {
		goos = "windows"
	}
	if goos != "windows" {
		// 不属于错误：调用方本就不该在别的平台调用它。
		return nil, nil
	}

	version := strings.TrimSpace(opts.Version)
	useDefault := version == ""
	if useDefault {
		version = DefaultWindivertVersion
	}
	base := strings.TrimSpace(opts.ReleaseBase)
	useDefaultBase := base == ""
	if useDefaultBase {
		base = DefaultWindivertBase
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = InstallDir()
	}

	url := strings.TrimRight(base, "/") + "/" + version + "/" + windivertZipName
	opts.Logf("TCP/UDP 模式需要 WinDivert，正在下载 %s", windivertZipName)
	opts.Logf("来源：%s", url)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("trace: 创建安装目录 %s: %w", dir, err)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cf-route-tester")

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trace: 下载 WinDivert 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trace: 下载 WinDivert 失败: HTTP %d（%s）", resp.StatusCode, url)
	}

	blob, err := io.ReadAll(io.LimitReader(resp.Body, windivertZipMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("trace: 读取 WinDivert: %w", err)
	}

	// 哈希校验：默认版本 + 默认地址才校验。
	//
	// 用了自定义镜像就无法知道内容应当是什么，那是使用者自己的选择；
	// 但默认路径必须校验，否则这个常量就形同虚设。
	if !opts.SkipHashCheck && useDefault && useDefaultBase {
		sum := sha256.Sum256(blob)
		got := hex.EncodeToString(sum[:])
		if got != windivertZipSHA256 {
			return nil, fmt.Errorf(
				"trace: WinDivert 压缩包哈希不符，已放弃\n"+
					"  期望 %s\n  实际 %s\n"+
					"这可能意味着下载被中间人替换。请手工核对来源，"+
					"或用 --trace-no-download 关掉自动下载后自行放置文件",
				windivertZipSHA256, got)
		}
		opts.Logf("压缩包哈希校验通过")
	}

	reader, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil {
		return nil, fmt.Errorf("trace: WinDivert 压缩包无法解析: %w", err)
	}

	var installed []string
	for _, name := range WindivertFiles {
		data, err := readZipEntry(reader, "x64/"+name)
		if err != nil {
			return installed, fmt.Errorf("trace: 压缩包里没有 x64/%s: %w", name, err)
		}

		target := filepath.Join(dir, name)
		// 先写临时文件再改名：中断时不会留下半截的驱动，
		// 那比没有更危险（可能被加载）。
		temp, err := os.CreateTemp(dir, name+".part*")
		if err != nil {
			return installed, fmt.Errorf("trace: 创建临时文件: %w", err)
		}
		tempName := temp.Name()

		if _, err := temp.Write(data); err != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
			return installed, fmt.Errorf("trace: 写入 %s: %w", name, err)
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempName)
			return installed, fmt.Errorf("trace: 写入 %s: %w", name, err)
		}
		if err := os.Rename(tempName, target); err != nil {
			_ = os.Remove(tempName)
			return installed, fmt.Errorf("trace: 安装到 %s: %w", target, err)
		}

		opts.Logf("已安装 %s（%.1f KB）", target, float64(len(data))/1024)
		installed = append(installed, target)
	}

	return installed, nil
}

// readZipEntry 取出以 suffix 结尾的条目内容。
//
// 用"以 /x64/xxx 结尾"而不是写死完整路径：压缩包里的顶层目录名
// 带着版本号（WinDivert-2.2.2-A/），写死会让换版本就找不到文件。
func readZipEntry(reader *zip.Reader, suffix string) ([]byte, error) {
	suffix = strings.ReplaceAll(suffix, `\`, `/`)
	for _, file := range reader.File {
		name := strings.ReplaceAll(file.Name, `\`, `/`)
		if !strings.HasSuffix(name, "/"+suffix) && name != suffix {
			continue
		}
		handle, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(handle, windivertZipMaxBytes))
		_ = handle.Close()
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, os.ErrNotExist
}

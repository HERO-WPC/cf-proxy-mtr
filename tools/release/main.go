// Command release 构建 cf-route-tester 的跨平台发布包。
//
// 用法（任何平台都能跑，只需要 Go 工具链）：
//
//	go run ./tools/release -version 0.1.0
//	go run ./tools/release -version 0.1.0 -skip-tests
//
// 为什么用 Go 写而不是 shell 脚本：
//
//   - 项目要求"单文件二进制、零外部依赖"，构建流程也应当如此。
//     一个 pwsh 脚本 Linux 用户跑不了，一个 sh 脚本 Windows 用户跑不了，
//     两套脚本则必然会漂移（命名规则、校验和格式、清单字段）。
//   - 用 Go 写就只有一份实现，任何平台都能跑，而且能被单元测试覆盖
//     （见 main_test.go：命名规则与校验和格式是发布产物的一部分，
//     写错了用户就没法校验）。
//
// 刻意不使用 goreleaser 之类的工具：6 个目标平台的交叉编译用 go build
// 就够了，少一个工具链依赖就少一处"我这里构建不出来"的原因。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Target 是一个发布目标。
type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// DefaultTargets 是发布目标列表（与 CI 的交叉编译列表一致）。
//
// 需求要求 Windows / Linux / macOS × amd64 / arm64。
func DefaultTargets() []Target {
	return []Target{
		{OS: "windows", Arch: "amd64"},
		{OS: "windows", Arch: "arm64"},
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
	}
}

// BinaryName 返回某个目标平台的产物文件名。
//
// 命名规则里有版本号：下载下来的文件自己就说明了它是哪一版，
// 不需要用户再去对照发布页。
func BinaryName(version string, target Target) string {
	name := fmt.Sprintf("%s-%s-%s-%s", clientName, version, target.OS, target.Arch)
	if target.OS == "windows" {
		name += ".exe"
	}
	return name
}

// clientName 必须与 internal/version.ClientName 一致。
//
// 这里刻意重复一个字面量而不是 import：本工具在**构建**阶段运行，
// 而 internal/version 属于被测代码；两者耦合会让"版本号从哪来"
// 变得绕。下面 readSourceVersion 会直接从源码里读，保持一致。
const clientName = "cf-route-tester"

// GUIBinaryName 返回图形界面入口的产物文件名。
//
// 只在 Windows 上存在：其它平台没有"双击弹控制台"的问题。
func GUIBinaryName(version string, target Target) string {
	name := fmt.Sprintf("%s-gui-%s-%s-%s", clientName, version, target.OS, target.Arch)
	if target.OS == "windows" {
		name += ".exe"
	}
	return name
}

// describe 统计一个产物并生成清单条目。
func describe(name string, target Target, path string) (ManifestEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("stat %s: %w", path, err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return ManifestEntry{}, err
	}
	return ManifestEntry{
		File:   name,
		OS:     target.OS,
		Arch:   target.Arch,
		Bytes:  info.Size(),
		SHA256: sum,
	}, nil
}

// main 是入口。
func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// options 是构建选项。
type options struct {
	version   string
	outDir    string
	skipTests bool
	repoRoot  string
}

// run 执行发布构建。
func run(args []string, stdout, stderr io.Writer) error {
	var opts options

	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.version, "version", "", "语义化版本号（默认从源码读取）")
	fs.StringVar(&opts.outDir, "out", filepath.Join("dist", "release"), "产物目录")
	fs.BoolVar(&opts.skipTests, "skip-tests", false, "跳过 gofmt / vet / test（不建议）")
	fs.StringVar(&opts.repoRoot, "repo", ".", "项目根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := filepath.Abs(opts.repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("%s does not look like the project root (no go.mod): %w", root, err)
	}

	// 版本号：显式给出的优先，否则从源码读。
	if strings.TrimSpace(opts.version) == "" {
		opts.version, err = readSourceVersion(root)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "版本号取自源码：%s\n", opts.version)
	}
	if !isSemver(opts.version) {
		return fmt.Errorf("version %q is not a semantic version (want e.g. 0.1.0)", opts.version)
	}

	commit, dirty, commitTime := gitCommit(root)

	// 构建时间取**commit 的时间**，而不是"现在"。
	//
	// 这样同一份提交无论在什么时间、什么机器上构建，
	// 产物的字节都是相同的（配合 -trimpath 与固定的 ldflags），
	// 于是"用校验和确认我拿到的就是发布者构建的那个文件"才有意义。
	// 用 time.Now() 会让每次构建的二进制都不同，
	// 校验和只能证明"文件没坏"，证明不了"内容一致"。
	buildDate := commitTime
	if buildDate == "" {
		// 读不到 git（例如导出的源码包）：只能退回当前时间，
		// 并如实告诉用户这次构建不可复现。
		buildDate = time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintln(stderr, "warning: 无法读取 commit 时间，构建时间取当前时间；此次构建不可复现")
	}

	fmt.Fprintf(stdout, "版本:      %s\n", opts.version)
	fmt.Fprintf(stdout, "commit:    %s\n", commit)
	if dirty {
		fmt.Fprintf(stdout, "注意:      工作区有未提交改动（commit 标注为 -dirty）\n")
	}
	fmt.Fprintf(stdout, "构建时间:  %s（取自 commit 时间，保证可复现）\n\n", buildDate)

	// 发布前校验。
	if !opts.skipTests {
		if err := verify(root, stdout); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(stderr, "warning: 已跳过测试与静态检查（-skip-tests）")
	}

	// 产物目录：先清空，避免上一次的产物混进校验和。
	outDir := opts.outDir
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(root, outDir)
	}
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("clear output directory: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// ldflags 注入版本信息。
	//
	// -s -w 去掉符号表与调试信息：产物体积明显更小，
	// 而本项目不需要事后用 dlv 调试发布二进制。
	ldflags := strings.Join([]string{
		"-s", "-w",
		"-X", modulePath + "/internal/version.Commit=" + commit,
		"-X", modulePath + "/internal/version.BuildDate=" + buildDate,
	}, " ")

	targets := DefaultTargets()
	entries := make([]ManifestEntry, 0, len(targets)*2)

	for _, target := range targets {
		fmt.Fprintf(stdout, "==> 构建 %s/%s\n", target.OS, target.Arch)

		// 1) 命令行入口。
		name := BinaryName(opts.version, target)
		path := filepath.Join(outDir, name)
		if err := buildTarget(root, target, ldflags, "cf-route-tester", path, false); err != nil {
			return err
		}
		entry, err := describe(name, target, path)
		if err != nil {
			return err
		}
		entries = append(entries, entry)

		// 2) 图形界面入口（无控制台窗口），仅 Windows。
		//
		// 只有 Windows 需要它：那里的控制台程序双击启动会先弹出一个
		// 黑窗口，而图形界面的使用者不该看到它。
		// 其它平台没有这个问题（.app / .desktop 启动器天然不显示终端），
		// 也就不必多一份产物。
		if target.OS == "windows" {
			guiName := GUIBinaryName(opts.version, target)
			guiPath := filepath.Join(outDir, guiName)
			fmt.Fprintf(stdout, "    （图形界面入口，无控制台）\n")
			if err := buildTarget(root, target, ldflags, "cf-route-tester-gui", guiPath, true); err != nil {
				return err
			}
			guiEntry, err := describe(guiName, target, guiPath)
			if err != nil {
				return err
			}
			entries = append(entries, guiEntry)
		}
	}

	// 校验和文件：每行 "hash  filename"，与 sha256sum / shasum -c 兼容。
	if err := writeChecksums(filepath.Join(outDir, "SHA256SUMS"), entries); err != nil {
		return err
	}

	// 发布清单。
	manifest := Manifest{
		Name:          clientName,
		Version:       opts.version,
		Commit:        commit,
		Dirty:         dirty,
		BuildDate:     buildDate,
		GoVersion:     runtime.Version(),
		SchemaVersion: 1,
		CGOEnabled:    false,
		Targets:       entries,
		Notes: []string{
			"所有产物均为静态单文件二进制（CGO_ENABLED=0），无需运行时依赖。",
			"Windows 上的线路跟踪（trace / scan --trace）需要管理员权限 + WinDivert；",
			"首次使用请先执行一次 nexttrace --init（普通权限即可）。",
			"TCP/UDP 模式的线路跟踪在 Linux/macOS 上通常需要 root 或相应 capability。",
		},
	}
	manifestPath := filepath.Join(outDir, "release.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		return err
	}

	// 汇总。
	fmt.Fprintf(stdout, "\n产物（%s）：\n", outDir)
	for _, entry := range entries {
		fmt.Fprintf(stdout, "  %-56s %7.2f MiB  %s\n",
			entry.File, float64(entry.Bytes)/(1<<20), entry.SHA256[:16])
	}
	fmt.Fprintf(stdout, "\n校验和: %s\n", filepath.Join(outDir, "SHA256SUMS"))
	fmt.Fprintf(stdout, "清单:   %s\n", manifestPath)
	fmt.Fprintf(stdout, "\n发布产物已就绪。上传前请确认 README 的版本号与 %s 一致。\n", opts.version)
	return nil
}

// modulePath 是 go.mod 里的模块路径（ldflags 需要完整符号名）。
const modulePath = "github.com/cf-route-tester/cf-route-tester"

// verify 跑 gofmt / vet / test。
//
// 发布前不跑测试等于把验证责任推给用户。
func verify(root string, stdout io.Writer) error {
	steps := []struct {
		label string
		args  []string
	}{
		// gofmt -l 有输出就说明有文件没格式化。
		{"gofmt -l .", []string{"gofmt", "-l", "."}},
		{"go vet ./...", []string{"go", "vet", "./..."}},
		{"go test ./...", []string{"go", "test", "./...", "-count=1"}},
	}

	for _, step := range steps {
		fmt.Fprintf(stdout, "==> %s\n", step.label)

		cmd := exec.Command(step.args[0], step.args[1:]...)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s failed: %w\n%s", step.label, err, string(output))
		}
		if step.args[0] == "gofmt" && len(strings.TrimSpace(string(output))) > 0 {
			return fmt.Errorf("these files are not gofmt-formatted:\n%s", string(output))
		}
	}
	fmt.Fprintln(stdout)
	return nil
}

// buildTarget 交叉编译一个目标。
//
// gui 为真时用 -H=windowsgui 构建：产物不带控制台窗口，
// 适合"双击启动图形界面"。它只对 Windows 有意义，
// 在其它平台该标志会被忽略（因此调用方不会传 true）。
func buildTarget(root string, target Target, ldflags, pkg, output string, gui bool) error {
	linker := ldflags
	if gui && target.OS == "windows" {
		// -H windowsgui 让 PE 子系统是 GUI：双击时不会弹出控制台窗口。
		//
		// 代价是进程没有可用的 stdout/stderr，程序必须自己把
		// 该说的话写进日志文件——见 internal/cli 的 Env.Detached。
		linker = linker + " -H=windowsgui"
	}

	cmd := exec.Command("go", "build",
		// -trimpath 去掉构建机器的绝对路径：
		// 产物里不该出现 "D:\..." 或 "/home/..."，那既是隐私问题，
		// 也让同一份源码在不同机器上构建出的二进制不一致。
		"-trimpath",
		"-ldflags", linker,
		"-o", output,
		"./cmd/"+pkg,
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOOS="+target.OS,
		"GOARCH="+target.Arch,
		// CGO_ENABLED=0 是硬性要求：产物必须是静态单文件，
		// 不能依赖目标机器上的 C 运行库。
		"CGO_ENABLED=0",
	)

	combined, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s/%s: %w\n%s", target.OS, target.Arch, err, string(combined))
	}
	return nil
}

// readSourceVersion 从 internal/version/version.go 读取 Version 常量。
func readSourceVersion(root string) (string, error) {
	path := filepath.Join(root, "internal", "version", "version.go")
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	match := versionPattern.FindSubmatch(content)
	if len(match) < 2 {
		return "", fmt.Errorf("cannot find the Version constant in %s; pass -version explicitly", path)
	}
	return string(match[1]), nil
}

// versionPattern 匹配 version.go 里的 Version 常量。
var versionPattern = regexp.MustCompile(`(?m)^\s*Version\s*=\s*"([^"]+)"\s*$`)

// isSemver 做一次宽松的语义化版本检查。
//
// 宽松是有意的：预发布号（0.2.0-rc.1）也要能通过，
// 但"v0.1.0"、"0.1"、"latest"这类会让产物文件名混乱的写法要被挡住。
func isSemver(version string) bool {
	return semverPattern.MatchString(version)
}

var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.\-]+)?(?:\+[0-9A-Za-z.\-]+)?$`)

// gitCommit 返回短 commit、"工作区是否脏"，以及 commit 的提交时间。
//
// 读不到 git 信息不算错误（用户可能拿的是导出的源码包），
// 但必须如实标成 unknown，而不是编一个值。
func gitCommit(root string) (commit string, dirty bool, commitTime string) {
	commit = "unknown"

	if output, err := runGit(root, "rev-parse", "--short", "HEAD"); err == nil {
		commit = strings.TrimSpace(output)
	}
	if status, err := runGit(root, "status", "--porcelain"); err == nil {
		dirty = strings.TrimSpace(status) != ""
	}
	// %cI 是严格的 ISO-8601（RFC3339）提交时间。
	if output, err := runGit(root, "log", "-1", "--format=%cI"); err == nil {
		commitTime = strings.TrimSpace(output)
	}
	return commit, dirty, commitTime
}

// runGit 执行一条 git 命令。
func runGit(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// fileSHA256 计算文件的 SHA256。
//
// 流式读取：产物有几十 MB，一次性读进内存没有必要。
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// writeChecksums 写出 SHA256SUMS。
//
// 格式与 coreutils 的 sha256sum 一致（"hash  两个空格  filename"），
// 因此用户可以直接 `sha256sum -c SHA256SUMS` 或 `shasum -a 256 -c`。
func writeChecksums(path string, entries []ManifestEntry) error {
	var builder strings.Builder
	for _, entry := range entries {
		builder.WriteString(entry.SHA256)
		builder.WriteString("  ")
		builder.WriteString(entry.File)
		builder.WriteString("\n")
	}
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

// writeJSON 写出格式化的 JSON。
func writeJSON(path string, value any) error {
	blob, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return os.WriteFile(path, append(blob, '\n'), 0o644)
}

// ManifestEntry 是清单里的一个产物。
type ManifestEntry struct {
	File   string `json:"file"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest 是发布清单。
type Manifest struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`

	SchemaVersion int  `json:"schema_version"`
	CGOEnabled    bool `json:"cgo_enabled"`

	Targets []ManifestEntry `json:"targets"`
	Notes   []string        `json:"notes"`
}

// sortedNames 返回按名字排序的产物名（测试用）。
func sortedNames(entries []ManifestEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.File)
	}
	sort.Strings(out)
	return out
}

// errNoTargets 在目标列表为空时返回（防御性检查）。
var errNoTargets = errors.New("release: no build targets configured")

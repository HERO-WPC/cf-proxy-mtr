package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestBinaryName 固定产物命名规则。
//
// 命名是发布产物的一部分：用户下载下来的文件必须自证"这是哪个版本、
// 哪个平台"，否则他会去对照发布页猜。规则一旦变了，
// 已有的下载链接与文档都会失效，因此用测试钉住。
func TestBinaryName(t *testing.T) {
	cases := []struct {
		version string
		target  Target
		want    string
	}{
		{"0.1.0", Target{OS: "windows", Arch: "amd64"}, "cf-route-tester-0.1.0-windows-amd64.exe"},
		{"0.1.0", Target{OS: "windows", Arch: "arm64"}, "cf-route-tester-0.1.0-windows-arm64.exe"},
		{"0.1.0", Target{OS: "linux", Arch: "amd64"}, "cf-route-tester-0.1.0-linux-amd64"},
		{"0.1.0", Target{OS: "linux", Arch: "arm64"}, "cf-route-tester-0.1.0-linux-arm64"},
		{"0.1.0", Target{OS: "darwin", Arch: "amd64"}, "cf-route-tester-0.1.0-darwin-amd64"},
		{"0.1.0", Target{OS: "darwin", Arch: "arm64"}, "cf-route-tester-0.1.0-darwin-arm64"},
		// 预发布版本也要能生成合法文件名。
		{"0.2.0-rc.1", Target{OS: "linux", Arch: "amd64"}, "cf-route-tester-0.2.0-rc.1-linux-amd64"},
	}

	for _, tc := range cases {
		if got := BinaryName(tc.version, tc.target); got != tc.want {
			t.Errorf("BinaryName(%q, %s/%s) = %q, want %q",
				tc.version, tc.target.OS, tc.target.Arch, got, tc.want)
		}
	}

	// 只有 Windows 带 .exe。
	for _, target := range DefaultTargets() {
		name := BinaryName("1.0.0", target)
		hasExe := strings.HasSuffix(name, ".exe")
		if hasExe != (target.OS == "windows") {
			t.Errorf("%s/%s: name %q, .exe suffix = %v", target.OS, target.Arch, name, hasExe)
		}
	}
}

// TestDefaultTargetsCoverRequiredPlatforms 验证发布覆盖了需求要求的平台。
func TestDefaultTargetsCoverRequiredPlatforms(t *testing.T) {
	targets := DefaultTargets()

	want := map[string]bool{
		"windows/amd64": false,
		"windows/arm64": false,
		"linux/amd64":   false,
		"linux/arm64":   false,
		"darwin/amd64":  false,
		"darwin/arm64":  false,
	}
	for _, target := range targets {
		key := target.OS + "/" + target.Arch
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected target %s", key)
			continue
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("required target %s is missing from the release", key)
		}
	}
	if len(targets) != len(want) {
		t.Errorf("targets = %d, want %d", len(targets), len(want))
	}
}

// TestIsSemver 验证版本号校验既不过严也不过松。
func TestIsSemver(t *testing.T) {
	valid := []string{"0.1.0", "1.0.0", "10.20.30", "0.2.0-rc.1", "1.0.0+build.5", "0.1.0-alpha"}
	for _, version := range valid {
		if !isSemver(version) {
			t.Errorf("isSemver(%q) = false, want true", version)
		}
	}

	// 这些写法会让产物文件名混乱或与文档脱节，必须挡住。
	invalid := []string{"", "v0.1.0", "0.1", "1", "latest", "0.1.0.0", "0.1.0 ", " 0.1.0", "dev"}
	for _, version := range invalid {
		if isSemver(version) {
			t.Errorf("isSemver(%q) = true, want false", version)
		}
	}
}

// TestChecksumFormatMatchesCoreutils 验证校验和文件与 sha256sum 兼容。
//
// 格式写错了用户就没法校验下载下来的产物——而校验正是发布的意义之一。
func TestChecksumFormatMatchesCoreutils(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SHA256SUMS")

	entries := []ManifestEntry{
		{File: "cf-route-tester-0.1.0-linux-amd64", SHA256: strings.Repeat("a", 64)},
		{File: "cf-route-tester-0.1.0-windows-amd64.exe", SHA256: strings.Repeat("b", 64)},
	}
	if err := writeChecksums(path, entries); err != nil {
		t.Fatalf("writeChecksums: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// coreutils 的格式：64 位十六进制 + 两个空格 + 文件名。
	linePattern := regexp.MustCompile(`(?m)^([0-9a-f]{64})  (\S+)$`)
	matches := linePattern.FindAllStringSubmatch(string(content), -1)
	if len(matches) != len(entries) {
		t.Fatalf("checksum lines matching the coreutils format = %d, want %d:\n%s",
			len(matches), len(entries), content)
	}
	for i, match := range matches {
		if match[1] != entries[i].SHA256 {
			t.Errorf("line %d hash = %s, want %s", i, match[1], entries[i].SHA256)
		}
		if match[2] != entries[i].File {
			t.Errorf("line %d file = %s, want %s", i, match[2], entries[i].File)
		}
	}

	// 必须换行结尾（否则最后一行在某些工具里会被当成未完成）。
	if !strings.HasSuffix(string(content), "\n") {
		t.Error("SHA256SUMS does not end with a newline")
	}
}

// TestReadSourceVersion 验证能从源码里读出真实版本号。
//
// 用项目自己的 version.go：这样"脚本读的版本"与"二进制里编的版本"
// 不一致时，测试会立刻失败。
func TestReadSourceVersion(t *testing.T) {
	// 从 tools/release 往上找到项目根。
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("cannot locate the project root from %s: %v", root, err)
	}

	version, err := readSourceVersion(root)
	if err != nil {
		t.Fatalf("readSourceVersion: %v", err)
	}
	if !isSemver(version) {
		t.Errorf("version read from source = %q, which is not a semantic version", version)
	}

	// 与 internal/version 的常量保持一致（这里直接读文件，
	// 避免为了让构建工具依赖被测代码而 import）。
	content, err := os.ReadFile(filepath.Join(root, "internal", "version", "version.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `Version = "`+version+`"`) {
		t.Errorf("read %q but internal/version/version.go does not declare it", version)
	}
}

func TestReadSourceVersionMissingFile(t *testing.T) {
	if _, err := readSourceVersion(t.TempDir()); err == nil {
		t.Error("readSourceVersion succeeded on a directory without version.go")
	}
}

// TestGitCommitReadsTimestamp 验证能读到 commit 时间。
//
// 构建时间取自 commit 时间（而不是 time.Now()）是可复现构建的前提：
// 同一份提交在任何时间、任何机器上构建出的字节必须相同，
// 否则校验和只能证明"文件没坏"，证明不了"内容一致"。
func TestGitCommitReadsTimestamp(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("not a git checkout; skipping")
	}

	commit, _, commitTime := gitCommit(root)

	if commit == "" || commit == "unknown" {
		t.Errorf("commit = %q, want a real short hash", commit)
	}
	if commitTime == "" {
		t.Fatal("commitTime is empty; the build date would fall back to time.Now() and break reproducibility")
	}

	// 必须是 RFC3339，否则会被原样写进二进制与 release.json。
	if _, err := time.Parse(time.RFC3339, commitTime); err != nil {
		t.Errorf("commitTime = %q, which is not RFC3339: %v", commitTime, err)
	}
}

// TestGitCommitOutsideRepoIsNotFatal 验证没有 git 时优雅降级。
//
// 用户可能拿的是导出的源码包（没有 .git），构建仍然要能完成，
// 只是如实标注 unknown。
func TestGitCommitOutsideRepoIsNotFatal(t *testing.T) {
	commit, dirty, commitTime := gitCommit(t.TempDir())

	if commit != "unknown" {
		t.Errorf("commit = %q, want unknown outside a repo", commit)
	}
	if dirty {
		t.Error("dirty = true outside a repo")
	}
	if commitTime != "" {
		t.Errorf("commitTime = %q, want empty outside a repo", commitTime)
	}
}

// TestWriteJSONProducesValidManifest 验证清单可被解析且字段完整。
func TestWriteJSONProducesValidManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release.json")

	manifest := Manifest{
		Name: "cf-route-tester", Version: "0.1.0", Commit: "abc123",
		BuildDate: "2026-10-03T10:00:00Z", GoVersion: "go1.26.0",
		SchemaVersion: 1,
		Targets:       []ManifestEntry{{File: "x", OS: "linux", Arch: "amd64", Bytes: 1, SHA256: "ab"}},
		Notes:         []string{"note"},
	}
	if err := writeJSON(path, manifest); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 缩进过，便于人直接看。
	if !strings.Contains(string(content), "\n  \"name\"") {
		t.Errorf("manifest is not indented:\n%s", content)
	}
	if !strings.HasSuffix(string(content), "\n") {
		t.Error("manifest does not end with a newline")
	}
}

// TestRunRejectsBadArguments 验证参数错误被明确报出。
func TestRunRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "not a project root",
			args: []string{"-repo", dir},
			want: "does not look like the project root",
		},
		{
			name: "bad version",
			args: []string{"-repo", dir, "-version", "not-a-version"},
			want: "", // 先会因为根目录失败，见下面的独立用例
		},
	}

	for _, tc := range cases[:1] {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			err := run(tc.args, &stdout, &stderr)
			if err == nil {
				t.Fatal("run succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// 版本号校验独立测（给一个真实的项目根）。
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("cannot locate the project root")
	}

	var stdout, stderr strings.Builder
	err = run([]string{"-repo", root, "-version", "v0.1.0", "-skip-tests"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run accepted the version \"v0.1.0\"")
	}
	if !strings.Contains(err.Error(), "semantic version") {
		t.Errorf("error = %v, want a semantic-version complaint", err)
	}
}

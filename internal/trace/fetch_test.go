package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 本文件覆盖"引擎自己把 nexttrace 准备好"。
//
// 为什么值得测：下载这条路走通与否，决定了使用者是"开箱能用"
// 还是"先去搜索、挑平台、下对文件、放对位置"。而它同时牵涉
// 网络、文件系统与执行权限，是最容易只在我这台机器上碰巧成功的一段。

// ---------------------------------------------------------------------------
// 发布物命名
// ---------------------------------------------------------------------------

// TestAssetNameMatchesUpstream 验证资源名与上游一致。
//
// 上游 v1.7.3 的 99 个资源都是 `nexttrace_<os>_<arch>[.exe]` 这个形状，
// 拼错一个字母就是一个 404，而 404 的表现是"下载失败"，
// 看不出到底是名字错了还是网络问题。
func TestAssetNameMatchesUpstream(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"windows", "amd64", "nexttrace_windows_amd64.exe"},
		{"windows", "arm64", "nexttrace_windows_arm64.exe"},
		{"linux", "amd64", "nexttrace_linux_amd64"},
		{"linux", "arm64", "nexttrace_linux_arm64"},
		{"darwin", "amd64", "nexttrace_darwin_amd64"},
		{"darwin", "arm64", "nexttrace_darwin_arm64"},
	}

	for _, tc := range cases {
		got, err := AssetName(tc.goos, tc.goarch)
		if err != nil {
			t.Errorf("AssetName(%s, %s) returned error: %v", tc.goos, tc.goarch, err)
			continue
		}
		if got != tc.want {
			t.Errorf("AssetName(%s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestAssetNameRejectsUnsupported 验证不支持的平台被明确拒绝。
//
// 本项目只发布六个平台。与其下到一个跑不起来的二进制，
// 不如在动手之前就说清楚"这个平台不支持"。
func TestAssetNameRejectsUnsupported(t *testing.T) {
	for _, tc := range [][2]string{
		{"freebsd", "amd64"},
		{"linux", "386"},
		{"plan9", "amd64"},
		{"windows", "mips"},
	} {
		if got, err := AssetName(tc[0], tc[1]); err == nil {
			t.Errorf("AssetName(%s, %s) = %q, want an error", tc[0], tc[1], got)
		}
	}
}

// TestAssetNameDefaultsToCurrentPlatform 验证空参数用当前平台。
func TestAssetNameDefaultsToCurrentPlatform(t *testing.T) {
	got, err := AssetName("", "")
	if err != nil {
		t.Fatalf("AssetName(\"\", \"\") returned error: %v", err)
	}
	if !strings.Contains(got, runtime.GOOS) || !strings.Contains(got, runtime.GOARCH) {
		t.Errorf("AssetName(\"\", \"\") = %q, want it to reflect %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
}

// ---------------------------------------------------------------------------
// 查找顺序
// ---------------------------------------------------------------------------

// TestResolveBinaryPrefersProgramDirectory 验证同目录优先于 PATH。
//
// 这是使用者最容易预期的行为：把 nexttrace 与本程序放一起就能用。
// 而 PATH 里可能同时存在另一个版本，那时"同目录那份"才是他想要的那份。
func TestResolveBinaryPrefersProgramDirectory(t *testing.T) {
	dir := t.TempDir()
	name := "nexttrace"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	beside := filepath.Join(dir, name)
	if err := os.WriteFile(beside, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveBinary("nexttrace", []string{dir})
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(beside) {
		t.Errorf("resolveBinary = %q, want the copy next to the program (%q)", got, beside)
	}
}

// TestResolveBinarySearchesDataBin 验证 data/bin 也在查找范围里。
func TestResolveBinarySearchesDataBin(t *testing.T) {
	exeDir := t.TempDir()
	binDir := filepath.Join(exeDir, "data", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	name := "nexttrace"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(binDir, name)
	if err := os.WriteFile(target, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 同目录没有 -> 应当落到 data/bin。
	got, err := resolveBinary("nexttrace", []string{exeDir, binDir})
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(target) {
		t.Errorf("resolveBinary = %q, want %q", got, target)
	}
}

// TestResolveBinaryStillFallsBackToPath 验证最后的 PATH 兜底没有丢。
func TestResolveBinaryStillFallsBackToPath(t *testing.T) {
	// 用一个几乎必然在 PATH 里的可执行文件名。
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}

	dir := t.TempDir() // 空目录：同目录与 data/bin 都没有
	got, err := resolveBinary(name, []string{dir})
	if err != nil {
		t.Skipf("PATH 里没有 %s，无法验证兜底: %v", name, err)
	}
	if !strings.Contains(strings.ToLower(got), strings.ToLower(name)) {
		t.Errorf("resolveBinary = %q, want a PATH hit for %s", got, name)
	}
}

// TestResolveBinaryErrorNamesAllSearchedPlaces 验证找不到时的错误
// 说清了找过哪些地方。
//
// "not found in PATH" 会让人以为只查了 PATH，于是把文件放进同目录
// 之后仍然困惑；把三个位置都列出来才能直接照做。
func TestResolveBinaryErrorNamesAllSearchedPlaces(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveBinary("definitely-not-installed-xyz", []string{dir})
	if err == nil {
		t.Fatal("resolveBinary found something that does not exist")
	}

	for _, want := range []string{"program directory", "data/bin", "PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestExplicitPathDetection 验证"显式路径"的判定。
//
// 这个判定决定自动下载会不会覆盖使用者指定的文件：
// 判错了会让"我配了路径"的人实际用的是别的东西。
func TestExplicitPathDetection(t *testing.T) {
	cases := map[string]bool{
		"":                         false,
		"nexttrace":                false,
		"nexttrace.exe":            false,
		"./nexttrace":              true,
		"../bin/nexttrace":         true,
		"C:/Tools/nexttrace.exe":   true,
		`C:\Tools\nexttrace.exe`:   true,
		"/usr/local/bin/nexttrace": true,
	}

	for input, want := range cases {
		if got := explicitPath(input); got != want {
			t.Errorf("explicitPath(%q) = %v, want %v", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 下载
// ---------------------------------------------------------------------------

// fakeReleaseServer 提供一个假的发布物服务。
//
// 返回的 body 可控，用来覆盖"下到了 HTML 错误页""下载中途失败"这些
// 真实会发生、但用真上游很难稳定复现的情况。
func fakeReleaseServer(t *testing.T, status int, body []byte) (*httptest.Server, *int) {
	t.Helper()

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// withFakeVerify 临时替换"下载后能否执行"的判据。
//
// 要构造一个在 Windows/Linux/macOS 上都能通过 --version 的真实
// 可执行文件并不现实，而"下载成功"这条路径恰恰最需要被测。
func withFakeVerify(t *testing.T, err error) {
	t.Helper()

	original := verifyInstalled
	verifyInstalled = func(context.Context, string) error { return err }
	t.Cleanup(func() { verifyInstalled = original })
}

// TestDownloadInstallsBinary 验证下载成功路径。
func TestDownloadInstallsBinary(t *testing.T) {
	// 内容必须大于 minBinaryBytes，否则会被当成错误页挡掉。
	body := make([]byte, minBinaryBytes+1024)
	for i := range body {
		body[i] = byte(i % 251)
	}
	server, hits := fakeReleaseServer(t, http.StatusOK, body)
	withFakeVerify(t, nil)

	dir := t.TempDir()
	got, err := Download(context.Background(), DownloadOptions{
		Version:     "v9.9.9",
		ReleaseBase: server.URL,
		Dir:         dir,
		GOOS:        "linux",
		GOARCH:      "amd64",
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	want := filepath.Join(dir, "nexttrace_linux_amd64")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("Download returned %q, want %q", got, want)
	}
	if *hits != 1 {
		t.Errorf("made %d requests, want 1", *hits)
	}

	// 内容必须落盘且完整。
	written, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("下载的文件不存在: %v", err)
	}
	if len(written) != len(body) {
		t.Errorf("落盘 %d 字节, want %d", len(written), len(body))
	}

	// 不留临时文件：留着会让人以为有多个版本。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".part") {
			t.Errorf("残留临时文件: %s", entry.Name())
		}
	}
}

// TestDownloadRejectsErrorPage 验证"200 但内容很小"被挡住。
//
// 上游偶发返回 HTML 错误页（状态码 200）。只检查状态码会把它
// 当成成功，于是磁盘上留下一个几十字节的"引擎"。
func TestDownloadRejectsErrorPage(t *testing.T) {
	server, _ := fakeReleaseServer(t, http.StatusOK, []byte("<html>rate limited</html>"))
	withFakeVerify(t, nil)

	dir := t.TempDir()
	_, err := Download(context.Background(), DownloadOptions{
		ReleaseBase: server.URL,
		Dir:         dir,
		GOOS:        "linux",
		GOARCH:      "amd64",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("Download accepted an HTML error page as the engine")
	}
	if !strings.Contains(err.Error(), "不像一个可执行文件") {
		t.Errorf("error = %v, want it to explain the size check", err)
	}

	// 不能留下任何文件：留一个坏文件比什么都不留更糟，
	// 下次启动会以为"已经装好了"。
	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
		t.Errorf("拒绝下载后仍留下文件: %v", entries)
	}
}

// TestDownloadRejectsHTTPError 验证非 200 报错。
func TestDownloadRejectsHTTPError(t *testing.T) {
	server, _ := fakeReleaseServer(t, http.StatusNotFound, []byte("not found"))
	withFakeVerify(t, nil)

	_, err := Download(context.Background(), DownloadOptions{
		ReleaseBase: server.URL,
		Dir:         t.TempDir(),
		GOOS:        "linux",
		GOARCH:      "amd64",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("Download accepted a 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to mention the status code", err)
	}
}

// TestDownloadRemovesBinaryThatCannotRun 验证"下到了但跑不起来"会被清理。
//
// 典型场景：下到了另一个架构的二进制。留着它会让每次跟踪都失败，
// 而错误信息看起来像"引擎有问题"而不是"下载有问题"。
func TestDownloadRemovesBinaryThatCannotRun(t *testing.T) {
	body := make([]byte, minBinaryBytes+1024)
	server, _ := fakeReleaseServer(t, http.StatusOK, body)
	withFakeVerify(t, os.ErrPermission)

	dir := t.TempDir()
	_, err := Download(context.Background(), DownloadOptions{
		ReleaseBase: server.URL,
		Dir:         dir,
		GOOS:        "linux",
		GOARCH:      "amd64",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("Download accepted a binary that cannot run")
	}

	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
		t.Errorf("跑不起来的二进制没有被清理: %v", entries)
	}
}

// TestDownloadRejectsUnsupportedTarget 验证不支持的平台在联网前就被拒绝。
func TestDownloadRejectsUnsupportedTarget(t *testing.T) {
	server, hits := fakeReleaseServer(t, http.StatusOK, make([]byte, minBinaryBytes+1))
	withFakeVerify(t, nil)

	_, err := Download(context.Background(), DownloadOptions{
		ReleaseBase: server.URL,
		Dir:         t.TempDir(),
		GOOS:        "plan9",
		GOARCH:      "amd64",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("Download accepted an unsupported platform")
	}
	if *hits != 0 {
		t.Errorf("对不支持的平台仍发起了 %d 次请求", *hits)
	}
}

// ---------------------------------------------------------------------------
// 与引擎构造的配合
// ---------------------------------------------------------------------------

// TestResolveBinaryFindsUpstreamAssetName 验证**上游资源名**也能被找到。
//
// 这条测试是一次真实缺陷的回归护栏：自动下载存的是上游的文件名
// （`nexttrace_windows_amd64.exe`），而查找只认 `nexttrace` /
// `nexttrace.exe`，于是下一次启动找不到、**每次运行都重下 32 MB**。
// 同样地，按上游说明手工下载后放进 data/bin 的文件也该被发现。
func TestResolveBinaryFindsUpstreamAssetName(t *testing.T) {
	asset, err := AssetName("", "")
	if err != nil {
		t.Fatalf("AssetName: %v", err)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, asset)
	if err := os.WriteFile(target, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 用默认名字查找：目录里只有上游命名的那份。
	got, err := resolveBinary(DefaultBinary, []string{dir})
	if err != nil {
		t.Fatalf("resolveBinary could not find the downloaded asset name: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(target) {
		t.Errorf("resolveBinary = %q, want %q", got, target)
	}
}

// TestEngineDoesNotDownloadWhenNotAsked 验证默认**不**下载。
//
// 这是测试能安全运行的前提：单元测试里构造一个不存在的引擎时
// 绝不能偷偷联网下 32 MB。自动下载必须由生产路径显式打开。
func TestEngineDoesNotDownloadWhenNotAsked(t *testing.T) {
	_, hits := fakeReleaseServer(t, http.StatusOK, make([]byte, minBinaryBytes+1))
	defer func() {
		// 这条断言才是本测试的核心：整个构造过程不该碰网络。
		if *hits != 0 {
			t.Errorf("未启用自动下载，却发起了 %d 次请求", *hits)
		}
	}()

	_, err := NewNextTraceEngine(context.Background(), EngineOptions{
		BinaryPath:  "definitely-not-installed-xyz",
		SearchDirs:  []string{t.TempDir()},
		DownloadDir: t.TempDir(),
		// AutoDownload 未设置 -> 不做任何下载
	})
	if err == nil {
		t.Fatal("the constructor succeeded without an engine")
	}
	if !strings.Contains(err.Error(), NotFoundMessage) {
		t.Errorf("error = %v, want the not-found message", err)
	}
}

// TestEngineDoesNotDownloadForExplicitPath 验证显式路径不会被覆盖。
//
// 使用者写了 --binary /path/to/nexttrace，那就不该因为"这个文件不可用"
// 而换成别的东西——他会以为自己配的生效了。
func TestEngineDoesNotDownloadForExplicitPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "my-own-nexttrace")

	_, err := NewNextTraceEngine(context.Background(), EngineOptions{
		BinaryPath:   missing,
		SearchDirs:   []string{dir},
		AutoDownload: true,
		DownloadDir:  dir,
	})
	if err == nil {
		t.Fatal("the constructor succeeded with a missing explicit binary")
	}
	if !strings.Contains(err.Error(), "is not usable") {
		t.Errorf("error = %v, want it to complain about the explicit path", err)
	}

	// 显式路径失败时不该顺手下一份到那个目录。
	if _, statErr := os.Stat(filepath.Join(dir, "nexttrace_linux_amd64")); statErr == nil {
		t.Error("显式路径不可用时仍然下载了别的引擎")
	}
}

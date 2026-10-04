package trace

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultVersionsCarryTheTagPrefix 验证默认版本号带 `v` 前缀。
//
// 这条测试来自一个真实发生的 404：上游的 tag 是 `v2.2.2`，而这里
// 一度写成 `2.2.2`，于是拼出来的下载地址 404。当时症状只是
// "WinDivert 未能就绪"，看不出是版本号拼错了。
//
// 两个项目的 tag 都带 `v`，因此直接钉住这个约定。
func TestDefaultVersionsCarryTheTagPrefix(t *testing.T) {
	for name, version := range map[string]string{
		"nexttrace": DefaultVersion,
		"windivert": DefaultWindivertVersion,
	} {
		if !strings.HasPrefix(version, "v") {
			t.Errorf("%s default version = %q; upstream tags carry a leading v", name, version)
		}
	}
}

// 本文件覆盖 Windows TCP/UDP 模式需要的 WinDivert 两个文件。
//
// 重点在"驱动"这两个字：它是内核驱动，出问题的代价比用户态程序
// 大得多，因此校验、原子落盘、以及"不该装的时候不装"都要测到。

// makeWindivertZip 造一个与上游同形状的压缩包。
//
// 上游的顶层目录带着版本号（WinDivert-2.2.2-A/x64/...），这里
// 刻意也用带版本号的目录名：实现里若写死了完整路径，换版本就会
// 找不到文件，而这个测试会立刻发现。
func makeWindivertZip(t *testing.T, topDir string, entries map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, data := range entries {
		handle, err := writer.Create(topDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// windivertServer 提供一个假的压缩包服务。
func windivertServer(t *testing.T, body []byte) (*httptest.Server, *int) {
	t.Helper()

	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

// TestWindivertNeededOnlyForTCPAndUDP 验证"不该装的时候不装"。
//
// 这是本文件里最重要的断言：ICMP 模式用系统自带能力，给它下载一个
// 内核驱动是不合适的。判错了会让所有 ICMP 用户平白多一个驱动。
func TestWindivertNeededOnlyForTCPAndUDP(t *testing.T) {
	cases := map[Mode]bool{
		ModeTCP:  true,
		ModeUDP:  true,
		ModeICMP: false,
		"":       false,
		"gre":    false,
	}

	for mode, want := range cases {
		if got := WindivertNeeded(mode); got != want {
			t.Errorf("WindivertNeeded(%q) = %v, want %v", mode, got, want)
		}
	}
}

// TestDownloadWindivertExtractsX64Files 验证解出正确的两个文件。
func TestDownloadWindivertExtractsX64Files(t *testing.T) {
	blob := makeWindivertZip(t, "WinDivert-2.2.2-A", map[string][]byte{
		"x64/WinDivert.dll":   bytes.Repeat([]byte("D"), 4096),
		"x64/WinDivert64.sys": bytes.Repeat([]byte("S"), 8192),
		"x86/WinDivert.dll":   []byte("32-bit dll, must not be used"),
		"x86/WinDivert32.sys": []byte("32-bit sys, must not be used"),
		"LICENSE.txt":         []byte("license"),
	})
	server, hits := windivertServer(t, blob)

	dir := t.TempDir()
	installed, err := DownloadWindivert(context.Background(), WindivertOptions{
		ReleaseBase: server.URL,
		Dir:         dir,
		GOOS:        "windows",
		HTTPClient:  server.Client(),
	})
	if err != nil {
		t.Fatalf("DownloadWindivert: %v", err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed %d files, want 2: %v", len(installed), installed)
	}
	if *hits != 1 {
		t.Errorf("made %d requests, want 1", *hits)
	}

	// 必须是 x64 那份，不能是 x86。
	got, err := os.ReadFile(filepath.Join(dir, "WinDivert.dll"))
	if err != nil {
		t.Fatalf("WinDivert.dll missing: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("D"), 4096)) {
		t.Error("WinDivert.dll is not the x64 entry")
	}

	sys, err := os.ReadFile(filepath.Join(dir, "WinDivert64.sys"))
	if err != nil {
		t.Fatalf("WinDivert64.sys missing: %v", err)
	}
	if !bytes.Equal(sys, bytes.Repeat([]byte("S"), 8192)) {
		t.Error("WinDivert64.sys is not the x64 entry")
	}

	// 32 位的文件不该被写出来。
	if _, err := os.Stat(filepath.Join(dir, "WinDivert32.sys")); err == nil {
		t.Error("32-bit driver was installed")
	}

	// 不留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 5 && entry.Name()[len(entry.Name())-5:] == ".part" {
			t.Errorf("leftover temp file: %s", entry.Name())
		}
	}
}

// TestDownloadWindivertRejectsBadZip 验证坏压缩包被拒绝且不留残骸。
func TestDownloadWindivertRejectsBadZip(t *testing.T) {
	server, _ := windivertServer(t, []byte("this is not a zip"))

	dir := t.TempDir()
	_, err := DownloadWindivert(context.Background(), WindivertOptions{
		ReleaseBase: server.URL,
		Dir:         dir,
		GOOS:        "windows",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("DownloadWindivert accepted a non-zip body")
	}

	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
		t.Errorf("拒绝后仍留下文件: %v", entries)
	}
}

// TestDownloadWindivertRejectsMissingEntry 验证压缩包里缺文件时报错。
//
// 上游将来把 x64 目录改名、或发布一个不含驱动的包时，必须明确
// 失败，而不是装上一个不完整的东西。
func TestDownloadWindivertRejectsMissingEntry(t *testing.T) {
	blob := makeWindivertZip(t, "WinDivert-9.9.9-A", map[string][]byte{
		"x64/WinDivert.dll": bytes.Repeat([]byte("D"), 1024),
		// 故意不放 WinDivert64.sys
	})
	server, _ := windivertServer(t, blob)

	_, err := DownloadWindivert(context.Background(), WindivertOptions{
		ReleaseBase: server.URL,
		Dir:         t.TempDir(),
		GOOS:        "windows",
		HTTPClient:  server.Client(),
	})
	if err == nil {
		t.Fatal("DownloadWindivert accepted a zip without the driver")
	}
}

// TestDownloadWindivertHashMismatchIsFatal 验证默认地址下哈希不符会中止。
//
// 这条测试的意义：一个被中间人替换的内核驱动会被加载进内核。
// 因此"哈希不符"必须是**硬失败**，不能降级成警告。
func TestDownloadWindivertHashMismatchIsFatal(t *testing.T) {
	// 内容是一个合法压缩包，但它的哈希不可能等于钉住的常量。
	blob := makeWindivertZip(t, "WinDivert-2.2.2-A", map[string][]byte{
		"x64/WinDivert.dll":   bytes.Repeat([]byte("D"), 1024),
		"x64/WinDivert64.sys": bytes.Repeat([]byte("S"), 1024),
	})

	dir := t.TempDir()
	// 不设 ReleaseBase：走默认地址，因此会做哈希校验。
	// 用自定义 HTTPClient 把请求导向假服务端，模拟"默认地址返回了被换过的包"。
	client := &http.Client{Transport: &rewriteTransport{body: blob}}

	_, err := DownloadWindivert(context.Background(), WindivertOptions{
		Dir:        dir,
		GOOS:       "windows",
		HTTPClient: client,
	})
	if err == nil {
		t.Fatal("DownloadWindivert accepted a zip whose hash does not match")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("哈希不符")) {
		t.Errorf("error = %v, want it to report the hash mismatch", err)
	}

	// 被换过的驱动绝不能落盘。
	for _, name := range WindivertFiles {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil {
			t.Errorf("哈希不符的驱动仍然落盘: %s", name)
		}
	}
}

// rewriteTransport 把任意请求导向一份固定响应。
//
// 用途：在不改 DownloadWindivert 的前提下，让"默认地址"这条路径
// 收到我们控制的字节，从而测到哈希校验分支。
type rewriteTransport struct {
	body []byte
}

func (t *rewriteTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     make(http.Header),
	}, nil
}

// TestMissingWindivert 验证缺失检测。
func TestMissingWindivert(t *testing.T) {
	dir := t.TempDir()

	missing := MissingWindivert(dir)
	if len(missing) != len(WindivertFiles) {
		t.Errorf("MissingWindivert on an empty dir = %v, want all %d files", missing, len(WindivertFiles))
	}

	for _, name := range WindivertFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if missing := MissingWindivert(dir); len(missing) != 0 {
		t.Errorf("MissingWindivert = %v, want empty", missing)
	}

	// 少一个也要被发现。
	if err := os.Remove(filepath.Join(dir, "WinDivert64.sys")); err != nil {
		t.Fatal(err)
	}
	missing = MissingWindivert(dir)
	if len(missing) != 1 || missing[0] != "WinDivert64.sys" {
		t.Errorf("MissingWindivert = %v, want [WinDivert64.sys]", missing)
	}
}

// TestDownloadWindivertSkipsNonWindows 验证非 Windows 上是空操作。
func TestDownloadWindivertSkipsNonWindows(t *testing.T) {
	// 断言"不发请求"比断言"返回空"更重要：Linux 上不该为
	// Windows 驱动发起任何网络请求。
	blob := makeWindivertZip(t, "WinDivert-2.2.2-A", map[string][]byte{
		"x64/WinDivert.dll": bytes.Repeat([]byte("D"), 64),
	})
	server, hits := windivertServer(t, blob)

	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		installed, err := DownloadWindivert(context.Background(), WindivertOptions{
			ReleaseBase: server.URL,
			Dir:         t.TempDir(),
			GOOS:        goos,
			HTTPClient:  server.Client(),
		})
		if err != nil {
			t.Errorf("%s: unexpected error: %v", goos, err)
		}
		if len(installed) != 0 {
			t.Errorf("%s: installed %v, want nothing", goos, installed)
		}
	}
	if *hits != 0 {
		t.Errorf("在非 Windows 平台上发起了 %d 次请求", *hits)
	}
}

var _ = fmt.Sprintf

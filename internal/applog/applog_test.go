package applog

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritesToFileAndConsole 验证日志同时落到文件与控制台。
//
// 这是"脱离命令行启动"能成立的前提：没有终端时，
// 日志文件是唯一的排查入口。
func TestWritesToFileAndConsole(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer

	logger := New(Options{Dir: dir, Console: &console})
	defer func() { _ = logger.Close() }()

	if logger.Path() == "" {
		t.Fatal("Path() is empty; nothing would be written to a file")
	}

	logger.Info("scan started", "workers", 100)
	logger.Warn("engine missing")

	// 控制台
	consoleText := console.String()
	for _, want := range []string{"scan started", "workers=100", "engine missing"} {
		if !strings.Contains(consoleText, want) {
			t.Errorf("console output missing %q:\n%s", want, consoleText)
		}
	}

	// 文件
	fileText, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, want := range []string{"scan started", "workers=100", "engine missing"} {
		if !strings.Contains(string(fileText), want) {
			t.Errorf("log file missing %q:\n%s", want, fileText)
		}
	}
}

// TestConsoleOnlyWhenDirEmpty 验证不给目录时只写控制台、不建文件。
func TestConsoleOnlyWhenDirEmpty(t *testing.T) {
	var console bytes.Buffer
	logger := New(Options{Console: &console})
	defer func() { _ = logger.Close() }()

	if logger.Path() != "" {
		t.Errorf("Path() = %q, want empty when no directory is configured", logger.Path())
	}

	logger.Info("hello")
	if !strings.Contains(console.String(), "hello") {
		t.Errorf("console output = %q", console.String())
	}
}

// TestRotatesAndKeepsBackups 验证超限轮转且保留有限个备份。
//
// 轮转是最容易静默失效的部分：写错了不会报错，
// 只会在某天发现磁盘被日志占满。
func TestRotatesAndKeepsBackups(t *testing.T) {
	dir := t.TempDir()

	// 很小的上限，逼它频繁轮转。
	const maxBytes = 512
	const maxBackups = 2

	logger := New(Options{Dir: dir, MaxBytes: maxBytes, MaxBackups: maxBackups})
	defer func() { _ = logger.Close() }()

	base := logger.Path()
	if base == "" {
		t.Fatal("no log file path")
	}

	// 写足够多的日志触发多次轮转。
	for i := 0; i < 120; i++ {
		logger.Info("this is a fairly long log line to fill the file quickly", "i", i)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 主文件必须存在。
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("main log file missing: %v", err)
	}

	// 备份最多 maxBackups 个，且比 maxBackups 更旧的不能存在。
	for i := 1; i <= maxBackups; i++ {
		name := base + "." + itoa(i)
		if _, err := os.Stat(name); err != nil {
			t.Errorf("expected backup %s to exist: %v", filepath.Base(name), err)
		}
	}
	if _, err := os.Stat(base + "." + itoa(maxBackups+1)); err == nil {
		t.Errorf("backup %d exists, want at most %d backups", maxBackups+1, maxBackups)
	}

	// 单次写入后的大小不应远超上限（允许超出一条日志的长度）。
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes*2 {
		t.Errorf("main log file is %d bytes, want it to stay near the %d-byte limit",
			info.Size(), maxBytes)
	}
}

// TestLogLinesAreNotTruncatedAcrossRotation 验证轮转不会把日志行截断。
//
// 截断的日志比没有日志更难读懂：半行 JSON 或半条错误信息
// 会让人误判问题。
func TestLogLinesAreNotTruncatedAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	logger := New(Options{Dir: dir, MaxBytes: 256, MaxBackups: 1})
	defer func() { _ = logger.Close() }()

	// 每条都很长，必然跨越轮转边界。
	const marker = "UNIQUE-LINE-END-MARKER"
	for i := 0; i < 40; i++ {
		logger.Info(strings.Repeat("x", 100), "marker", marker)
	}
	_ = logger.Close()

	// 主文件与备份里的每一行都应当是完整的
	// （含 marker 与结束引号，说明没有半行）。
	base := LogFilePath(dir, DefaultFileName)
	for _, path := range []string{base, base + ".1"} {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
			if line == "" {
				continue
			}
			if !strings.Contains(line, marker) {
				t.Errorf("%s has a line without the marker (truncated?):\n%s", filepath.Base(path), line)
			}
		}
	}
}

// TestRotatesExistingOversizedFileOnStartup 验证启动时发现文件已超限会先轮转。
//
// 场景：上次运行写到一半被强杀，留下的文件已超限。
// 如果不检查，新的日志会继续追加到一个已经很大的文件里。
func TestRotatesExistingOversizedFileOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := LogFilePath(dir, DefaultFileName)

	if err := os.WriteFile(path, []byte(strings.Repeat("old log line\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	logger := New(Options{Dir: dir, MaxBytes: 100, MaxBackups: 2})
	defer func() { _ = logger.Close() }()

	logger.Info("fresh start")

	// 旧的超大文件应当被挪到 .1，主文件重新从小开始。
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected the oversized file to be rotated to .1: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("main log file is %d bytes (was %d before); it should have been rotated",
			after.Size(), before.Size())
	}
}

// TestCreatesMissingDirectory 验证日志目录不存在时会创建。
func TestCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")

	logger := New(Options{Dir: dir})
	defer func() { _ = logger.Close() }()

	if logger.Path() == "" {
		t.Fatal("no log file path; the directory was not created")
	}
	logger.Info("hello")

	if _, err := os.Stat(logger.Path()); err != nil {
		t.Errorf("log file was not created: %v", err)
	}
}

// TestUnwritableDirectoryDegradesGracefully 验证日志目录不可写时
// 不返回错误、而是退化成控制台输出并说明原因。
//
// 日志是辅助设施：它挂了不该让主程序起不来。
func TestUnwritableDirectoryDegradesGracefully(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks do not apply")
	}

	// 用一个"父路径是文件"的目录，必然创建失败。
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var console bytes.Buffer
	logger := New(Options{Dir: filepath.Join(file, "logs"), Console: &console})
	defer func() { _ = logger.Close() }()

	// 必须仍然可用（写到控制台）。
	logger.Info("still works")

	if !strings.Contains(console.String(), "still works") {
		t.Errorf("logger stopped working: %q", console.String())
	}
	// 必须说明为什么没写文件。
	if !strings.Contains(console.String(), "cannot open log file") {
		t.Errorf("console does not explain the failure: %q", console.String())
	}
	if logger.Path() != "" {
		t.Errorf("Path() = %q, want empty when the file could not be opened", logger.Path())
	}
}

// TestLevelFiltering 验证级别过滤生效。
func TestLevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logger := New(Options{Dir: dir, Level: slog.LevelWarn})
	defer func() { _ = logger.Close() }()

	logger.Info("should be filtered out")
	logger.Warn("should be kept")

	content, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "should be filtered out") {
		t.Error("Info-level message was written despite Level=Warn")
	}
	if !strings.Contains(string(content), "should be kept") {
		t.Error("Warn-level message was dropped")
	}
}

// TestDiscardLoggerIsSafe 验证测试用的空日志器不会 panic。
func TestDiscardLoggerIsSafe(t *testing.T) {
	logger := Discard()
	logger.Info("nothing happens")
	if err := logger.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if logger.Path() != "" {
		t.Errorf("Path() = %q, want empty", logger.Path())
	}

	// nil 接收者也不该 panic（调用方可能忘了初始化）。
	var nilLogger *Logger
	nilLogger.Info("safe")
	if err := nilLogger.Close(); err != nil {
		t.Errorf("nil Close() = %v", err)
	}
	if nilLogger.Path() != "" {
		t.Errorf("nil Path() = %q", nilLogger.Path())
	}
}

// TestCloseIsIdempotent 验证重复 Close 不报错。
func TestCloseIsIdempotent(t *testing.T) {
	logger := New(Options{Dir: t.TempDir()})
	if err := logger.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// itoa 是本文件用的整数格式化。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

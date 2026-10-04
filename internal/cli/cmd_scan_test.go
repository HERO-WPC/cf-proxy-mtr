package cli

import (
	"encoding/csv"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 `scan` 命令的**命令行表面**。
//
// 扫描结果只写 CSV：没有数据库、没有会话、没有续测。
// 因此这里不再有"续测""会话 ID""落库"之类的测试——
// 那些行为已经不存在，留着测它们只会让测试与实现脱节。

// scanCSVArgs 构造一组把命令完全限制在本地的参数。
//
// 显式禁用备用数据源：万一缓存没命中，测试会立刻失败，
// 而不是去访问 zip.cm.edu.kg。
func scanCSVArgs(cachePath, outPath, identityPath string, extra ...string) []string {
	args := []string{
		"scan",
		"--url", sourceDefaultURL(),
		"--fallback-url", "",
		"--cache", cachePath,
		"--source-retries", "0",
		"--out", outPath,
		"--identity", identityPath,
		// 测试**绝不能**触发 nexttrace 自动下载：那会在单元测试里
		// 拉 32 MB、依赖外网、并且把测试拖到超时。
		// 下载路径由 internal/trace 的单元测试用假服务端覆盖。
		"--trace-no-download",
		// 同理：夹具给的是固定缓存、断言的是固定目标，
		// 默认的"网络优先"会去下载真实列表，测试就变成依赖外网了。
		"--source-cache-first",
	}
	return append(args, extra...)
}

// writeScanCache 把一段目标列表写进临时缓存并返回路径。
func writeScanCache(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "all.json")
	if err := writeCacheFromJSON(t, path, body); err != nil {
		t.Fatalf("writeCacheFromJSON: %v", err)
	}
	return path
}

// readResultCSV 读结果文件。
func readResultCSV(t *testing.T, path string) [][]string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return records
}

// TestScanHelpListsCSVFlags 验证帮助里说明了结果文件参数。
func TestScanHelpListsCSVFlags(t *testing.T) {
	code, stdout, stderr := runCLI("scan", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"-out", "-append", "-limit", "-workers", "-timeout",
		"-trace", "-trace-mode", "-trace-binary", "-identity",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("scan help missing %q", want)
		}
	}

	// 会话与数据库的参数不该再出现：它们已经不存在了。
	// 帮助里留着它们会让用户以为还能用。
	for _, gone := range []string{"-resume", "-session", "-new", "-db"} {
		if strings.Contains(stdout, gone) {
			t.Errorf("scan help still mentions %q, but that flag no longer exists", gone)
		}
	}
}

// TestScanWritesResultsToCSV 验证一次扫描真的把结果写进 CSV。
func TestScanWritesResultsToCSV(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	code, stdout, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"), "--timeout", "2s")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	// 汇总必须给出结果文件路径——否则用户不知道去哪儿找结果。
	if !strings.Contains(stdout, out) {
		t.Errorf("stdout does not mention the output file:\n%s", stdout)
	}
	if !strings.Contains(stdout, "rows:") {
		t.Errorf("stdout does not report how many rows were written:\n%s", stdout)
	}

	records := readResultCSV(t, out)
	if len(records) != 2 {
		t.Fatalf("csv rows = %d, want 2 (header + 1)", len(records))
	}
	if records[0][0] != "timestamp_utc" {
		t.Errorf("first header column = %q, want timestamp_utc", records[0][0])
	}
	if !strings.Contains(records[1][1], "127.0.0.1:") {
		t.Errorf("target column = %q, want a 127.0.0.1 target", records[1][1])
	}
	if records[1][4] != "true" {
		t.Errorf("success = %q, want true (a real listener is accepting)", records[1][4])
	}
}

// TestScanDoesNotCreateDatabase 验证扫描不再碰数据库。
func TestScanDoesNotCreateDatabase(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	code, _, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"), "--timeout", "2s")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".db") || strings.Contains(entry.Name(), ".db-") {
			t.Errorf("scan created a database file %q; it must only write CSV", entry.Name())
		}
	}
}

// TestScanEmptyOutAutoNames 验证空 --out 不再报错，而是自动命名。
//
// 契约变过一次：以前空 --out 是用法错误，默认值又写死成
// data/results.csv，于是每跑一轮覆盖上一轮且没有提示。现在留空表示
// "按日期时间自动命名"，每轮一份独立结果。
func TestScanEmptyOutAutoNames(t *testing.T) {
	// 同 service 那条：自动命名是相对路径，会把文件写进包目录。
	t.Chdir(t.TempDir())

	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()

	code, stdout, stderr := runCLI(scanCSVArgs(cache, "", filepath.Join(dir, "collector.json"))...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeOK, stderr)
	}

	// 使用者必须能从输出里知道结果落在哪——自动命名不能让文件"找不到"。
	if !strings.Contains(stdout+stderr, "results-") {
		t.Errorf("output does not report the auto-generated file name:\nstdout=%s\nstderr=%s",
			stdout, stderr)
	}
	// 扫描开始前那句提示也不能说"覆盖"：那会让人以为上一轮要没了。
	if strings.Contains(stdout, "（覆盖）") {
		t.Errorf("stdout still claims it will overwrite:\n%s", stdout)
	}
}

// TestScanRejectsBadTraceMode 验证非法跟踪模式在任何副作用之前被拒绝。
func TestScanRejectsBadTraceMode(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	code, _, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"), "--trace", "--trace-mode", "gre")...)
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
	}
	if !strings.Contains(stderr, "trace mode") {
		t.Errorf("stderr = %q, want it to mention the trace mode", stderr)
	}
	// 连结果文件都不该被创建。
	if _, err := os.Stat(out); err == nil {
		t.Error("the results CSV was created before the parameters were validated")
	}
}

// TestScanEmptyTargetListIsError 验证没有目标时明确报错。
func TestScanEmptyTargetListIsError(t *testing.T) {
	empty := `{"generated_at":"2026-10-03T00:00:00","list":{"ips":0},"data":[]}`
	cache := writeRawCacheFromJSON(t, empty)
	dir := t.TempDir()

	code, stdout, stderr := runCLI(scanCSVArgs(cache, filepath.Join(dir, "results.csv"),
		filepath.Join(dir, "collector.json"), "--quiet")...)

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "no targets") {
		t.Errorf("stderr = %q, want a clear message", stderr)
	}
}

// TestScanLimitKeepsSourceOrder 验证 --limit 只测前 N 个目标。
//
// 断言的是"**恰好测了 N 个**、且它们都来自这份目标列表"，
// 而不是"具体是哪几个"。原因：解析层会把同一个 IP 的端口展开成
// 目标，并按端口排序，因此"第 N 个监听器"与"第 N 个目标"并不是
// 同一个东西。早先这里断言了 listeners[2] 必须被跳过，在端口恰好
// 升序时能过；一旦端口顺序不同（CI 上就发生了）就会误报——
// 那是**测试在假设实现细节**，不是产品出错。
func TestScanLimitKeepsSourceOrder(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	code, _, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"),
		"--limit", "2", "--timeout", "2s", "--workers", "2")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	records := readResultCSV(t, out)
	if len(records) != 3 {
		t.Fatalf("csv rows = %d, want 3 (header + 2)", len(records))
	}

	// 这份目标列表里合法的端口（三个监听器）。
	allowed := make(map[string]bool, len(listeners))
	for _, listener := range listeners {
		allowed[listener.Addr().String()] = true
	}

	// 恰好 2 个**互不相同**的目标，且都来自该列表。
	measured := make(map[string]bool, 2)
	for _, record := range records[1:] {
		target := record[1]
		if !allowed[target] {
			t.Errorf("measured target %q is not from the source list", target)
		}
		if measured[target] {
			t.Errorf("target %q was measured twice; --limit 2 should measure two distinct targets", target)
		}
		measured[target] = true
	}
	if len(measured) != 2 {
		t.Errorf("measured %d distinct targets, want 2", len(measured))
	}
}

// TestScanAppendKeepsPreviousResults 验证 --append 不覆盖已有结果。
func TestScanAppendKeepsPreviousResults(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")
	identity := filepath.Join(dir, "collector.json")

	for i := 0; i < 2; i++ {
		args := scanCSVArgs(cache, out, identity, "--timeout", "2s", "--append")
		if code, _, stderr := runCLI(args...); code != ExitCodeOK {
			t.Fatalf("run %d: exit code = %d (stderr=%q)", i, code, stderr)
		}
	}

	records := readResultCSV(t, out)
	if len(records) != 3 {
		t.Fatalf("csv rows = %d, want 3 (one header + two runs)", len(records))
	}
	for i, record := range records {
		if i > 0 && record[0] == "timestamp_utc" {
			t.Errorf("header repeated at row %d", i)
		}
	}
}

// TestScanTraceFlagReportsUnavailableEngine 验证引擎不可用时
// TCP 结果照常写入，并明确说明跟踪被跳过。
func TestScanTraceFlagReportsUnavailableEngine(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	code, stdout, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"),
		"--trace", "--trace-binary", "definitely-not-installed-nexttrace-xyz",
		"--timeout", "2s")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	// TCP 结果必须已经写进去了：跟踪不可用不该让整轮白跑。
	records := readResultCSV(t, out)
	if len(records) < 2 {
		t.Fatalf("csv rows = %d, want at least one data row", len(records))
	}

	combined := stdout + stderr
	if !strings.Contains(combined, "SKIPPED") {
		t.Errorf("output does not say tracing was skipped:\n%s", combined)
	}
	if !strings.Contains(combined, "NextTrace not found.") {
		t.Errorf("output does not explain why:\n%s", combined)
	}
}

// TestScanProgressAndSummaryGoToExpectedStreams 验证进度走 stderr、
// 汇总走 stdout，且 --quiet 只压掉进度。
func TestScanProgressAndSummaryGoToExpectedStreams(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer closeAll(listeners)

	cache := writeScanCache(t, body)
	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")
	identity := filepath.Join(dir, "collector.json")

	code, stdout, stderr := runCLI(scanCSVArgs(cache, out, identity, "--timeout", "2s")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr, "probe completed") {
		t.Errorf("stderr does not show progress:\n%s", stderr)
	}
	if !strings.Contains(stdout, "rows:") {
		t.Errorf("stdout does not show the summary:\n%s", stdout)
	}

	// --quiet：进度没了，汇总还在。
	code, stdout, stderr = runCLI(scanCSVArgs(cache, out, identity, "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("quiet exit code = %d", code)
	}
	if strings.Contains(stderr, "probe completed") {
		t.Errorf("--quiet still printed progress:\n%s", stderr)
	}
	if !strings.Contains(stdout, "rows:") {
		t.Errorf("--quiet suppressed the summary too:\n%s", stdout)
	}
}

// TestScanTimeoutIsRespected 验证 --timeout 生效，且失败结果被如实记录。
//
// 用文档保留网段（不会被路由）确保连接超时。
func TestScanTimeoutIsRespected(t *testing.T) {
	// 192.0.2.0/24 是 RFC 5737 的文档网段，不会被路由。
	cache := writeScanCache(t, unroutableTargetJSON("192.0.2.1", 443))

	dir := t.TempDir()
	out := filepath.Join(dir, "results.csv")

	start := time.Now()
	code, _, stderr := runCLI(scanCSVArgs(cache, out,
		filepath.Join(dir, "collector.json"), "--timeout", "1s", "--quiet")...)
	elapsed := time.Since(start)

	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if elapsed > 8*time.Second {
		t.Errorf("scan took %s; --timeout 1s was not respected", elapsed)
	}

	records := readResultCSV(t, out)
	if len(records) != 2 {
		t.Fatalf("csv rows = %d, want 2", len(records))
	}
	if records[1][4] != "false" {
		t.Errorf("success = %q, want false for an unroutable target", records[1][4])
	}
	// 失败时延迟必须留空而不是 0：两者在表格里含义完全不同。
	if records[1][5] != "" {
		t.Errorf("latency = %q, want empty for a failed measurement", records[1][5])
	}
}

// closeAll 关闭测试用的监听。
func closeAll(listeners []net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

// unroutableTargetJSON 构造一份指向**不可路由**地址的目标列表。
//
// 用文档保留网段（RFC 5737 的 192.0.2.0/24）：它不会被路由，
// 因此连接必然超时，可以稳定地测试超时与失败记录。
func unroutableTargetJSON(ip string, port int) string {
	return `{"generated_at":"2026-10-03T00:00:00","list":{"ips":1},"data":[` +
		`{"ip":"` + ip + `","port":[` + itoaCLI(port) + `],` +
		`"latitude":"0","longitude":"0","country":"US","city":"Chicago"}]}`
}

// readFileString 读取文本文件内容。
//
// 放在这里而不是各测试文件里：它与 readResultCSV 服务于同一批
// "读回结果并断言"的测试。
func readFileString(path string) (string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// scanArgs 构造一组把 scan 完全限制在本地的参数。
//
// 显式禁用备用数据源：万一缓存没命中，测试应当立刻失败，
// 而不是去访问 zip.cm.edu.kg。
func scanArgs(cachePath, dbPath, identityPath string, extra ...string) []string {
	args := []string{
		"scan",
		"--url", sourceDefaultURL(),
		"--fallback-url", "",
		"--cache", cachePath,
		"--source-retries", "0",
		"--db", dbPath,
		"--identity", identityPath,
	}
	return append(args, extra...)
}

func TestScanHelpListsSessionFlags(t *testing.T) {
	code, stdout, stderr := runCLI("scan", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"Flags:",
		"-db",
		"-identity",
		"-resume",
		"-new",
		"-session",
		"-limit",
		"-workers",
		"-trace",
		"Sessions:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("scan help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestScanRejectsConflictingSessionFlags(t *testing.T) {
	code, _, stderr := runCLI("scan", "--resume", "--new")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr = %q, want a conflict message", stderr)
	}
}

func TestScanRejectsEmptyDatabasePath(t *testing.T) {
	code, _, stderr := runCLI("scan", "--db", "")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "--db") {
		t.Errorf("stderr = %q, want a message about --db", stderr)
	}
}

// TestScanStoresMeasurementsAndSession 是 Phase 5 的主干端到端测试。
func TestScanStoresMeasurementsAndSession(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")
	identityPath := filepath.Join(dir, "collector.json")

	code, stdout, stderr := runCLI(scanArgs(cachePath, dbPath, identityPath,
		"--workers", "3",
		"--timeout", "2s",
		"--country", "cn",
		"--province", "Zhejiang",
		"--city", "Hangzhou",
		"--isp", "China Mobile",
		"--asn", "9808",
		"--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	// 汇总格式是需求第 19 条指定的样式。
	for _, want := range []string{
		"session:",
		"targets:     3 total, 3 to measure",
		"Completed: 3 / 3",
		"Success:   3",
		"Failed:    0",
		"Rate:",
		"stored:      3 measurement(s)",
		"session:     finished",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("scan summary missing %q\ngot:\n%s", want, stdout)
		}
	}

	// 数据库里必须有一个已结束的会话、三条测量、正确的采集者画像。
	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ctx := t.Context()
	stats, err := store.CollectStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tables["measurements"] != 3 {
		t.Errorf("measurements = %d, want 3", stats.Tables["measurements"])
	}
	if stats.Tables["scan_sessions"] != 1 {
		t.Errorf("scan_sessions = %d, want 1", stats.Tables["scan_sessions"])
	}

	sessionID := extractSessionID(t, stdout)
	session, err := store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("LoadSession(%s): %v", sessionID, err)
	}
	if !session.Finished() {
		t.Error("session is not finished after a complete scan")
	}
	if session.TargetCount != 3 || session.CompletedCount != 3 {
		t.Errorf("session counts = %d/%d, want 3/3", session.CompletedCount, session.TargetCount)
	}

	// 每条测量都必须关联到这次会话（断点续测判据的基础）。
	measurements, err := store.QueryMeasurements(ctx, storage.MeasurementQuery{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 3 {
		t.Fatalf("measurements for session = %d, want 3", len(measurements))
	}

	collectors, err := store.LoadCollectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collectors) != 1 {
		t.Fatalf("collectors = %d, want 1", len(collectors))
	}
	if collectors[0].Country != "CN" || collectors[0].ASN != "AS9808" {
		t.Errorf("collector = %+v, want normalized CN/AS9808", collectors[0])
	}
}

// TestScanResumeOnlyMeasuresPendingTargets 是需求第 20 条的 CLI 级测试。
//
// 用一个"较大的 limit"与一个"较小的 limit"交错，验证续测确实
// 只测没测过的目标，并且不会因为重跑而重复计数。
func TestScanResumeOnlyMeasuresPendingTargets(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0, 0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")
	identityPath := filepath.Join(dir, "collector.json")

	// 第一次"中断的扫描"：只扫前 2 个目标（模拟跑到一半被杀）。
	code, first, stderr := runCLI(scanArgs(cachePath, dbPath, identityPath,
		"--limit", "2", "--workers", "2", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("first scan exit code = %d (stderr=%q)", code, stderr)
	}
	sessionID := extractSessionID(t, first)
	if !strings.Contains(first, "session:     finished") {
		t.Fatalf("first scan should finish its session:\n%s", first)
	}

	// 手工把会话重新打开：模拟"进程被杀，会话未结束"的真实中断场景。
	// （第一次扫描正常结束了，因此这里显式重置 finished_at。）
	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(),
		`UPDATE scan_sessions SET finished_at = NULL WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	_ = store.Close()

	// 续测：目标列表是全部 6 个，但前 2 个已经在本会话测过。
	code, resumed, stderr := runCLI(scanArgs(cachePath, dbPath, identityPath,
		"--resume", "--session", sessionID, "--workers", "4", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("resume exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"mode:        resume",
		"skipped 2 already-measured target(s)",
		"targets:     6 total, 4 to measure",
		"Completed: 4 / 4",
		"stored:      4 measurement(s)",
	} {
		if !strings.Contains(resumed, want) {
			t.Errorf("resume summary missing %q\ngot:\n%s", want, resumed)
		}
	}

	// 库里必须是 6 条测量（2 旧 + 4 新），没有任何重复。
	store, err = storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	all, err := store.QueryMeasurements(t.Context(), storage.MeasurementQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Errorf("measurements = %d, want 6 (2 before + 4 after resume)", len(all))
	}

	// 每个目标在本会话里只能有一条测量。
	perTarget := make(map[string]int, 6)
	for _, m := range all {
		perTarget[m.TargetID]++
	}
	for targetID, n := range perTarget {
		if n != 1 {
			t.Errorf("target %s has %d measurements, want 1", targetID, n)
		}
	}
}

// TestScanResumeWithoutSessionIDUsesLastSession 验证 --resume 的默认值。
func TestScanResumeWithoutSessionIDUsesLastSession(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")
	identityPath := filepath.Join(dir, "collector.json")

	code, first, stderr := runCLI(scanArgs(cachePath, dbPath, identityPath,
		"--limit", "1", "--workers", "1", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("first scan exit code = %d (stderr=%q)", code, stderr)
	}
	sessionID := extractSessionID(t, first)

	// 重新打开会话，模拟中断。
	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(),
		`UPDATE scan_sessions SET finished_at = NULL WHERE id = ?`, sessionID); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// 不带 --session：应当用身份文件里记住的上一个会话。
	code, resumed, stderr := runCLI(scanArgs(cachePath, dbPath, identityPath,
		"--resume", "--workers", "1", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("resume exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(resumed, sessionID) {
		t.Errorf("resume should reuse the remembered session %s:\n%s", sessionID, resumed)
	}
	if !strings.Contains(resumed, "mode:        resume") {
		t.Errorf("resume output should report resume mode:\n%s", resumed)
	}
}

// TestScanResumeMissingSessionIsActionable 验证续测失败给出可操作提示。
func TestScanResumeMissingSessionIsActionable(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	code, _, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		filepath.Join(dir, "collector.json"),
		"--resume", "--session", "20260101T000000Z-00000000",
		"--timeout", "2s", "--quiet")...)

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	// 错误信息必须告诉用户下一步怎么做。
	if !strings.Contains(stderr, "--new") {
		t.Errorf("stderr = %q, want a hint to use --new", stderr)
	}
}

// TestScanTraceFlagIsReportedAsSkipped 验证 --trace 在 Phase 7 之前
// 会明确说明"没做"，而不是静默跳过（那会让用户以为已经跟踪过）。
func TestScanTraceFlagIsReportedAsSkipped(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	code, stdout, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		filepath.Join(dir, "collector.json"),
		"--trace", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "trace:       SKIPPED") {
		t.Errorf("summary must state that tracing was skipped:\n%s", stdout)
	}
}

// TestScanProgressGoesToStderr 验证进度与汇总的输出流分离。
func TestScanProgressGoesToStderr(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	code, stdout, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		filepath.Join(dir, "collector.json"),
		"--workers", "2", "--timeout", "2s")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{"source:", "session:", "collector:", "concurrency:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr should carry the header (%q):\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "concurrency:") || strings.Contains(stdout, "collector:") {
		t.Errorf("stdout should not carry the header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "stored:") {
		t.Errorf("stdout should carry the summary:\n%s", stdout)
	}
}

// TestScanEmptyTargetListIsError 验证没有目标时明确报错。
func TestScanEmptyTargetListIsError(t *testing.T) {
	empty := `{"generated_at":"2026-10-03T00:00:00","list":{"ips":0},"data":[]}`
	cachePath := writeRawCacheFromJSON(t, empty)

	dir := t.TempDir()
	code, stdout, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		filepath.Join(dir, "collector.json"), "--quiet")...)

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "no targets to scan") {
		t.Errorf("stderr = %q, want a clear message", stderr)
	}
}

// TestScanRecordsLastSessionInIdentityFile 验证身份文件记住了会话。
func TestScanRecordsLastSessionInIdentityFile(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	identityPath := filepath.Join(dir, "collector.json")

	code, stdout, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		identityPath, "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	sessionID := extractSessionID(t, stdout)

	if !model.IsSessionID(sessionID) {
		t.Fatalf("session id %q has an unexpected format", sessionID)
	}

	blob, err := readFileString(identityPath)
	if err != nil {
		t.Fatalf("read identity file: %v", err)
	}
	if !strings.Contains(blob, sessionID) {
		t.Errorf("identity file does not remember session %s:\n%s", sessionID, blob)
	}
	// 身份文件里不能出现任何可识别信息。
	for _, forbidden := range []string{"public_ip", "local_ip", "mac", "hostname"} {
		if strings.Contains(strings.ToLower(blob), forbidden) {
			t.Errorf("identity file contains %q, which must never be persisted:\n%s", forbidden, blob)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// extractSessionID 从 scan 的汇总输出里取出会话 ID。
func extractSessionID(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "session:") {
			continue
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "session:"))
		// 汇总里 "session:     finished" 之类的行不是 ID。
		if model.IsSessionID(id) {
			return id
		}
	}
	t.Fatalf("no session id found in output:\n%s", stdout)
	return ""
}

// readFileString 读取文件内容为字符串。
func readFileString(path string) (string, error) {
	blob, err := osReadFile(path)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// TestScanLimitKeepsSourceOrder 验证 --limit 取的是源顺序的前 N 个。
func TestScanLimitKeepsSourceOrder(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")

	code, stdout, stderr := runCLI(scanArgs(cachePath, dbPath,
		filepath.Join(dir, "collector.json"),
		"--limit", "2", "--workers", "2", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "targets:     2 total, 2 to measure") {
		t.Errorf("--limit 2 should scan exactly 2 targets:\n%s", stdout)
	}

	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	// 目标表里只会写入被扫描的那两个（scan 只登记本次要测的目标）。
	targets, err := store.LoadTargets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Errorf("targets = %d, want 2", len(targets))
	}
}

// TestScanTimeoutIsRespected 验证 --timeout 被传下去（用一个必然超时的目标）。
func TestScanTimeoutIsRespected(t *testing.T) {
	// 192.0.2.0/24 是 TEST-NET-1，保证不可路由。
	body := `{
	  "generated_at": "2026-10-03T00:00:00",
	  "list": {"ips": 1},
	  "data": [{"ip": "192.0.2.1", "port": [443], "meta": {"country": "US", "city": "Chicago"}}]
	}`
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	start := time.Now()
	code, stdout, stderr := runCLI(scanArgs(cachePath, filepath.Join(dir, "results.db"),
		filepath.Join(dir, "collector.json"),
		"--workers", "1", "--timeout", "400ms", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	elapsed := time.Since(start)

	// 必须是非成功（要么超时要么不可达），且耗时接近 --timeout。
	if strings.Contains(stdout, "Success:   1") {
		t.Errorf("TEST-NET-1 target unexpectedly succeeded:\n%s", stdout)
	}
	if elapsed > 10*time.Second {
		t.Errorf("scan took %s; --timeout 400ms was not respected", elapsed)
	}
	// 失败也要入库（需求第 39 条）。
	if !strings.Contains(stdout, "stored:      1 measurement(s)") {
		t.Errorf("a failed measurement must still be stored:\n%s", stdout)
	}
}

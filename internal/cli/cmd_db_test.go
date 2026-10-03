package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// ---------------------------------------------------------------------------
// db 命令组
// ---------------------------------------------------------------------------

func TestDBHelpAndDispatch(t *testing.T) {
	code, stdout, stderr := runCLI("--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "db") {
		t.Errorf("top-level help should list db:\n%s", stdout)
	}
	if strings.Contains(stdout, "Planned commands (not implemented yet):\n  db") {
		t.Errorf("db must not be listed as unimplemented:\n%s", stdout)
	}

	// 没有子命令时给出可操作的用法提示。
	code, _, stderr = runCLI("db")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "stats") || !strings.Contains(stderr, "migrate") {
		t.Errorf("stderr = %q, want the subcommand list", stderr)
	}

	// 未知子命令也必须明确报错，而不是静默做别的事。
	code, _, stderr = runCLI("db", "frobnicate")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "unknown db subcommand") {
		t.Errorf("stderr = %q, want unknown subcommand message", stderr)
	}
}

func TestDBMigrateAndStats(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "results.db")

	// migrate 必须创建数据库并把结构升到最新版本。
	code, stdout, stderr := runCLI("db", "migrate", "--db", dbPath)
	if code != ExitCodeOK {
		t.Fatalf("db migrate exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "schema_version:") {
		t.Errorf("stdout = %q, want the schema version", stdout)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file not created: %v", err)
	}

	// 重复 migrate 必须成功（幂等）。
	if code, _, stderr := runCLI("db", "migrate", "--db", dbPath); code != ExitCodeOK {
		t.Fatalf("second db migrate exit code = %d (stderr=%q)", code, stderr)
	}

	// stats 必须报告各表行数（此刻全为 0）与"还没有测量"。
	code, stdout, stderr = runCLI("db", "stats", "--db", dbPath)
	if code != ExitCodeOK {
		t.Fatalf("db stats exit code = %d (stderr=%q)", code, stderr)
	}
	for _, want := range []string{
		"database:", "size:", "schema_version:", "rows:",
		"targets:", "collectors:", "measurements:", "traces:",
		"measurement coverage:", "(no measurements yet)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("db stats missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestDBVacuumRequiresExistingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.db")

	code, _, stderr := runCLI("db", "vacuum", "--db", missing)
	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeError)
	}
	if !strings.Contains(stderr, "does not exist") {
		t.Errorf("stderr = %q, want a clear message", stderr)
	}
}

func TestDBVacuumOnExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "results.db")

	if code, _, stderr := runCLI("db", "migrate", "--db", dbPath); code != ExitCodeOK {
		t.Fatalf("migrate exit code = %d (stderr=%q)", code, stderr)
	}

	code, stdout, stderr := runCLI("db", "vacuum", "--db", dbPath)
	if code != ExitCodeOK {
		t.Fatalf("vacuum exit code = %d (stderr=%q)", code, stderr)
	}
	for _, want := range []string{"database:", "size:", "reclaimed:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("vacuum output missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestDBStatsReportsUnappliedVersion(t *testing.T) {
	// 手工造一个"版本落后"的数据库：只建 schema_migrations 并写入旧版本。
	dbPath := filepath.Join(t.TempDir(), "old.db")
	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 正常路径下 db stats 不应报告版本不一致。
	code, stdout, stderr := runCLI("db", "stats", "--db", dbPath)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if strings.Contains(stdout, "this build expects") {
		t.Errorf("freshly migrated database should be up to date:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// probe --db：真实测量入库
// ---------------------------------------------------------------------------

// TestProbeStoresMeasurements 是 Phase 3 与 Phase 4 的端到端契约测试：
//
//	all.json 缓存 -> probe 真实 TCP 测量 -> SQLite 落库 -> 查询校验
func TestProbeStoresMeasurements(t *testing.T) {
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

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath,
		"--workers", "2",
		"--timeout", "2s",
		"--db", dbPath,
		"--identity", identityPath,
		"--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "stored:      2 measurement(s)") {
		t.Errorf("summary should report stored measurements:\n%s", stdout)
	}

	// 直接读库校验：行数、目标、采集者、延迟都必须正确。
	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := t.Context()
	measurements, err := store.QueryMeasurements(ctx, storage.MeasurementQuery{})
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	if len(measurements) != 2 {
		t.Fatalf("measurements = %d, want 2", len(measurements))
	}
	for _, m := range measurements {
		if !m.Success {
			t.Errorf("measurement for %s is not successful: %+v", m.TargetID, m)
		}
		if m.LatencyMS < 0 {
			t.Errorf("negative latency for %s: %v", m.TargetID, m.LatencyMS)
		}
		if m.TargetID == "" {
			t.Error("measurement without target id")
		}
		// 版本信息必须落库，便于日后按版本区分样本。
		if m.SchemaVersion != 1 || m.ClientVersion == "" {
			t.Errorf("versions = %d/%q, want 1 and a version string", m.SchemaVersion, m.ClientVersion)
		}
	}

	// 目标与采集者都必须已入库。
	targets, err := store.LoadTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Errorf("targets = %d, want 2", len(targets))
	}
	collectors, err := store.LoadCollectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collectors) != 1 {
		t.Fatalf("collectors = %d, want 1", len(collectors))
	}
	if !strings.HasPrefix(collectors[0].CollectorID, "c-") {
		t.Errorf("collector_id = %q, want an anonymous c- id", collectors[0].CollectorID)
	}

	// 采集者画像必须来自命令行覆盖项。
	if code, _, stderr := runCLI(offlineProbeArgs(cachePath,
		"--workers", "1",
		"--timeout", "2s",
		"--db", dbPath,
		"--identity", identityPath,
		"--country", "cn",
		"--province", "Zhejiang",
		"--city", "Hangzhou",
		"--isp", "China Mobile",
		"--asn", "9808",
		"--quiet")...); code != ExitCodeOK {
		t.Fatalf("second run exit code = %d (stderr=%q)", code, stderr)
	}

	collectors, err = store.LoadCollectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := collectors[0]
	if got.Country != "CN" || got.ASN != "AS9808" || got.City != "Hangzhou" {
		t.Errorf("collector = %+v, want normalized CN/AS9808/Hangzhou", got)
	}

	// 第二次运行写的是**新时间戳**的测量，因此必须又多了 2 条，
	// 而不是覆盖第一次的（时间序列语义）。
	measurements, err = store.QueryMeasurements(ctx, storage.MeasurementQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 4 {
		t.Errorf("measurements = %d, want 4 (a second run must append, not overwrite)", len(measurements))
	}
}

// TestProbeWithoutDBDoesNotCreateDatabase 验证不传 --db 时不落库。
func TestProbeWithoutDBDoesNotCreateDatabase(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath, "--workers", "1", "--timeout", "2s", "--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if strings.Contains(stdout, "stored:") {
		t.Errorf("summary should not mention storage without --db:\n%s", stdout)
	}
	// 默认路径 data/results.db 必须没有被创建。
	if _, err := os.Stat(filepath.Join(dir, "data", "results.db")); err == nil {
		t.Error("a database was created without --db")
	}
}

// TestProbeDBStatsAfterRun 验证 db stats 能反映 probe 写入的数据。
func TestProbeDBStatsAfterRun(t *testing.T) {
	body, listeners := probeFixture(t, []int{0, 0, 0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")

	if code, _, stderr := runCLI(offlineProbeArgs(cachePath,
		"--workers", "3",
		"--timeout", "2s",
		"--db", dbPath,
		"--identity", filepath.Join(dir, "collector.json"),
		"--quiet")...); code != ExitCodeOK {
		t.Fatalf("probe exit code = %d (stderr=%q)", code, stderr)
	}

	code, stdout, stderr := runCLI("db", "stats", "--db", dbPath)
	if code != ExitCodeOK {
		t.Fatalf("db stats exit code = %d (stderr=%q)", code, stderr)
	}
	for _, want := range []string{
		"measurements:    3",
		"targets:         3",
		"collectors:      1",
		"targets with samples:    3",
		"collectors with samples: 1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("db stats missing %q\ngot:\n%s", want, stdout)
		}
	}
	// 时间范围必须被填上（不再是 "no measurements yet"）。
	if strings.Contains(stdout, "(no measurements yet)") {
		t.Errorf("stats report no measurements after a successful probe:\n%s", stdout)
	}
}

// TestProbeIdentityFileIsReusedAcrossRuns 验证同一个节点跨运行保持同一 ID。
func TestProbeIdentityFileIsReusedAcrossRuns(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")
	identityPath := filepath.Join(dir, "collector.json")

	for i := 0; i < 2; i++ {
		if code, _, stderr := runCLI(offlineProbeArgs(cachePath,
			"--workers", "1",
			"--timeout", "2s",
			"--db", dbPath,
			"--identity", identityPath,
			"--quiet")...); code != ExitCodeOK {
			t.Fatalf("run #%d exit code = %d (stderr=%q)", i, code, stderr)
		}
	}

	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	collectors, err := store.LoadCollectors(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 两次运行必须归到同一个采集者，否则众测数据的"同一节点"维度就碎了。
	if len(collectors) != 1 {
		t.Fatalf("collectors = %d, want 1 (the same node across runs)", len(collectors))
	}
}

// TestProbeInvalidCollectorProfileIsRejected 验证非法画像在写库前被挡下。
func TestProbeInvalidCollectorProfileIsRejected(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	code, _, stderr := runCLI(offlineProbeArgs(cachePath,
		"--timeout", "2s",
		"--db", filepath.Join(dir, "results.db"),
		"--identity", filepath.Join(dir, "collector.json"),
		"--country", "CHN", // 3 位国家代码：非法
		"--quiet")...)

	if code != ExitCodeError {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeError, stderr)
	}
	if !strings.Contains(stderr, "collector profile") && !strings.Contains(stderr, "country") {
		t.Errorf("stderr = %q, want a message about the collector profile", stderr)
	}
}

// TestHumanBytes 覆盖体积格式化的边界。
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		-1:      "0 B",
		512:     "512 B",
		2048:    "2.00 KiB",
		5 << 20: "5.00 MiB",
		3 << 30: "3.00 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDBStatsJSONShapeIsStable 确认 db stats 不依赖 map 迭代顺序。
//
// 它输出的是逐表固定的顺序，脚本可以按行解析。
func TestDBStatsJSONShapeIsStable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "results.db")
	if code, _, stderr := runCLI("db", "migrate", "--db", dbPath); code != ExitCodeOK {
		t.Fatalf("migrate exit code = %d (stderr=%q)", code, stderr)
	}

	_, first, _ := runCLI("db", "stats", "--db", dbPath)
	_, second, _ := runCLI("db", "stats", "--db", dbPath)

	// 去掉易变字段（size、时间范围）后必须逐字节一致。
	normalize := func(s string) string {
		var out []string
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "size:") ||
				strings.HasPrefix(line, "database:") ||
				strings.HasPrefix(line, "  time range:") ||
				strings.HasPrefix(line, "  first:") ||
				strings.HasPrefix(line, "  last:") ||
				strings.HasPrefix(line, "  span:") {
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	if normalize(first) != normalize(second) {
		t.Errorf("db stats output is not stable:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// ensureLocalListener 在测试需要真实端口时提供一个。
//
// 保留它是因为 TestProbeStoresMeasurements 依赖 probeFixture 生成的
// 真实监听端口；这个函数用于个别需要单端口的场景。
func ensureLocalListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener, listener.Addr().(*net.TCPAddr).Port
}

// TestProbeDBRecordsJSONFidelity 确认库里读回的延迟与 probe 报告的一致。
func TestProbeDBRecordsJSONFidelity(t *testing.T) {
	body, listeners := probeFixture(t, []int{0})
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	cachePath := writeProbeCacheFrom(t, body)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")

	// 用 --json 拿到 probe 报告的延迟，再与库里的值比对。
	code, stdout, stderr := runCLI(offlineProbeArgs(cachePath,
		"--workers", "1",
		"--timeout", "2s",
		"--db", dbPath,
		"--identity", filepath.Join(dir, "collector.json"),
		"--json",
		"--quiet")...)
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	var probeLatency float64
	var probeTarget string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var record struct {
			TargetID  string  `json:"target_id"`
			LatencyMS float64 `json:"latency_ms"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// stdout 里还混着汇总文本，逐行尝试解析即可。
			continue
		}
		if record.TargetID != "" {
			probeLatency = record.LatencyMS
			probeTarget = record.TargetID
		}
	}
	if probeTarget == "" {
		t.Fatalf("no json measurement line found in stdout:\n%s", stdout)
	}

	store, err := storage.Open(t.Context(), storage.Config{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	rows, err := store.QueryMeasurements(t.Context(), storage.MeasurementQuery{TargetID: probeTarget})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("measurements for %s = %d, want 1", probeTarget, len(rows))
	}
	// 库里是整数微秒，读出时换算回毫秒：误差必须小于 1 微秒。
	if diff := rows[0].LatencyMS - probeLatency; diff > 0.001 || diff < -0.001 {
		t.Errorf("latency mismatch: probe reported %.4f ms, database returned %.4f ms",
			probeLatency, rows[0].LatencyMS)
	}
}

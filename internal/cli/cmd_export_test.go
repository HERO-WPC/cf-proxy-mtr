package cli

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// exportTestSessionID 是 exportTestDB 使用的会话 ID。
//
// 用常量而不是从 --list-sessions 的输出里解析：会话 ID 的格式
// （时间戳 + 随机后缀）由被测代码生成，用正则去猜它会让测试
// 与实现细节耦合，而且猜错时报的是"找不到会话"这种误导性错误。
const exportTestSessionID = "20260101T000000Z-00000000"

// exportTestDB 准备一个带数据的库并返回 (dbPath, identityPath)。
//
// 数据是**直接写入**的，不走真实扫描。原因是这里要验证的是导出链路
// （SQL、字段映射、隐私过滤、压缩），而目标必须是**公网地址**——
// 用本机监听当目标会被隐私过滤器正确地丢掉（那条路径另有测试覆盖），
// 于是"导出测试"反而什么都测不到。
func exportTestDB(t *testing.T, targetCount int) (string, string) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "results.db")
	identityPath := filepath.Join(dir, "collector.json")

	// 身份文件：带一份完整画像，这样导出里 collector 分组字段非空。
	local, err := identity.Load(identityPath)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}
	local.Profile = identity.Profile{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: "ipv4",
	}
	if err := identity.Save(identityPath, local); err != nil {
		t.Fatalf("identity.Save: %v", err)
	}

	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	collectorPK, err := store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), "0.1.0")
	if err != nil {
		t.Fatalf("UpsertCollector: %v", err)
	}

	sessionID := "20260101T000000Z-00000000"
	if err := store.DefineSession(ctx, sessionID, collectorPK, targetCount, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatalf("DefineSession: %v", err)
	}

	// 公网测试地址（文档用途之外的普通公网段）。
	publicIPs := []string{
		"45.63.67.144", "104.16.0.1", "1.1.1.1", "8.8.8.8",
		"159.60.146.81", "64.177.112.4", "108.61.101.77", "216.128.158.164",
	}

	for i := 0; i < targetCount; i++ {
		ip := publicIPs[i%len(publicIPs)]
		port := 443 + i

		target, err := model.NewTargetFromStrings(ip, port)
		if err != nil {
			t.Fatalf("target %d: %v", i, err)
		}
		target.Location = model.Location{
			Country: "US", CCA2: "US", Region: "Illinois", City: "Chicago",
			Latitude: 41.85003, Longitude: -87.65005, HasCoordinates: true,
			CountryEN: "United States",
		}
		target.Colo = model.ColoInfo{
			IATA: "ORD", CCA2: "US", City: "Chicago",
			Latitude: 41.9786, Longitude: -87.9048, HasCoordinates: true,
		}

		if _, err := store.UpsertTargets(ctx, []model.Target{target}, time.Now().UTC()); err != nil {
			t.Fatalf("UpsertTargets: %v", err)
		}

		// 一半成功一半失败：两条路径都要被导出。
		success := i%3 != 0
		result := probe.ProbeResult{
			TargetID:  target.ID,
			IP:        target.IP,
			Port:      target.Port,
			Success:   success,
			LatencyMS: 42.5 + float64(i),
			Timestamp: time.Now().UTC(),
		}
		if !success {
			result.ErrorType = probe.ErrorTypeTimeout
			result.ErrorMessage = "dial tcp4 45.63.67.144:2053: i/o timeout"
		}

		if _, _, err := store.SaveMeasurements(ctx, []storage.Measurement{
			storage.NewMeasurement(collectorPK, sessionID, result),
		}); err != nil {
			t.Fatalf("SaveMeasurements: %v", err)
		}

		// 为成功的目标写一条跟踪，覆盖 trace 导出路径。
		if success {
			traceRecord := storage.Trace{
				TargetID: target.ID, CollectorID: collectorPK, SessionID: sessionID,
				Timestamp: time.Now().UTC(), Engine: "nexttrace", EngineVersion: "1.7.3",
				Mode: "tcp", Protocol: "tcp", Port: target.Port,
				Success: true, DurationMS: 1234.5, HopCount: 3,
				// 故意含内网跳：导出时必须被替换成占位符。
				TraceJSON: `[{"TTL":1,"IP":"192.168.1.1","RTTMS":[1.2],"Timeout":false},` +
					`{"TTL":2,"IP":"10.0.0.1","RTTMS":[2.4],"Timeout":false},` +
					`{"TTL":3,"IP":"45.63.67.144","RTTMS":[42.5],"Timeout":false,"ASN":"AS20473"}]`,
			}
			if _, _, err := store.SaveTraces(ctx, []storage.Trace{traceRecord}); err != nil {
				t.Fatalf("SaveTraces: %v", err)
			}
		}
	}

	return dbPath, identityPath
}

func TestExportHelpExplainsPrivacyAndFormats(t *testing.T) {
	code, stdout, stderr := runCLI("export", "--help")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	for _, want := range []string{
		"-db",
		"-session",
		"-format",
		"-out",
		"-dry-run",
		"-list-sessions",
		"Formats:",
		"jsonl",
		"jsonl.gz",
		"zstd",
		"NOT available",
		// 隐私说明必须在帮助里，而不是只写在注释中。
		"Privacy:",
		"private-v4",
		"内网",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("export help missing %q\ngot:\n%s", want, stdout)
		}
	}
}

func TestExportRequiresDB(t *testing.T) {
	code, _, stderr := runCLI("export", "--quiet")
	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	if !strings.Contains(stderr, "--db is required") {
		t.Errorf("stderr = %q, want it to demand --db", stderr)
	}
}

// TestExportListSessions 验证会话列举。
func TestExportListSessions(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 4)

	code, stdout, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath, "--list-sessions")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	if !strings.Contains(stdout, "sessions in") {
		t.Errorf("stdout missing the header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "SESSION") || !strings.Contains(stdout, "MEASURED") {
		t.Errorf("stdout missing the table header:\n%s", stdout)
	}
	// 会话 ID 形如 20260101T000000Z-00000000，必须出现在列表里。
	if !strings.Contains(stdout, "measured") && !strings.Contains(stdout, "2026") {
		t.Errorf("stdout does not look like it lists a session:\n%s", stdout)
	}
}

// TestExportJSONPath 是端到端的主路径：导出 JSONL 并检查内容。
func TestExportJSONPath(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 6)
	dir := filepath.Dir(dbPath)
	outPath := filepath.Join(dir, "out.jsonl")

	code, stdout, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--format", "jsonl", "--out", outPath, "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "total exported:") {
		t.Errorf("stdout missing the summary:\n%s", stdout)
	}
	if !strings.Contains(stdout, "privacy filtering:") {
		t.Errorf("stdout must report privacy filtering:\n%s", stdout)
	}

	blob, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(blob), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("output is empty")
	}

	for i, line := range lines {
		var row struct {
			SchemaVersion int    `json:"schema_version"`
			Kind          string `json:"kind"`
			ClientVersion string `json:"client_version"`
			CollectorID   string `json:"collector_id"`
			TargetID      string `json:"target_id"`
			IP            string `json:"ip"`
			Port          int    `json:"port"`
			TimestampUTC  string `json:"timestamp_utc"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if row.SchemaVersion == 0 {
			t.Errorf("line %d has no schema_version", i)
		}
		if row.Kind != "measurement" && row.Kind != "trace" {
			t.Errorf("line %d has unknown kind %q", i, row.Kind)
		}
		if row.ClientVersion == "" {
			t.Errorf("line %d has no client_version", i)
		}
		if row.CollectorID == "" {
			t.Errorf("line %d has no collector_id; the data cannot be attributed", i)
		}
		if row.IP == "" || row.Port == 0 {
			t.Errorf("line %d has no target identity", i)
		}
		if row.TimestampUTC == "" {
			t.Errorf("line %d has no timestamp", i)
		}
	}
}

// TestExportOutFlagIsRequiredToStayUnique 验证不静默覆盖已有文件。
func TestExportRefusesToOverwrite(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 3)
	outPath := filepath.Join(filepath.Dir(dbPath), "out.jsonl")

	if code, _, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--format", "jsonl", "--out", outPath, "--quiet"); code != ExitCodeOK {
		t.Fatalf("first export failed: %d (%s)", code, stderr)
	}

	code, _, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--format", "jsonl", "--out", outPath, "--quiet")
	if code == ExitCodeOK {
		t.Fatal("export overwrote an existing file without complaint")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want an 'already exists' message", stderr)
	}
	// 必须给出解决办法，而不是只说不行。
	if !strings.Contains(stderr, "--out") {
		t.Errorf("stderr = %q, want it to suggest --out", stderr)
	}
}

// TestExportGzipIsValidArchive 验证 gzip 产物可以被解开。
func TestExportGzipIsValidArchive(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 4)
	outPath := filepath.Join(filepath.Dir(dbPath), "out.jsonl.gz")

	code, _, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--out", outPath, "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	file, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("output is not a valid gzip stream: %v", err)
	}
	defer func() { _ = reader.Close() }()

	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if len(plain) == 0 {
		t.Fatal("decompressed output is empty")
	}
	if !strings.HasPrefix(string(plain), "{") {
		t.Errorf("decompressed output does not start with JSON: %q", string(plain[:minInt(80, len(plain))]))
	}
}

// TestExportZstdIsRefusedWithReason 验证 zstd 被明确拒绝并解释原因。
func TestExportZstdIsRefusedWithReason(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 2)

	code, _, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--format", "zstd", "--out", filepath.Join(filepath.Dir(dbPath), "x.jsonl.zst"))

	if code != ExitCodeUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
	}
	// 必须说明"为什么"以及"该用什么"。
	if !strings.Contains(stderr, "Go 1.26") {
		t.Errorf("stderr = %q, want the actual cause", stderr)
	}
	if !strings.Contains(stderr, "jsonl.gz") {
		t.Errorf("stderr = %q, want a suggested alternative", stderr)
	}
	// 不应该留下半个文件。
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "x.jsonl.zst")); err == nil {
		t.Error("a file was created even though the format is unsupported")
	}
}

// TestExportDryRunWritesNothing 验证 dry-run 不产生文件。
func TestExportDryRunWritesNothing(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 3)
	dir := filepath.Dir(dbPath)

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--dry-run", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("stdout should say it is a dry run:\n%s", stdout)
	}
	if !strings.Contains(stdout, "total exported:") {
		t.Errorf("stdout should still report the counts:\n%s", stdout)
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("dry run created files: %d -> %d", len(before), len(after))
	}
}

// TestExportSessionFilter 验证按会话过滤真的生效。
func TestExportSessionFilter(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 4)

	t.Run("bogus session yields nothing and says so", func(t *testing.T) {
		code, stdout, stderr := runCLI("export",
			"--db", dbPath, "--identity", identityPath,
			// 这个 ID 必须**不存在于**数据库里：测试的是"筛选出空结果"。
			// 不要改成 exportTestSessionID 那个值，否则空结果变成有结果。
			"--session", "20260101T000000Z-00000001", "--dry-run", "--quiet")
		if code != ExitCodeOK {
			t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
		}
		if !strings.Contains(stdout, "total exported: 0") {
			t.Errorf("stdout should report zero rows:\n%s", stdout)
		}
		// 空结果必须被明确提示，而不是安静地写出一个空文件。
		if !strings.Contains(stdout, "nothing to export") {
			t.Errorf("stdout should warn that nothing matched:\n%s", stdout)
		}
		if !strings.Contains(stdout, "--list-sessions") {
			t.Errorf("stdout should point at --list-sessions:\n%s", stdout)
		}
	})

	t.Run("real session exports its rows", func(t *testing.T) {
		// 先确认 list-sessions 里能看到它。
		code, listing, stderr := runCLI("export",
			"--db", dbPath, "--identity", identityPath, "--list-sessions")
		if code != ExitCodeOK {
			t.Fatalf("list-sessions failed: %d (%s)", code, stderr)
		}
		if !strings.Contains(listing, exportTestSessionID) {
			t.Fatalf("list-sessions does not mention %s:\n%s", exportTestSessionID, listing)
		}

		code, stdout, stderr := runCLI("export",
			"--db", dbPath, "--identity", identityPath,
			"--session", exportTestSessionID, "--dry-run", "--quiet")
		if code != ExitCodeOK {
			t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
		}
		if strings.Contains(stdout, "total exported: 0") {
			t.Errorf("session %s exported nothing:\n%s", exportTestSessionID, stdout)
		}
	})
}

// TestExportKindFilter 验证 --kind 只导出指定种类。
func TestExportKindFilter(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 4)
	dir := filepath.Dir(dbPath)

	t.Run("measurements only", func(t *testing.T) {
		outPath := filepath.Join(dir, "m.jsonl")
		code, stdout, stderr := runCLI("export",
			"--db", dbPath, "--identity", identityPath,
			"--kind", "measurements", "--format", "jsonl", "--out", outPath, "--quiet")
		if code != ExitCodeOK {
			t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
		}
		if strings.Contains(stdout, "traces:") {
			t.Errorf("stdout mentions traces even though only measurements were requested:\n%s", stdout)
		}

		blob, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), `"kind":"trace"`) {
			t.Error("trace rows leaked into a measurements-only export")
		}
		if !strings.Contains(string(blob), `"kind":"measurement"`) {
			t.Error("no measurement rows in a measurements-only export")
		}
	})

	t.Run("unknown kind is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI("export",
			"--db", dbPath, "--identity", identityPath,
			"--kind", "nonsense", "--dry-run")
		if code != ExitCodeUsage {
			t.Fatalf("exit code = %d, want %d", code, ExitCodeUsage)
		}
		if !strings.Contains(stderr, "measurements") {
			t.Errorf("stderr = %q, want the valid kinds listed", stderr)
		}
	})
}

// TestExportTimeRangeIsInclusiveOfTheWholeDay 验证纯日期的 --until 语义。
//
// "--since 2026-10-03 --until 2026-10-03" 必须能包含当天数据，
// 否则用户会得到一个空结果却不知道为什么。
func TestExportTimeRangeIsInclusiveOfTheWholeDay(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 3)

	// 用今天作为范围（数据是刚刚生成的）。
	today := time.Now().UTC().Format("2006-01-02")

	code, stdout, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--since", today, "--until", today, "--dry-run", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if strings.Contains(stdout, "total exported: 0") {
		t.Errorf("same-day range exported nothing; --until must cover the whole day:\n%s", stdout)
	}

	// 未来的范围必须为空。
	code, stdout, stderr = runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--since", "2099-01-01", "--dry-run", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stdout, "total exported: 0") {
		t.Errorf("future range exported rows:\n%s", stdout)
	}
}

func TestExportRejectsBadTimeAndRange(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 2)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad since", []string{"--since", "yesterday"}, "--since"},
		{"bad until", []string{"--until", "13/13/2026"}, "--until"},
		{"until before since", []string{"--since", "2026-10-03", "--until", "2026-10-01"}, "before"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"export", "--db", dbPath, "--identity", identityPath, "--dry-run"}, tc.args...)
			code, _, stderr := runCLI(args...)
			if code != ExitCodeUsage {
				t.Fatalf("exit code = %d, want %d (stderr=%q)", code, ExitCodeUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

// TestExportStdoutMode 验证 --out - 把数据写到 stdout。
func TestExportStdoutMode(t *testing.T) {
	dbPath, identityPath := exportTestDB(t, 3)

	code, stdout, stderr := runCLI("export",
		"--db", dbPath, "--identity", identityPath,
		"--format", "jsonl", "--out", "-", "--quiet")
	if code != ExitCodeOK {
		t.Fatalf("exit code = %d (stderr=%q)", code, stderr)
	}

	// stdout 上应当是纯 JSONL（没有汇总混进去），
	// 否则管道里的 jq 之类会解析失败。
	for i, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			t.Fatalf("stdout line %d is not JSON (summary leaked into the stream): %q", i, line)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("stdout line %d is not valid JSON: %v", i, err)
		}
	}

	// 汇总必须改道 stderr，否则会把数据流弄脏。
	if !strings.Contains(stderr, "total exported:") {
		t.Errorf("summary did not go to stderr:\n%s", stderr)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

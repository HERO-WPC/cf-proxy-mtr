package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// listenLocal 起一个真实的本机监听，返回端口。
//
// 用真实监听而不是 mock：探测层的行为（连接被接受、被拒绝、超时）
// 只有真连接才能如实复现，而 mock 会把"探测写错了"测试成通过。
func listenLocal(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	// 持续接受连接并立刻关闭：只测握手，不测数据传输。
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	return listener.Addr().(*net.TCPAddr).Port
}

// testService 构造一个完全离线、指向临时目录的服务。
func testService(t *testing.T, cachePath string) *Service {
	t.Helper()

	dir := t.TempDir()
	return New(Options{
		DBPath:       filepath.Join(dir, "results.db"),
		IdentityPath: filepath.Join(dir, "collector.json"),
		// 这个夹具是**完全离线**的：它给一份固定缓存、并断言固定目标。
		// 默认的"网络优先"会去下载真实列表，于是测试变成依赖外网。
		SourceCacheFirst: true,
		Source: source.Config{
			URL:         source.DefaultURL,
			FallbackURL: "",
			CachePath:   cachePath,
			Retries:     0,
		},
	})
}

// writeCache 写一份合法缓存，其中每个 IP:Port 都是本机监听。
//
// 缓存的 source url 必须与 Service 的主数据源一致，
// 否则 Loader 会判定"缓存属于另一个源"并转去联网——
// 那会让离线测试悄悄变成真实网络请求。
func writeCache(t *testing.T, ports ...int) string {
	t.Helper()

	data := make([]string, 0, len(ports))
	for _, port := range ports {
		data = append(data, fmt.Sprintf(
			`{"ip":"127.0.0.1","port":[%d],"latitude":"0","longitude":"0","country":"CN","city":"Hangzhou"}`,
			port))
	}

	body := fmt.Sprintf(
		`{"generated_at":"2026-10-03T00:00:00","list":{"ips":%d},"data":[%s]}`,
		len(ports), strings.Join(data, ","))

	parsed, err := source.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON fixture: %v", err)
	}
	if len(parsed.Targets) != len(ports) {
		t.Fatalf("fixture produced %d targets, want %d", len(parsed.Targets), len(ports))
	}
	if err := model.ValidateAll(parsed.Targets); err != nil {
		t.Fatalf("fixture produced an invalid target: %v", err)
	}

	meta := parsed.Meta
	meta.URL = source.DefaultURL
	meta.Format = "json"

	path := filepath.Join(t.TempDir(), "all.json")
	if err := source.WriteCache(path, meta, parsed.Stats, parsed.Targets); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// 基本扫描
// ---------------------------------------------------------------------------

// TestRunScanMeasuresAndStores 验证一次扫描真的测量并落库。
func TestRunScanMeasuresAndStores(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunScan(ctx, ScanOptions{Workers: 2, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	if result.TargetsConsidered != 1 {
		t.Errorf("TargetsConsidered = %d, want 1", result.TargetsConsidered)
	}
	if result.Probe.Success != 1 {
		t.Errorf("Probe.Success = %d, want 1 (a real listener is accepting)", result.Probe.Success)
	}
	if result.Stored != 1 {
		t.Errorf("Stored = %d, want 1", result.Stored)
	}
	if result.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if !result.SessionFinished {
		t.Error("SessionFinished = false, want true after a complete scan")
	}

	// 落库必须真的发生（而不是只返回了统计）。
	stats, err := svc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got := stats.Storage.Tables["measurements"]; got != 1 {
		t.Errorf("measurements in DB = %d, want 1", got)
	}
	if got := stats.Storage.Tables["targets"]; got != 1 {
		t.Errorf("targets in DB = %d, want 1", got)
	}
	if stats.LatestSession == nil {
		t.Error("LatestSession is nil, want the session we just ran")
	}
}

// TestRunScanReportsSourceOrigin 验证如实报告目标列表的来源。
func TestRunScanReportsSourceOrigin(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunScan(ctx, ScanOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	if !result.SourceFromCache {
		t.Error("SourceFromCache = false, want true (the fixture is a local cache)")
	}
	if result.SourceURL != source.DefaultURL {
		t.Errorf("SourceURL = %q, want %q", result.SourceURL, source.DefaultURL)
	}
	if result.SourceGeneratedAt.IsZero() {
		t.Error("SourceGeneratedAt is zero; the generator time should be reported")
	}
}

// TestRunScanHonorsLimit 验证 --limit 取源顺序的前 N 个。
//
// 刻意不是随机抽样：同一个 limit 在不同机器上应当测同一批目标，
// 否则结果无法互相比较。
func TestRunScanHonorsLimit(t *testing.T) {
	p1, p2, p3 := listenLocal(t), listenLocal(t), listenLocal(t)
	svc := testService(t, writeCache(t, p1, p2, p3))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunScan(ctx, ScanOptions{Limit: 2, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if result.TargetsConsidered != 2 {
		t.Errorf("TargetsConsidered = %d, want 2", result.TargetsConsidered)
	}

	stats, err := svc.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.Storage.Tables["targets"]; got != 2 {
		t.Errorf("targets in DB = %d, want 2 (only the first two)", got)
	}

	// 确认取的是**前两个**（p1、p2），而不是任意两个。
	store, err := storage.Open(ctx, storage.Config{Path: svc.Options().DBPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	seen := map[int]bool{}
	if _, err := store.StreamMeasurements(ctx, storage.ExportFilter{},
		func(row storage.ExportMeasurement) error {
			seen[row.Port] = true
			return nil
		}); err != nil {
		t.Fatalf("StreamMeasurements: %v", err)
	}
	if !seen[p1] || !seen[p2] {
		t.Errorf("measured ports = %v, want the first two (%d, %d)", seen, p1, p2)
	}
	if seen[p3] {
		t.Errorf("port %d was measured despite --limit 2", p3)
	}
}

// ---------------------------------------------------------------------------
// 参数校验必须发生在副作用之前
// ---------------------------------------------------------------------------

// TestRunScanValidatesTraceModeBeforeSideEffects 验证写错跟踪模式时
// **不会**先下载目标、开数据库、建会话。
//
// 这条规则是踩过坑之后立的：早先的实现把校验放在后面，
// 用户写错 --trace-mode 时会先经历完整的前置流程才看到错误，
// 而且当目标列表为空时他根本看不到这条错误，
// 只会去排查"为什么没有目标"这个完全无关的方向。
func TestRunScanValidatesTraceModeBeforeSideEffects(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))
	dbPath := svc.Options().DBPath

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := svc.RunScan(ctx, ScanOptions{
		Trace:       true,
		TraceConfig: TraceOptions{Mode: "gre"},
		Timeout:     2 * time.Second,
	})

	if err == nil {
		t.Fatal("RunScan accepted an invalid trace mode")
	}
	if !IsUsage(err) {
		t.Errorf("error = %v, want it classified as a usage error", err)
	}
	if !strings.Contains(err.Error(), "trace mode") {
		t.Errorf("error = %v, want it to mention the trace mode", err)
	}

	// 数据库文件不该被创建：校验发生在开库之前。
	if fileExists(dbPath) {
		t.Error("the database was created before the trace mode was validated")
	}
}

// TestRunScanRejectsConflictingSessionFlags 验证冲突参数明确报错。
func TestRunScanRejectsConflictingSessionFlags(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := svc.RunScan(ctx, ScanOptions{
		Resume:     true,
		NewSession: true,
		Timeout:    2 * time.Second,
	})
	if err == nil {
		t.Fatal("RunScan accepted --resume together with --new")
	}
	if !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
}

// TestRunScanRejectsNegativeLimit 验证负的 limit 被拒绝。
func TestRunScanRejectsNegativeLimit(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.RunScan(ctx, ScanOptions{Limit: -1}); !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
}

// TestRunScanRequiresDatabase 验证没配数据库时明确报错。
//
// 扫描结果必须落库，因此这是硬依赖，不能静默跳过。
func TestRunScanRequiresDatabase(t *testing.T) {
	port := listenLocal(t)
	cachePath := writeCache(t, port)

	svc := New(Options{
		DBPath:       "",
		IdentityPath: filepath.Join(t.TempDir(), "c.json"),
		Source:       source.Config{URL: source.DefaultURL, FallbackURL: "", CachePath: cachePath},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.RunScan(ctx, ScanOptions{}); !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
}

// TestRunScanEmptyTargetList 验证空目标列表被识别为 ErrNoTargets。
func TestRunScanEmptyTargetList(t *testing.T) {
	empty := `{"generated_at":"2026-10-03T00:00:00","list":{"ips":0},"data":[]}`
	parsed, err := source.ParseJSON([]byte(empty))
	if err != nil {
		t.Fatal(err)
	}
	meta := parsed.Meta
	meta.URL = source.DefaultURL
	meta.Format = "json"

	cachePath := filepath.Join(t.TempDir(), "all.json")
	if err := source.WriteCache(cachePath, meta, parsed.Stats, parsed.Targets); err != nil {
		t.Fatal(err)
	}

	svc := testService(t, cachePath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = svc.RunScan(ctx, ScanOptions{})
	if !IsNoTargets(err) {
		t.Errorf("error = %v, want it classified as no-targets", err)
	}
	if err != nil && !strings.Contains(err.Error(), "no targets") {
		t.Errorf("error = %v, want a clear message", err)
	}
}

// ---------------------------------------------------------------------------
// 会话与续测
// ---------------------------------------------------------------------------

// TestRunScanRecordsLastSession 验证扫描后记住了会话，供下次续测使用。
func TestRunScanRecordsLastSession(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunScan(ctx, ScanOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	if !result.LastSessionSaved {
		t.Fatalf("LastSessionSaved = false (error: %q)", result.LastSessionError)
	}

	local, err := identity.Load(svc.Options().IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	if local.LastSessionID != result.SessionID {
		t.Errorf("recorded session = %q, want %q", local.LastSessionID, result.SessionID)
	}
}

// TestRunScanResumeMeasuresOnlyPending 验证续测只测未完成的目标。
//
// 这是整个工具最核心的保证之一：中断之后继续，不重复测量。
func TestRunScanResumeMeasuresOnlyPending(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 第一次：完整扫描。
	first, err := svc.RunScan(ctx, ScanOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("first RunScan: %v", err)
	}

	// 第二次：续测同一个会话。
	second, err := svc.RunScan(ctx, ScanOptions{
		Resume:  true,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("resume RunScan: %v", err)
	}

	if second.SessionID != first.SessionID {
		t.Errorf("resumed session = %q, want the same as the first (%q)", second.SessionID, first.SessionID)
	}
	if !second.Resumed {
		t.Error("Resumed = false, want true")
	}
	if second.TargetsPending != 0 {
		t.Errorf("TargetsPending = %d, want 0 (everything was already measured)", second.TargetsPending)
	}

	// 关键：续测**不能**产生重复的测量行。
	stats, err := svc.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.Storage.Tables["measurements"]; got != 1 {
		t.Errorf("measurements = %d, want 1 (resume must not duplicate)", got)
	}
}

// TestRunScanResumeWithoutHistory 验证没有可续测会话时给出明确错误，
// 而不是悄悄开一个新会话。
//
// 静默开新会话会让用户以为自己在补测，实际却从零开始。
func TestRunScanResumeWithoutHistory(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := svc.RunScan(ctx, ScanOptions{Resume: true, Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("resume succeeded without any previous session")
	}
	// 允许两种合理来源：身份文件里没有，或数据库里没有未结束的会话。
	if !IsNoSession(err) && !strings.Contains(err.Error(), "no session") {
		t.Errorf("error = %v, want it to explain that there is nothing to resume", err)
	}
}

// TestRunScanExplicitSessionImpliesResume 验证指定 --session 即视为续测。
func TestRunScanExplicitSessionImpliesResume(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 先跑一次拿到一个会话 ID。
	first, err := svc.RunScan(ctx, ScanOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// 显式指定该 ID（不带 Resume）：应当被理解为续测它。
	second, err := svc.RunScan(ctx, ScanOptions{
		SessionID: first.SessionID,
		Timeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunScan with an explicit session: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("session = %q, want %q", second.SessionID, first.SessionID)
	}
	if !second.Resumed {
		t.Error("Resumed = false; an explicit session ID should imply resuming it")
	}
}

// ---------------------------------------------------------------------------
// 进度回调
// ---------------------------------------------------------------------------

// TestRunScanReportsProgress 验证进度回调真的被调用。
//
// 图形界面完全依赖这条通路显示进度；它断了不会报错，
// 只会让界面一直停在 0%。
func TestRunScanReportsProgress(t *testing.T) {
	p1, p2, p3 := listenLocal(t), listenLocal(t), listenLocal(t)
	svc := testService(t, writeCache(t, p1, p2, p3))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var events []scheduler.ProgressEvent
	result, err := svc.RunScan(ctx, ScanOptions{
		Timeout: 2 * time.Second,
		Progress: func(event scheduler.ProgressEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	if len(events) == 0 {
		t.Fatal("no progress events were delivered")
	}

	// ProgressEvent 没有 Final 字段：一个阶段"跑完"的信号是
	// Completed == Total。界面必须据此判断完成，因此这里锁住这个约定，
	// 避免哪天调度器改了语义而界面静默失效（界面不会报错，
	// 只会永远停在某个百分比）。
	reachedEnd := false
	for _, event := range events {
		if event.Total > 0 && event.Completed == event.Total {
			reachedEnd = true
			break
		}
	}
	if !reachedEnd {
		t.Errorf("no progress event reported Completed == Total; "+
			"a UI keyed on that signal would stay stuck. events=%+v", events)
	}

	// 进度数字必须自洽，否则界面会显示"成功+失败 > 已完成"这种自相矛盾的进度。
	for _, event := range events {
		if event.Total > 0 && event.Completed > event.Total {
			t.Errorf("event reports %d/%d completed, more than the total", event.Completed, event.Total)
		}
		if event.Success+event.Failed > event.Completed {
			t.Errorf("event reports success=%d failed=%d but only %d completed",
				event.Success, event.Failed, event.Completed)
		}
	}

	last := events[len(events)-1]
	if last.Completed > result.TargetsConsidered {
		t.Errorf("progress reported %d completed, more than the %d targets considered",
			last.Completed, result.TargetsConsidered)
	}
}

// ---------------------------------------------------------------------------
// 跟踪引擎不可用
// ---------------------------------------------------------------------------

// TestRunScanWithoutTraceEngineStillMeasures 验证跟踪引擎不可用时
// TCP 测量照常完成，并如实报告原因。
//
// "用户没装 NextTrace"是最常见的情况之一，不该让整个扫描白跑；
// 但也绝不能静默跳过——那会让汇总看起来像跟踪过了。
func TestRunScanWithoutTraceEngineStillMeasures(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := svc.RunScan(ctx, ScanOptions{
		Trace:       true,
		TraceConfig: TraceOptions{Binary: "definitely-not-installed-nexttrace-xyz"},
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	if result.Probe.Success != 1 {
		t.Errorf("Probe.Success = %d, want 1 (TCP measurement must still happen)", result.Probe.Success)
	}
	if result.TraceUnavailable == "" {
		t.Error("TraceUnavailable is empty; the UI could not explain why tracing did not happen")
	}
	if !result.TraceSkipped {
		t.Error("TraceSkipped = false, want true when the engine is unavailable")
	}
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// TestStatsOnEmptyDatabase 验证空数据库也能给出统计（而不是报错）。
func TestStatsOnEmptyDatabase(t *testing.T) {
	svc := testService(t, writeCache(t, listenLocal(t)))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 尚未扫描：数据库文件还不存在，Stats 应当把它建出来并返回全零。
	stats, err := svc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats on a fresh database: %v", err)
	}
	if stats.Storage.SchemaVersion == 0 {
		t.Error("SchemaVersion = 0, want the migrated version")
	}
	if stats.Storage.Tables["measurements"] != 0 {
		t.Errorf("measurements = %d, want 0", stats.Storage.Tables["measurements"])
	}
	if stats.LatestSession != nil {
		t.Errorf("LatestSession = %+v, want nil when no scan has run", stats.LatestSession)
	}
}

// TestStatsRequiresDatabase 验证没配数据库时明确报错。
func TestStatsRequiresDatabase(t *testing.T) {
	svc := New(Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Stats(ctx); !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}
}

// ---------------------------------------------------------------------------
// 画像覆盖
// ---------------------------------------------------------------------------

// TestRunScanAppliesCollectorOverrides 验证手动给的画像优先于本地文件。
//
// 手动值优先是刻意的：公网 ASN 不一定等于用户实际感知的接入线路。
func TestRunScanAppliesCollectorOverrides(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := svc.RunScan(ctx, ScanOptions{
		Timeout: 2 * time.Second,
		Collector: model.CollectorProfile{
			// 小写是合理输入，必须被归一化成大写而不是被拒绝。
			Country: "cn",
			City:    "Hangzhou",
			ISP:     "China Mobile",
			ASN:     "AS9808",
		},
	})
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	local, err := identity.Load(svc.Options().IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := local.Profile.ToModel()
	if profile.Country != "CN" {
		t.Errorf("country = %q, want CN (lowercase input must be normalized, not rejected)", profile.Country)
	}
	if profile.City != "Hangzhou" {
		t.Errorf("city = %q, want Hangzhou", profile.City)
	}
	if profile.ASN != "AS9808" {
		t.Errorf("asn = %q, want AS9808", profile.ASN)
	}
}

// TestRunScanRejectsInvalidCollectorOverride 验证非法画像被挡住，
// 而不是写进标识文件污染后续所有数据。
func TestRunScanRejectsInvalidCollectorOverride(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := svc.RunScan(ctx, ScanOptions{
		Timeout:   2 * time.Second,
		Collector: model.CollectorProfile{Country: "CHN"}, // 3 位国家代码
	})
	if err == nil {
		t.Fatal("RunScan accepted a 3-letter country code")
	}
	if !IsUsage(err) {
		t.Errorf("error = %v, want a usage error", err)
	}

	// 标识文件不该被写入非法值。
	if local, loadErr := identity.Load(svc.Options().IdentityPath); loadErr == nil {
		if local.Profile.Country == "CHN" {
			t.Error("the invalid country code was written to the identity file")
		}
	}
}

// fileExists 报告路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// logCollector 收集日志行（便于断言服务层记录了关键事件）。
type logCollector struct {
	lines []string
}

func (c *logCollector) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCollector) contains(substr string) bool {
	for _, line := range c.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

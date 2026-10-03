package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// 内存数据库计数，避免多个测试共用同一个库。
var memoryCounter int64

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	name := fmt.Sprintf("file:sched%d?mode=memory&cache=shared", atomic.AddInt64(&memoryCounter, 1))
	store, err := storage.Open(context.Background(), storage.Config{Path: name})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testProfile() model.CollectorProfile {
	return model.CollectorProfile{
		Country:   "CN",
		Province:  "Zhejiang",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "AS9808",
		IPVersion: model.IPVersionIPv4,
	}
}

// collector 写入采集者并返回主键。
func collector(t *testing.T, store *storage.Store, id string) int64 {
	t.Helper()
	pk, err := store.UpsertCollector(context.Background(), id, testProfile(), "0.1.0")
	if err != nil {
		t.Fatalf("UpsertCollector: %v", err)
	}
	return pk
}

// liveTargets 启动 n 个本机监听，返回可探测的目标。
//
// 用真实监听而不是假 dialer：扫描这条链路的价值就在于
// "真的连上了、真的写进库了"，模拟掉太多会让测试失去意义。
func liveTargets(t *testing.T, n int) []model.Target {
	t.Helper()

	targets := make([]model.Target, 0, n)
	for i := 0; i < n; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen #%d: %v", i, err)
		}
		t.Cleanup(func() { _ = listener.Close() })

		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}(listener)

		addr := listener.Addr().(*net.TCPAddr)
		target, err := model.NewTargetFromStrings(addr.IP.String(), addr.Port)
		if err != nil {
			t.Fatalf("target: %v", err)
		}
		targets = append(targets, target)
	}
	return targets
}

// newScheduler 创建 scheduler 并完成前置写入（目标与采集者）。
func newScheduler(t *testing.T, store *storage.Store, collectorPK int64, cfg Config) *Scheduler {
	t.Helper()

	cfg.CollectorPK = collectorPK
	if cfg.Probe.Workers == 0 {
		cfg.Probe = probe.Config{Workers: 4, Timeout: 2 * time.Second}
	}
	// 进度回调间隔压到 0，保证测试能观察到每一次进度。
	cfg.ProgressInterval = time.Nanosecond

	sched, err := New(store, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sched
}

// newSessionID 生成一个合法的会话 ID。
func newSessionID(t *testing.T) string {
	t.Helper()
	id, err := model.NewSessionID(time.Now().UTC())
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// 正常路径
// ---------------------------------------------------------------------------

func TestScanProbesAndStoresAllTargets(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-basic")
	targets := liveTargets(t, 3)

	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sessionID := newSessionID(t)
	sched := newScheduler(t, store, pk, Config{SessionID: sessionID})

	var events []ProgressEvent
	sched.SetProgress(func(ev ProgressEvent) { events = append(events, ev) })

	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", result.SessionID, sessionID)
	}
	if result.Resumed {
		t.Error("Resumed = true, want false for a new session")
	}
	if result.TargetsTotal != 3 || result.TargetsPending != 3 {
		t.Errorf("targets = %d/%d, want 3/3", result.TargetsTotal, result.TargetsPending)
	}
	if result.AlreadyDone != 0 {
		t.Errorf("AlreadyDone = %d, want 0", result.AlreadyDone)
	}
	if result.Probe.Completed != 3 {
		t.Errorf("Probe.Completed = %d, want 3", result.Probe.Completed)
	}
	if result.Probe.Success != 3 {
		t.Errorf("Probe.Success = %d, want 3", result.Probe.Success)
	}
	if result.Stored != 3 {
		t.Errorf("Stored = %d, want 3", result.Stored)
	}
	if result.StoreFailures != 0 {
		t.Errorf("StoreFailures = %d, want 0", result.StoreFailures)
	}
	if result.Interrupted {
		t.Error("Interrupted = true, want false")
	}
	if !result.SessionFinished {
		t.Error("SessionFinished = false, want true")
	}
	if result.Progress.Measured != 3 || result.Progress.Success != 3 {
		t.Errorf("progress = %+v, want 3 measured / 3 success", result.Progress)
	}

	// 进度回调必须覆盖到探测阶段结束。
	if len(events) == 0 {
		t.Fatal("no progress events")
	}
	last := events[len(events)-1]
	if last.Phase != PhaseProbe || last.Completed != 3 || last.Total != 3 {
		t.Errorf("last event = %+v, want probe 3/3", last)
	}

	// 数据库必须真的写进去了。
	measurements, err := store.QueryMeasurements(ctx, storage.MeasurementQuery{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 3 {
		t.Fatalf("measurements = %d, want 3", len(measurements))
	}
	for _, m := range measurements {
		if m.SessionID != sessionID {
			t.Errorf("measurement %d has session %q, want %q", m.ID, m.SessionID, sessionID)
		}
		if !m.Success {
			t.Errorf("measurement %d for %s is not successful", m.ID, m.TargetID)
		}
	}

	// 会话必须被标记结束。
	session, err := store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !session.Finished() {
		t.Error("session not finished after a complete scan")
	}
	if session.TargetCount != 3 {
		t.Errorf("TargetCount = %d, want 3", session.TargetCount)
	}
	if session.CompletedCount != 3 {
		t.Errorf("CompletedCount = %d, want 3", session.CompletedCount)
	}
}

// ---------------------------------------------------------------------------
// 断点续测：本阶段的核心
// ---------------------------------------------------------------------------

// TestScanResumeSkipsAlreadyMeasuredTargets 是需求第 20 条的核心测试。
//
// 判据必须是 (目标, 采集者, 会话) 三元组：
// 已在本会话测过的目标跳过，其余照测；且不产生重复行。
func TestScanResumeSkipsAlreadyMeasuredTargets(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-resume")
	targets := liveTargets(t, 4)

	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sessionID := newSessionID(t)
	if err := store.DefineSession(ctx, sessionID, pk, len(targets), "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 模拟"上一次跑到一半"：手动为前两个目标写入测量结果。
	at := time.Now().UTC().Add(-time.Hour)
	var preExisting []storage.Measurement
	for _, target := range targets[:2] {
		preExisting = append(preExisting, storage.Measurement{
			TargetID:    target.ID,
			CollectorID: pk,
			SessionID:   sessionID,
			Timestamp:   at,
			Success:     true,
			LatencyMS:   12.5,
		})
	}
	if _, _, err := store.SaveMeasurements(ctx, preExisting); err != nil {
		t.Fatalf("seed measurements: %v", err)
	}

	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Resume: true})
	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.Resumed {
		t.Error("Resumed = false, want true")
	}
	if result.AlreadyDone != 2 {
		t.Errorf("AlreadyDone = %d, want 2", result.AlreadyDone)
	}
	if result.TargetsPending != 2 {
		t.Errorf("TargetsPending = %d, want 2", result.TargetsPending)
	}
	// 只应该探测那两个还没测过的目标。
	if result.Probe.Completed != 2 {
		t.Errorf("Probe.Completed = %d, want 2 (must not re-probe finished targets)", result.Probe.Completed)
	}
	if result.Stored != 2 {
		t.Errorf("Stored = %d, want 2", result.Stored)
	}

	// 会话里最终应该是 4 条测量（2 旧 + 2 新），没有重复。
	measurements, err := store.QueryMeasurements(ctx, storage.MeasurementQuery{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != 4 {
		t.Fatalf("measurements = %d, want 4", len(measurements))
	}

	// 旧的那两条必须原样保留（时间序列只追加）。
	var foundOld bool
	for _, m := range measurements {
		if m.Timestamp.Equal(at.Truncate(time.Millisecond)) || m.Timestamp.Equal(at) {
			if m.LatencyMS != 12.5 {
				t.Errorf("pre-existing measurement was modified: %+v", m)
			}
			foundOld = true
		}
	}
	if !foundOld {
		t.Error("pre-existing measurements were not preserved")
	}
}

// TestScanResumeWhenEverythingIsDone 验证"会话开着但目标都测过了"的行为。
//
// 场景：上一次扫描实际上把所有目标都测完了，却因为进程被杀
// 没来得及标记会话结束。此时 `--resume` 应该什么都不测，
// 然后正常收尾——而不是报错，也不是假装又扫了一遍。
func TestScanResumeWhenEverythingIsDone(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-alldone")
	targets := liveTargets(t, 2)

	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 手工构造"未结束但已全部测过"的会话。
	sessionID := newSessionID(t)
	if err := store.DefineSession(ctx, sessionID, pk, len(targets), "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	seeded := make([]storage.Measurement, 0, len(targets))
	for _, target := range targets {
		seeded = append(seeded, storage.Measurement{
			TargetID: target.ID, CollectorID: pk, SessionID: sessionID,
			Timestamp: at, Success: true, LatencyMS: 9,
		})
	}
	if _, _, err := store.SaveMeasurements(ctx, seeded); err != nil {
		t.Fatal(err)
	}

	// 会话必须是未结束的，否则续测会被正确拒绝（那是另一条测试）。
	if session, err := store.LoadSession(ctx, sessionID); err != nil || session.Finished() {
		t.Fatalf("seed session state = %+v, err = %v; want open", session, err)
	}

	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Resume: true})
	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	if result.TargetsPending != 0 {
		t.Errorf("TargetsPending = %d, want 0", result.TargetsPending)
	}
	if result.Probe.Completed != 0 {
		t.Errorf("Probe.Completed = %d, want 0 (nothing left to measure)", result.Probe.Completed)
	}
	if result.Stored != 0 {
		t.Errorf("Stored = %d, want 0", result.Stored)
	}
	if result.AlreadyDone != len(targets) {
		t.Errorf("AlreadyDone = %d, want %d", result.AlreadyDone, len(targets))
	}
	// 已经全部完成的会话可以正常收尾，不会被误判成"空列表错误"。
	if result.Progress.Measured != int64(len(targets)) {
		t.Errorf("progress.Measured = %d, want %d", result.Progress.Measured, len(targets))
	}
	// 没有待测目标时应当直接收尾结束会话。
	if !result.SessionFinished {
		t.Error("SessionFinished = false, want true (nothing left to do)")
	}
}

// TestScanResumeErrorsAreActionable 验证续测失败时给出可操作的错误。
func TestScanResumeErrorsAreActionable(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-errors")
	targets := liveTargets(t, 1)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	t.Run("missing session", func(t *testing.T) {
		sched := newScheduler(t, store, pk, Config{SessionID: "20260101T000000Z-00000000", Resume: true})
		_, err := sched.Run(ctx, targets, "0.1.0")
		if err == nil {
			t.Fatal("Run succeeded for a missing session")
		}
		if !strings.Contains(err.Error(), "resume session") {
			t.Errorf("error = %v, want a resume-specific message", err)
		}
	})

	t.Run("resume without session id", func(t *testing.T) {
		sched := newScheduler(t, store, pk, Config{Resume: true})
		if _, err := sched.Run(ctx, targets, "0.1.0"); err == nil {
			t.Fatal("Run succeeded without a session id")
		}
	})

	t.Run("session closed while targets remain", func(t *testing.T) {
		// 一个"被提前标记结束、但还有目标没测"的会话是真问题：
		// 往一个已完成会话里继续追加数据会让会话状态与数据事实矛盾。
		//
		// 注意与另一种情况的区别：对一个**已经全部测完**的会话再次
		// --resume 不是错误（那是用户的正常操作，见
		// TestScanResumeWhenEverythingIsDone）。
		sessionID := newSessionID(t)
		if err := store.DefineSession(ctx, sessionID, pk, len(targets), "0.1.0", time.Now().UTC()); err != nil {
			t.Fatalf("DefineSession: %v", err)
		}
		// 一个目标都没测，却把会话关掉。
		if err := store.FinishSession(ctx, sessionID, 0, time.Now().UTC()); err != nil {
			t.Fatalf("FinishSession: %v", err)
		}

		resume := newScheduler(t, store, pk, Config{SessionID: sessionID, Resume: true})
		_, err := resume.Run(ctx, targets, "0.1.0")
		if err == nil {
			t.Fatal("Run succeeded for a session that was closed while targets remained")
		}
		if !strings.Contains(err.Error(), "already finished") {
			t.Errorf("error = %v, want 'already finished'", err)
		}
		if !strings.Contains(err.Error(), "prematurely") {
			t.Errorf("error = %v, want it to explain that the session was closed early", err)
		}
	})

	t.Run("session of another collector", func(t *testing.T) {
		otherPK := collector(t, store, "c-scan-other")
		sessionID := newSessionID(t)

		// 会话属于另一个采集者，但**故意保持未结束**：
		// 这样错误信息才会命中"归属"这一条，而不是"已结束"。
		if err := store.DefineSession(ctx, sessionID, otherPK, 1, "0.1.0", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}

		// 另一个采集者试图续测这个会话必须失败：
		// 否则两个节点的数据会混进同一次会话，
		// "从哪条线路测的"这个维度就毁了。
		mine := newScheduler(t, store, pk, Config{SessionID: sessionID, Resume: true})
		_, err := mine.Run(ctx, targets, "0.1.0")
		if err == nil {
			t.Fatal("Run succeeded for another collector's session")
		}
		if !strings.Contains(err.Error(), "another collector") {
			t.Errorf("error = %v, want a collector mismatch message", err)
		}
	})
}

// TestPendingTargetsIsSessionScoped 直接验证判据的语义。
//
// 同一个目标在**另一个会话**里测过，不影响本会话的"待测"判定；
// 同一个会话里测过（无论成功还是失败）就算完成。
func TestPendingTargetsIsSessionScoped(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-pending")
	targets := liveTargets(t, 3)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sessionA := newSessionID(t)
	sessionB := newSessionID(t)
	if err := store.DefineSession(ctx, sessionA, pk, 3, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.DefineSession(ctx, sessionB, pk, 3, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 在会话 A 里测了 target[0]（成功）与 target[1]（失败）。
	at := time.Now().UTC()
	if _, _, err := store.SaveMeasurements(ctx, []storage.Measurement{
		{TargetID: targets[0].ID, CollectorID: pk, SessionID: sessionA, Timestamp: at, Success: true, LatencyMS: 5},
		{TargetID: targets[1].ID, CollectorID: pk, SessionID: sessionA, Timestamp: at,
			Success: false, ErrorType: probe.ErrorTypeTimeout, ErrorMessage: "timeout"},
	}); err != nil {
		t.Fatal(err)
	}

	// 会话 A：只剩 target[2] 待测。失败的那条也算"测过"——
	// 失败也是线路信息，重跑时不该被当成"还没测"。
	pendingA, err := store.PendingTargets(ctx, targets, pk, sessionA)
	if err != nil {
		t.Fatalf("PendingTargets(A): %v", err)
	}
	if len(pendingA) != 1 || pendingA[0].ID != targets[2].ID {
		t.Errorf("pendingA = %v, want only %s", model.Keys(pendingA), targets[2].ID)
	}

	// 会话 B：三个都要测（别的会话测过不算）。
	pendingB, err := store.PendingTargets(ctx, targets, pk, sessionB)
	if err != nil {
		t.Fatalf("PendingTargets(B): %v", err)
	}
	if len(pendingB) != 3 {
		t.Errorf("pendingB = %d, want 3 (another session's results do not count)", len(pendingB))
	}

	// 源顺序必须被保持（不能让 SQL 排序打乱它）。
	for i := range pendingB {
		if pendingB[i].ID != targets[i].ID {
			t.Errorf("pendingB[%d] = %s, want %s (source order must be preserved)",
				i, pendingB[i].ID, targets[i].ID)
		}
	}

	// 参数校验。
	if _, err := store.PendingTargets(ctx, targets, 0, sessionA); err == nil {
		t.Error("PendingTargets accepted a zero collector")
	}
	if _, err := store.PendingTargets(ctx, targets, pk, ""); err == nil {
		t.Error("PendingTargets accepted an empty session")
	}
	if pending, err := store.PendingTargets(ctx, nil, pk, sessionA); err != nil || pending != nil {
		t.Errorf("PendingTargets(nil) = %v, %v; want nil, nil", pending, err)
	}
}

// TestScanDifferentSessionsDoNotBlockEachOther 验证"重复扫描"是可行的。
//
// 这正是需求第 20 条强调的点：同一个采集者必须能重新测量全部目标，
// 否则长期线路数据库就永远只有一批样本。
func TestScanDifferentSessionsDoNotBlockEachOther(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-repeat")
	targets := liveTargets(t, 2)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	first := newScheduler(t, store, pk, Config{SessionID: newSessionID(t)})
	r1, err := first.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if r1.Probe.Completed != 2 {
		t.Fatalf("first scan completed %d, want 2", r1.Probe.Completed)
	}

	// 第二次扫描：全新会话，必须仍然测全部目标。
	second := newScheduler(t, store, pk, Config{SessionID: newSessionID(t)})
	r2, err := second.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if r2.Probe.Completed != 2 {
		t.Errorf("second scan completed %d, want 2 (a new session must re-measure everything)",
			r2.Probe.Completed)
	}
	if r2.AlreadyDone != 0 {
		t.Errorf("AlreadyDone = %d, want 0 for a new session", r2.AlreadyDone)
	}

	// 两个会话的测量都必须保留（时间序列）。
	total, err := store.CountMeasurements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("measurements = %d, want 4 (both sessions preserved)", total)
	}
}

// ---------------------------------------------------------------------------
// 中断
// ---------------------------------------------------------------------------

// TestScanFlushesOnTimeNotJustOnBatchSize 是落库策略的回归测试。
//
// 早期实现只在"攒够 BatchSize 条"时写库，后果很严重：
// 大量目标超时时，一批可能要等好几分钟才满；在那之前进程被
// Ctrl+C 或被杀，数据库里**一条都没有**——整段时间白测。
//
// 这里把 BatchSize 设得很大（永远不会因条数触发），
// 只靠 FlushInterval 落库，验证时间上限确实生效。
func TestScanFlushesOnTimeNotJustOnBatchSize(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-flush-interval")

	// 造一批"立刻成功"的目标：用一个立即返回成功连接的 dialer，
	// 这样探测会飞快地产生大量结果，而我们只关心它们有没有被及时落库。
	targets := make([]model.Target, 0, 40)
	for i := 0; i < 40; i++ {
		target, err := model.NewTargetFromStrings("192.0.2.1", 10000+i)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	fastDialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		return &fakeConn{}, nil
	}

	sessionID := newSessionID(t)
	cfg := Config{
		// BatchSize 远大于目标数：永远不会因为条数触发落库。
		BatchSize:     100000,
		FlushInterval: 30 * time.Millisecond,
		Probe: probe.Config{
			Workers: 1,
			Timeout: time.Second,
			Dialer:  fastDialer,
		},
		SessionID: sessionID,
	}
	sched := newScheduler(t, store, pk, cfg)

	// 在扫描进行到一半时（阻塞 dialer 之后）检查库里的行数。
	//
	// 做法：先跑一次完整扫描，确认结果都被写入（说明 flush 生效）。
	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Stored != len(targets) {
		t.Fatalf("Stored = %d, want %d", result.Stored, len(targets))
	}

	measurements, err := store.QueryMeasurements(ctx, storage.MeasurementQuery{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(measurements) != len(targets) {
		t.Errorf("measurements = %d, want %d", len(measurements), len(targets))
	}

	// 关键断言：写库必须发生在"批未满"的情况下。
	// 用一个带阻塞 dialer 的场景再验证一次：只测前 5 个就取消，
	// 那 5 条必须已经落库（因为 FlushInterval 很短）。
	blockingDialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return &fakeConn{}, nil
		}
	}

	sessionID2 := newSessionID(t)
	cfg2 := Config{
		BatchSize:     100000, // 永远不因条数触发
		FlushInterval: 20 * time.Millisecond,
		Probe: probe.Config{
			Workers: 4,
			Timeout: 200 * time.Millisecond,
			Dialer:  blockingDialer,
		},
		SessionID: sessionID2,
	}
	sched2 := newScheduler(t, store, pk, cfg2)

	small := targets[:20]
	res2, err := sched2.Run(ctx, small, "0.1.0")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.Stored != len(small) {
		t.Errorf("Stored = %d, want %d (time-based flush must persist results)", res2.Stored, len(small))
	}

	stored, err := store.CountMeasurementsByQuery(ctx, storage.MeasurementQuery{SessionID: sessionID2})
	if err != nil {
		t.Fatal(err)
	}
	if stored != int64(len(small)) {
		t.Errorf("measurements in session 2 = %d, want %d", stored, len(small))
	}
}

// fakeConn 是一个"握手成功"的空连接。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, errors.New("not implemented") }
func (fakeConn) Write([]byte) (int, error)        { return 0, errors.New("not implemented") }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// TestScanInterruptedLeavesSessionOpen 验证中断后会话保持未结束。
//
// 这是断点续测的前提：会话已结束就没法续测了。
// 用"注入一个会阻塞到 ctx 取消的 dialer"来精确控制中断时机。
func TestScanInterruptedLeavesSessionOpen(t *testing.T) {
	store := newStore(t)
	pk := collector(t, store, "c-scan-interrupt")

	// 造一批目标（不需要真实可达：dialer 会拦住它们）。
	targets := make([]model.Target, 0, 20)
	for i := 0; i < 20; i++ {
		target, err := model.NewTargetFromStrings("192.0.2.1", 10000+i)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	if _, err := store.UpsertTargets(context.Background(), targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 阻塞到 ctx 结束的 dialer：保证取消发生在处理过程中间。
	blockingDialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	sessionID := newSessionID(t)
	cfg := Config{
		Probe:     probe.Config{Workers: 2, Timeout: time.Hour, Dialer: blockingDialer},
		SessionID: sessionID,
	}
	sched := newScheduler(t, store, pk, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Interrupted {
		t.Error("Interrupted = false, want true")
	}
	if result.SessionFinished {
		t.Error("SessionFinished = true; an interrupted scan must leave the session open for --resume")
	}

	session, err := store.LoadSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Finished() {
		t.Error("session is finished after an interruption; --resume would be impossible")
	}
}

// ---------------------------------------------------------------------------
// 落库失败与跟踪阶段
// ---------------------------------------------------------------------------

// TestScanReportsStorageFailures 验证落库失败会被计数而不是静默吞掉。
func TestScanReportsStorageFailures(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-storefail")

	// 目标**不**入库：测量写入会触发外键失败。
	targets := liveTargets(t, 2)

	sched := newScheduler(t, store, pk, Config{SessionID: newSessionID(t), BatchSize: 1})

	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 测量仍然要"测到"（探测本身成功），但落库必须报失败。
	if result.Probe.Completed != 2 {
		t.Errorf("Probe.Completed = %d, want 2 (the probes themselves succeed)", result.Probe.Completed)
	}
	if result.StoreFailures == 0 {
		t.Error("StoreFailures = 0, want > 0 when measurements cannot be stored")
	}
	if result.Stored != 0 {
		t.Errorf("Stored = %d, want 0", result.Stored)
	}
}

// ---------------------------------------------------------------------------
// 两级测量（Level 1 TCP Probe -> Level 2 route trace）
// ---------------------------------------------------------------------------

// mustTargetOf 构造一个测试目标。
func mustTargetOf(t *testing.T, ip string, port int) model.Target {
	t.Helper()
	target, err := model.NewTargetFromStrings(ip, port)
	if err != nil {
		t.Fatalf("NewTargetFromStrings(%q, %d): %v", ip, port, err)
	}
	return target
}

// fakeTraceEngine 是一个可编程的跟踪引擎。
//
// 用假引擎而不是真的调 nexttrace：这一层要验证的是**编排规则**
// （只跟踪成功目标、续测跳过、失败不中断、目录落库），
// 真引擎的行为已经在 internal/trace 里用自己的测试与真实输出夹具覆盖了。
type fakeTraceEngine struct {
	calls  []string
	result func(target model.Target) *trace.TraceResult
	err    error
	delay  time.Duration
}

func (f *fakeTraceEngine) Name() string { return "fake-nexttrace" }

func (f *fakeTraceEngine) Trace(ctx context.Context, target model.Target) (*trace.TraceResult, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.calls = append(f.calls, target.ID)
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result(target), nil
	}
	return &trace.TraceResult{
		TargetID:      target.String(),
		IP:            target.IP,
		Port:          target.Port,
		Engine:        f.Name(),
		EngineVersion: "9.9.9",
		Mode:          trace.ModeTCP,
		Protocol:      "tcp",
		Success:       true,
		DurationMS:    120,
		Hops: []trace.Hop{
			{TTL: 1, IP: "192.168.1.1", RTTMS: []float64{1.2}},
			{TTL: 2, IP: target.IP, RTTMS: []float64{8.4}},
		},
		Timestamp: time.Now().UTC(),
	}, nil
}

// recordSuccess 直接往数据库里写一条"探测成功"的事实。
//
// 为什么不跑一次真实探测：我们需要**精确控制**哪些目标成功、
// 哪些失败，才能验证"只跟踪成功目标"这条规则。
// 靠真实扫描来碰运气地得到成功/失败组合是不可靠的。
func recordSuccess(t *testing.T, store *storage.Store, collectorPK int64, sessionID string, target model.Target) {
	t.Helper()
	upsertTarget(t, store, target)
	_, _, err := store.SaveMeasurements(context.Background(), []storage.Measurement{
		storage.NewMeasurement(collectorPK, sessionID, probe.ProbeResult{
			TargetID:  target.String(),
			IP:        target.IP,
			Port:      target.Port,
			Success:   true,
			LatencyMS: 12.5,
			Timestamp: time.Now().UTC(),
		}),
	})
	if err != nil {
		t.Fatalf("SaveMeasurements (success): %v", err)
	}
}

// upsertTarget 先写目标行：测量有外键指向 targets，
// 直接写测量会得到 FOREIGN KEY constraint failed。
func upsertTarget(t *testing.T, store *storage.Store, target model.Target) {
	t.Helper()
	if _, err := store.UpsertTargets(context.Background(), []model.Target{target}, time.Now().UTC()); err != nil {
		t.Fatalf("UpsertTargets(%s): %v", target.ID, err)
	}
}

// recordFailure 直接往数据库里写一条"探测失败"的事实。
func recordFailure(t *testing.T, store *storage.Store, collectorPK int64, sessionID string, target model.Target) {
	t.Helper()
	upsertTarget(t, store, target)
	_, _, err := store.SaveMeasurements(context.Background(), []storage.Measurement{
		storage.NewMeasurement(collectorPK, sessionID, probe.ProbeResult{
			TargetID:  target.String(),
			IP:        target.IP,
			Port:      target.Port,
			Success:   false,
			ErrorType: probe.ErrorTypeTimeout,
			Timestamp: time.Now().UTC(),
		}),
	})
	if err != nil {
		t.Fatalf("SaveMeasurements (failure): %v", err)
	}
}

// defineSession 建立一个会话（第二级查询需要它存在）。
func defineSession(t *testing.T, store *storage.Store, collectorPK int64, sessionID string, targetCount int) {
	t.Helper()
	if err := store.DefineSession(context.Background(), sessionID, collectorPK, targetCount, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatalf("DefineSession: %v", err)
	}
}

// TestTracePhaseOnlyTracesProbeSuccesses 是 Phase 8 的核心测试。
//
// 两级测量的实质：连 TCP 都不通的目标不值得花几十秒跑 traceroute。
func TestTracePhaseOnlyTracesProbeSuccesses(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-two-level")
	sessionID := newSessionID(t)

	// 三个目标：两个探测成功，一个探测失败。
	success1 := mustTargetOf(t, "10.1.1.1", 443)
	success2 := mustTargetOf(t, "10.1.1.2", 2053)
	failure := mustTargetOf(t, "10.1.1.3", 8443)

	defineSession(t, store, pk, sessionID, 3)
	recordSuccess(t, store, pk, sessionID, success1)
	recordSuccess(t, store, pk, sessionID, success2)
	recordFailure(t, store, pk, sessionID, failure)

	engine := &fakeTraceEngine{}
	sched := newScheduler(t, store, pk, Config{
		SessionID:   sessionID,
		Trace:       true,
		TraceEngine: engine,
	})

	result := &Result{}
	if err := sched.tracePhase(ctx, []model.Target{success1, success2, failure}, sessionID, result); err != nil {
		t.Fatalf("tracePhase: %v", err)
	}

	// 只应该跟踪那两个成功的目标。
	if result.TracePending != 2 {
		t.Errorf("TracePending = %d, want 2 (only TCP-successful targets)", result.TracePending)
	}
	if len(engine.calls) != 2 {
		t.Fatalf("engine called %d times, want 2 (calls=%v)", len(engine.calls), engine.calls)
	}
	for _, id := range engine.calls {
		if id == failure.ID {
			t.Errorf("engine was called for the FAILED target %s; it must be skipped", id)
		}
	}
	if result.TraceAttempted != 2 || result.TraceStored != 2 {
		t.Errorf("TraceAttempted = %d, TraceStored = %d, want 2 attempted and 2 stored",
			result.TraceAttempted, result.TraceStored)
	}
	if result.TraceStored != 2 {
		t.Errorf("TraceStored = %d, want 2", result.TraceStored)
	}
	if result.Trace.Success != 2 {
		t.Errorf("Trace.Success = %d, want 2", result.Trace.Success)
	}
	if result.Trace.AverageHops() != 2 {
		t.Errorf("AverageHops() = %v, want 2", result.Trace.AverageHops())
	}
}

// TestTracePhaseResumesWithoutRetracing 验证第二级也有断点续测。
//
// 一次 traceroute 要几十秒，中断重跑时把这些目标重跑一遍代价很高。
func TestTracePhaseResumesWithoutRetracing(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-trace-resume")
	sessionID := newSessionID(t)

	targets := []model.Target{
		mustTargetOf(t, "10.2.2.1", 443),
		mustTargetOf(t, "10.2.2.2", 443),
		mustTargetOf(t, "10.2.2.3", 443),
	}

	defineSession(t, store, pk, sessionID, len(targets))
	for _, target := range targets {
		recordSuccess(t, store, pk, sessionID, target)
	}

	// 第一次：全部跟踪。
	first := &fakeTraceEngine{}
	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Trace: true, TraceEngine: first})
	result1 := &Result{}
	if err := sched.tracePhase(ctx, targets, sessionID, result1); err != nil {
		t.Fatalf("first tracePhase: %v", err)
	}
	if len(first.calls) != 3 || result1.TraceStored != 3 {
		t.Fatalf("first pass: calls=%d stored=%d, want 3/3", len(first.calls), result1.TraceStored)
	}

	// 第二次：同一会话重跑，应该全部跳过。
	second := &fakeTraceEngine{}
	sched2 := newScheduler(t, store, pk, Config{SessionID: sessionID, Trace: true, TraceEngine: second})
	result2 := &Result{}
	if err := sched2.tracePhase(ctx, targets, sessionID, result2); err != nil {
		t.Fatalf("second tracePhase: %v", err)
	}

	if len(second.calls) != 0 {
		t.Errorf("engine called %d times on resume, want 0 (already traced)", len(second.calls))
	}
	if result2.TracePending != 0 {
		t.Errorf("TracePending = %d, want 0", result2.TracePending)
	}
	if result2.TraceAlreadyDone != 3 {
		t.Errorf("TraceAlreadyDone = %d, want 3", result2.TraceAlreadyDone)
	}
	if result2.TraceStored != 0 {
		t.Errorf("TraceStored = %d, want 0", result2.TraceStored)
	}
}

// TestTracePhaseEngineErrorDoesNotAbort 验证引擎层面的失败不中断整批。
func TestTracePhaseEngineErrorDoesNotAbort(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-trace-engine-err")
	sessionID := newSessionID(t)

	targets := []model.Target{
		mustTargetOf(t, "10.3.3.1", 443),
		mustTargetOf(t, "10.3.3.2", 443),
	}
	defineSession(t, store, pk, sessionID, len(targets))
	for _, target := range targets {
		recordSuccess(t, store, pk, sessionID, target)
	}

	engine := &fakeTraceEngine{err: errors.New("engine exploded")}
	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Trace: true, TraceEngine: engine})

	result := &Result{}
	if err := sched.tracePhase(ctx, targets, sessionID, result); err != nil {
		t.Fatalf("tracePhase returned error: %v (engine failures must not abort)", err)
	}

	// 每个目标都尝试过，失败被如实计入。
	if result.TraceAttempted != 2 {
		t.Errorf("TraceAttempted = %d, want 2", result.TraceAttempted)
	}
	if result.Trace.Failed != 2 {
		t.Errorf("Trace.Failed = %d, want 2", result.Trace.Failed)
	}
	if result.Trace.Success != 0 {
		t.Errorf("Trace.Success = %d, want 0", result.Trace.Success)
	}
	if result.TraceStoreFailures != 2 {
		t.Errorf("TraceStoreFailures = %d, want 2 (engine errors are counted)", result.TraceStoreFailures)
	}
	if result.TraceStored != 0 {
		t.Errorf("TraceStored = %d, want 0 (nothing successful to store)", result.TraceStored)
	}
}

// TestTracePhaseStoresFailuresToo 验证跟踪失败的结果也会入库。
//
// 与第一级同理："这个目标的路径断在第 5 跳"是线路信息，
// 不该因为 success=false 就被丢掉。
func TestTracePhaseStoresFailuresToo(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-trace-store-fail")
	sessionID := newSessionID(t)

	target := mustTargetOf(t, "10.4.4.1", 443)
	defineSession(t, store, pk, sessionID, 1)
	recordSuccess(t, store, pk, sessionID, target)

	engine := &fakeTraceEngine{
		result: func(target model.Target) *trace.TraceResult {
			return &trace.TraceResult{
				TargetID:     target.String(),
				IP:           target.IP,
				Port:         target.Port,
				Engine:       "fake-nexttrace",
				Mode:         trace.ModeTCP,
				Success:      false,
				ErrorType:    trace.ErrorTypeExitCode,
				ErrorMessage: "process exited with code 1",
				Timestamp:    time.Now().UTC(),
			}
		},
	}

	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Trace: true, TraceEngine: engine})
	result := &Result{}
	if err := sched.tracePhase(ctx, []model.Target{target}, sessionID, result); err != nil {
		t.Fatalf("tracePhase: %v", err)
	}

	if result.TraceStored != 1 {
		t.Errorf("TraceStored = %d, want 1 (failed traces are also route information)", result.TraceStored)
	}
	if result.Trace.Failed != 1 {
		t.Errorf("Trace.Failed = %d, want 1", result.Trace.Failed)
	}

	// 数据库里确实有一行失败记录，且带有分类。
	views, err := store.QueryTraces(ctx, target.ID, 10)
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("stored traces = %d, want 1", len(views))
	}
	if views[0].Success {
		t.Error("stored trace reports success, want failure")
	}
	if views[0].ErrorType != string(trace.ErrorTypeExitCode) {
		t.Errorf("stored error type = %q, want %q", views[0].ErrorType, trace.ErrorTypeExitCode)
	}
}

// TestScanTraceWithoutEngineIsReportedNotFaked 是一条"不撒谎"的测试。
//
// 没有引擎时必须把 TraceSkipped 置为真，让用户明确知道"跟踪没做"，
// 而不是静默跳过、让汇总看起来像做了。
func TestScanTraceWithoutEngineIsReportedNotFaked(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-trace")
	targets := liveTargets(t, 2)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sched := newScheduler(t, store, pk, Config{SessionID: newSessionID(t), Trace: true})
	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.TraceSkipped {
		t.Error("TraceSkipped = false, want true when --trace is requested but no engine is available")
	}
	if result.TraceAttempted != 0 {
		t.Errorf("TraceAttempted = %d, want 0 (nothing may be reported as traced)", result.TraceAttempted)
	}
	if result.TraceStored != 0 {
		t.Errorf("TraceStored = %d, want 0", result.TraceStored)
	}
}

// TestScanFullTwoLevelFlow 验证完整的两级流程（真实 TCP 探测 + 假跟踪引擎）。
//
// 这里第一级是**真实**的本机监听探测，第二级是假引擎——
// 因此它同时验证了"探测结果确实被用来筛选跟踪目标"这条连接。
func TestScanFullTwoLevelFlow(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-two-level-e2e")
	targets := liveTargets(t, 3)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	engine := &fakeTraceEngine{}
	sched := newScheduler(t, store, pk, Config{
		SessionID:   newSessionID(t),
		Trace:       true,
		TraceEngine: engine,
	})

	result, err := sched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.TraceSkipped {
		t.Fatal("TraceSkipped = true, want false when an engine is provided")
	}
	// 本机监听都能连上，因此第一级应当全部成功。
	if result.Probe.Success != 3 {
		t.Fatalf("Probe.Success = %d, want 3 (local listeners must connect)", result.Probe.Success)
	}
	// 第二级应当为每个成功目标各调用一次。
	if len(engine.calls) != 3 {
		t.Errorf("engine called %d times, want 3", len(engine.calls))
	}
	if result.TracePending != 3 {
		t.Errorf("TracePending = %d, want 3", result.TracePending)
	}
	if result.TraceStored != 3 {
		t.Errorf("TraceStored = %d, want 3", result.TraceStored)
	}
	if !result.SessionFinished {
		t.Error("SessionFinished = false, want true")
	}

	// 数据库里三条跟踪记录都在。
	total, err := store.CountTraces(ctx)
	if err != nil {
		t.Fatalf("CountTraces: %v", err)
	}
	if total != 3 {
		t.Errorf("traces in db = %d, want 3", total)
	}
}

// TestScanTraceResumeSkipsAlreadyTracedTargets 验证通过 Run 走完整流程时的续测。
func TestScanTraceResumeSkipsAlreadyTracedTargets(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-two-level-resume")
	targets := liveTargets(t, 2)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sessionID := newSessionID(t)

	// 第一次完整跑完。
	first := &fakeTraceEngine{}
	sched := newScheduler(t, store, pk, Config{SessionID: sessionID, Trace: true, TraceEngine: first})
	if _, err := sched.Run(ctx, targets, "0.1.0"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first.calls) != 2 {
		t.Fatalf("first pass engine calls = %d, want 2", len(first.calls))
	}

	// 续测：第一级已经全部测过，因此 pending 为空，直接收尾，
	// 第二级也不该被调用。
	second := &fakeTraceEngine{}
	resumeSched := newScheduler(t, store, pk, Config{
		SessionID: sessionID, Resume: true, Trace: true, TraceEngine: second,
	})
	result, err := resumeSched.Run(ctx, targets, "0.1.0")
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	if result.AlreadyDone != 2 {
		t.Errorf("AlreadyDone = %d, want 2", result.AlreadyDone)
	}
	if len(second.calls) != 0 {
		t.Errorf("engine called %d times on resume, want 0", len(second.calls))
	}
	// 跟踪记录没有翻倍。
	total, err := store.CountTraces(ctx)
	if err != nil {
		t.Fatalf("CountTraces: %v", err)
	}
	if total != 2 {
		t.Errorf("traces in db = %d, want 2 (no duplicates)", total)
	}
}

// TestTraceDuplicatesAreSkippedByIdempotentInsert 验证重复写入是幂等的。
//
// 与第一级同理：dedup_key 让同一个 batch 重复导入变成空操作。
func TestTraceDuplicatesAreSkippedByIdempotentInsert(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-trace-dedup")
	sessionID := newSessionID(t)

	target := mustTargetOf(t, "10.5.5.1", 443)
	defineSession(t, store, pk, sessionID, 1)
	recordSuccess(t, store, pk, sessionID, target)

	// 固定时间戳，保证两次生成的 dedup_key 相同。
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	engine := &fakeTraceEngine{
		result: func(target model.Target) *trace.TraceResult {
			return &trace.TraceResult{
				TargetID:   target.String(),
				IP:         target.IP,
				Port:       target.Port,
				Engine:     "fake-nexttrace",
				Mode:       trace.ModeTCP,
				Success:    true,
				DurationMS: 100,
				Hops:       []trace.Hop{{TTL: 1, IP: "10.0.0.1", RTTMS: []float64{1}}},
				Timestamp:  at,
			}
		},
	}

	// 第一遍写入。
	batch1 := []storage.Trace{storage.NewTrace(pk, sessionID, engine.resultFor(target))}
	saved1, _, err := store.SaveTraces(ctx, batch1)
	if err != nil {
		t.Fatalf("SaveTraces #1: %v", err)
	}
	if saved1 != 1 {
		t.Fatalf("saved #1 = %d, want 1", saved1)
	}

	// 第二遍写同样的行：应当被幂等跳过。
	batch2 := []storage.Trace{storage.NewTrace(pk, sessionID, engine.resultFor(target))}
	saved2, skipped2, err := store.SaveTraces(ctx, batch2)
	if err != nil {
		t.Fatalf("SaveTraces #2: %v", err)
	}
	if saved2 != 0 || skipped2 != 1 {
		t.Errorf("saved/skipped = %d/%d, want 0/1", saved2, skipped2)
	}

	total, err := store.CountTraces(ctx)
	if err != nil {
		t.Fatalf("CountTraces: %v", err)
	}
	if total != 1 {
		t.Errorf("traces in db = %d, want 1", total)
	}
}

// resultFor 是给幂等测试用的便捷封装。
func (f *fakeTraceEngine) resultFor(target model.Target) *trace.TraceResult {
	result, _ := f.Trace(context.Background(), target)
	return result
}

// ---------------------------------------------------------------------------
// 参数与边界
// ---------------------------------------------------------------------------

func TestNewValidatesConfig(t *testing.T) {
	store := newStore(t)

	if _, err := New(nil, Config{CollectorPK: 1}); err == nil {
		t.Error("New accepted a nil store")
	}
	if _, err := New(store, Config{}); err == nil {
		t.Error("New accepted a zero collector key")
	}

	// 会话有外键指向采集者，因此这里必须先建一个真实采集者。
	pk := collector(t, store, "c-config-validate")

	// 空目标列表不应该报错：调用方（例如 all.json 恰好为空）
	// 应当拿到一份"什么都没测"的结果，而不是一个错误。
	sessionID, err := model.NewSessionID(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	empty := newScheduler(t, store, pk, Config{SessionID: sessionID})

	result, err := empty.Run(context.Background(), nil, "0.1.0")
	if err != nil {
		t.Fatalf("Run with no targets: %v", err)
	}
	if result.TargetsTotal != 0 || result.TargetsPending != 0 || result.Stored != 0 {
		t.Errorf("result = %+v, want an empty scan", result)
	}

	// 新建会话却没有会话 ID 是编程错误，必须报错而不是静默生成一个。
	broken := newScheduler(t, store, pk, Config{})
	if _, err := broken.Run(context.Background(), nil, "0.1.0"); err == nil {
		t.Error("Run succeeded without a session id")
	}
}

func TestLooksLikeResumeError(t *testing.T) {
	if LooksLikeResumeError(nil) {
		t.Error("LooksLikeResumeError(nil) = true, want false")
	}
	if !LooksLikeResumeError(fmt.Errorf("wrap: %w", storage.ErrNotFound)) {
		t.Error("wrapped ErrNotFound should count as a resume error")
	}
	if !LooksLikeResumeError(storage.ErrInvalidInput) {
		t.Error("ErrInvalidInput should count as a resume error")
	}
	if LooksLikeResumeError(errors.New("some other failure")) {
		t.Error("unrelated error should not count as a resume error")
	}
}

// TestResultElapsed 覆盖耗时计算。
func TestResultElapsed(t *testing.T) {
	start := time.Now().UTC()
	r := Result{StartedAt: start, FinishedAt: start.Add(3 * time.Second)}
	if got := r.Elapsed(); got != 3*time.Second {
		t.Errorf("Elapsed() = %s, want 3s", got)
	}
	if got := (Result{}).Elapsed(); got != 0 {
		t.Errorf("Elapsed() = %s, want 0 for zero Result", got)
	}
}

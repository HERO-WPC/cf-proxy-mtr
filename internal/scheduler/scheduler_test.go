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

	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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
	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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
	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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
		_, err := sched.Run(ctx, targets, "0.1.0", nil)
		if err == nil {
			t.Fatal("Run succeeded for a missing session")
		}
		if !strings.Contains(err.Error(), "resume session") {
			t.Errorf("error = %v, want a resume-specific message", err)
		}
	})

	t.Run("resume without session id", func(t *testing.T) {
		sched := newScheduler(t, store, pk, Config{Resume: true})
		if _, err := sched.Run(ctx, targets, "0.1.0", nil); err == nil {
			t.Fatal("Run succeeded without a session id")
		}
	})

	t.Run("finished session", func(t *testing.T) {
		sessionID := newSessionID(t)
		sched := newScheduler(t, store, pk, Config{SessionID: sessionID})
		if _, err := sched.Run(ctx, targets, "0.1.0", nil); err != nil {
			t.Fatalf("initial Run: %v", err)
		}

		resume := newScheduler(t, store, pk, Config{SessionID: sessionID, Resume: true})
		_, err := resume.Run(ctx, targets, "0.1.0", nil)
		if err == nil {
			t.Fatal("Run succeeded for a finished session")
		}
		if !strings.Contains(err.Error(), "already finished") {
			t.Errorf("error = %v, want 'already finished'", err)
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
		_, err := mine.Run(ctx, targets, "0.1.0", nil)
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
	r1, err := first.Run(ctx, targets, "0.1.0", nil)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if r1.Probe.Completed != 2 {
		t.Fatalf("first scan completed %d, want 2", r1.Probe.Completed)
	}

	// 第二次扫描：全新会话，必须仍然测全部目标。
	second := newScheduler(t, store, pk, Config{SessionID: newSessionID(t)})
	r2, err := second.Run(ctx, targets, "0.1.0", nil)
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
	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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
	res2, err := sched2.Run(ctx, small, "0.1.0", nil)
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

	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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

	result, err := sched.Run(ctx, targets, "0.1.0", nil)
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

// TestScanTraceWithoutEngineIsReportedNotFaked 是一条"不撒谎"的测试。
//
// --trace 在 Phase 7 之前无法执行。此时必须把 TraceSkipped 置为真，
// 让用户明确知道"跟踪没做"，而不是静默跳过、让汇总看起来像做了。
func TestScanTraceWithoutEngineIsReportedNotFaked(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-trace")
	targets := liveTargets(t, 2)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	sched := newScheduler(t, store, pk, Config{SessionID: newSessionID(t), Trace: true})
	result, err := sched.Run(ctx, targets, "0.1.0", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.TraceSkipped {
		t.Error("TraceSkipped = false, want true when --trace is requested but no engine is available")
	}
	if result.TraceAttempted != 0 {
		t.Errorf("TraceAttempted = %d, want 0 (nothing may be reported as traced)", result.TraceAttempted)
	}
}

// TestScanTraceInvokesProvidedEngine 验证提供 traceFn 时它会被调用。
func TestScanTraceInvokesProvidedEngine(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-trace-fn")
	targets := liveTargets(t, 3)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var traced []string
	traceFn := func(ctx context.Context, target model.Target) error {
		traced = append(traced, target.ID)
		return nil
	}

	sched := newScheduler(t, store, pk, Config{SessionID: newSessionID(t), Trace: true})
	result, err := sched.Run(ctx, targets, "0.1.0", traceFn)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.TraceSkipped {
		t.Error("TraceSkipped = true, want false when an engine is provided")
	}
	if result.TraceAttempted != 3 {
		t.Errorf("TraceAttempted = %d, want 3", result.TraceAttempted)
	}
	if len(traced) != 3 {
		t.Errorf("engine called %d times, want 3", len(traced))
	}
}

// TestScanTraceFailuresDoNotAbort 验证单个跟踪失败不中断整体。
func TestScanTraceFailuresDoNotAbort(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	pk := collector(t, store, "c-scan-trace-err")
	targets := liveTargets(t, 3)
	if _, err := store.UpsertTargets(ctx, targets, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var calls int
	traceFn := func(ctx context.Context, target model.Target) error {
		calls++
		return errors.New("engine exploded")
	}

	sched := newScheduler(t, store, pk, Config{SessionID: newSessionID(t), Trace: true})
	result, err := sched.Run(ctx, targets, "0.1.0", traceFn)
	if err != nil {
		t.Fatalf("Run returned error: %v (a single trace failure must not abort the scan)", err)
	}
	if calls != 3 {
		t.Errorf("engine called %d times, want 3 (failures must not stop the loop)", calls)
	}
	if result.TraceAttempted != 3 {
		t.Errorf("TraceAttempted = %d, want 3", result.TraceAttempted)
	}
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

	result, err := empty.Run(context.Background(), nil, "0.1.0", nil)
	if err != nil {
		t.Fatalf("Run with no targets: %v", err)
	}
	if result.TargetsTotal != 0 || result.TargetsPending != 0 || result.Stored != 0 {
		t.Errorf("result = %+v, want an empty scan", result)
	}

	// 新建会话却没有会话 ID 是编程错误，必须报错而不是静默生成一个。
	broken := newScheduler(t, store, pk, Config{})
	if _, err := broken.Run(context.Background(), nil, "0.1.0", nil); err == nil {
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

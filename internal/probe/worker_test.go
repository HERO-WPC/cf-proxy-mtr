package probe

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 并发观测辅助
// ---------------------------------------------------------------------------

// concurrencyTracker 记录同时处于处理中的 job 数峰值。
//
// 用原子操作而不是锁：它会在每个 job 的热路径上被调用，
// 而且测试本身要验证的就是并发行为，不能再引入会改变时序的锁。
type concurrencyTracker struct {
	active int32
	peak   int32
	total  int32
}

func (c *concurrencyTracker) enter() {
	cur := atomic.AddInt32(&c.active, 1)
	atomic.AddInt32(&c.total, 1)
	for {
		old := atomic.LoadInt32(&c.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&c.peak, old, cur) {
			return
		}
	}
}

func (c *concurrencyTracker) leave() { atomic.AddInt32(&c.active, -1) }

func (c *concurrencyTracker) Peak() int  { return int(atomic.LoadInt32(&c.peak)) }
func (c *concurrencyTracker) Total() int { return int(atomic.LoadInt32(&c.total)) }

// sliceProducer 把固定列表作为 job 源。
func sliceProducer[T any](jobs []T) Producer[T] {
	return func(ctx context.Context, send func(T) bool) int {
		sent := 0
		for _, job := range jobs {
			if !send(job) {
				return sent
			}
			sent++
		}
		return sent
	}
}

// ---------------------------------------------------------------------------
// Run：基本语义
// ---------------------------------------------------------------------------

// TestRunProcessesEveryJobExactlyOnce 是 worker pool 最基本的正确性要求。
//
// 漏掉 job 意味着"有些目标根本没被测"，重复 job 意味着
// "同一个目标被算了两次"，两者都会直接污染统计。
func TestRunProcessesEveryJobExactlyOnce(t *testing.T) {
	const (
		jobCount = 500
		workers  = 8
	)

	jobs := make([]int, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		jobs = append(jobs, i)
	}

	var (
		mu      sync.Mutex
		seen    = make(map[int]int, jobCount)
		emitted int
		tracker concurrencyTracker
	)

	stats := Run(context.Background(), workers, 4,
		sliceProducer(jobs),
		func(ctx context.Context, job int, emit func(int)) error {
			tracker.enter()
			defer tracker.leave()
			// 每个 job 睡一小会儿：这样 Elapsed 一定能超过
			// Windows 上约 15ms 的计时器粒度，使时间断言有意义
			// （否则一个跑得很快的 run 会测出恰好 0）。
			time.Sleep(500 * time.Microsecond)
			emit(job * 2)
			return nil
		},
		func(r int) {
			mu.Lock()
			defer mu.Unlock()
			emitted++
			seen[r/2]++
		},
	)

	if stats.Submitted != jobCount {
		t.Errorf("Submitted = %d, want %d", stats.Submitted, jobCount)
	}
	if stats.Processed != jobCount {
		t.Errorf("Processed = %d, want %d", stats.Processed, jobCount)
	}
	if stats.Emitted != jobCount {
		t.Errorf("Emitted = %d, want %d", stats.Emitted, jobCount)
	}
	if stats.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", stats.Skipped)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (first: %v)", stats.Errors, stats.FirstError)
	}
	if stats.Canceled {
		t.Error("Canceled = true, want false")
	}
	if tracker.Total() != jobCount {
		t.Errorf("jobs actually entered processing = %d, want %d", tracker.Total(), jobCount)
	}
	if emitted != jobCount {
		t.Errorf("emitted = %d, want %d", emitted, jobCount)
	}
	if len(seen) != jobCount {
		t.Errorf("distinct jobs = %d, want %d", len(seen), jobCount)
	}
	for job, n := range seen {
		if n != 1 {
			t.Errorf("job %d was processed %d times, want exactly once", job, n)
		}
	}
	if stats.Elapsed <= 0 {
		t.Error("Elapsed = 0, want positive")
	}
	if stats.Completeness() != 1 {
		t.Errorf("Completeness() = %v, want 1", stats.Completeness())
	}
}

// TestRunRespectsWorkerLimit 验证并发上限。
//
// 需求第 12 / 19 条：绝不能无限创建 goroutine，
// 也绝不能因为并发过高把本机连接表打满。
func TestRunRespectsWorkerLimit(t *testing.T) {
	jobs := make([]int, 200)
	for i := range jobs {
		jobs[i] = i
	}

	for _, workers := range []int{1, 2, 8, 32} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var tracker concurrencyTracker

			stats := Run(context.Background(), workers, 4,
				sliceProducer(jobs),
				func(ctx context.Context, job int, emit func(int)) error {
					tracker.enter()
					defer tracker.leave()
					// 睡一会儿，制造重叠窗口，否则 worker 会串行完成，
					// 测不出并发上限。
					time.Sleep(200 * time.Microsecond)
					emit(job)
					return nil
				},
				func(int) {},
			)

			if stats.Processed != len(jobs) {
				t.Fatalf("Processed = %d, want %d", stats.Processed, len(jobs))
			}
			if peak := tracker.Peak(); peak > workers {
				t.Errorf("peak concurrency = %d, want <= %d", peak, workers)
			}
			// 并发数确实被用起来了（除了 workers=1）。
			if workers > 1 && tracker.Peak() < 2 {
				t.Errorf("peak concurrency = %d, want >= 2 for workers=%d", tracker.Peak(), workers)
			}
		})
	}
}

// TestRunHandlesZeroAndNegativeWorkers 确认不会因为 0 并发而挂死。
func TestRunHandlesZeroAndNegativeWorkers(t *testing.T) {
	jobs := []int{1, 2, 3}

	for _, workers := range []int{0, -1} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			stats := Run(context.Background(), workers, 0,
				sliceProducer(jobs),
				func(ctx context.Context, job int, emit func(int)) error {
					emit(job)
					return nil
				},
				func(int) {},
			)
			if stats.Processed != len(jobs) {
				t.Errorf("Processed = %d, want %d", stats.Processed, len(jobs))
			}
		})
	}
}

func TestRunEmptyProducer(t *testing.T) {
	stats := Run(context.Background(), 4, 0,
		sliceProducer([]int{}),
		func(ctx context.Context, job int, emit func(int)) error { return nil },
		func(int) {},
	)

	if stats.Submitted != 0 || stats.Processed != 0 || stats.Emitted != 0 {
		t.Errorf("stats = %+v, want all zero", stats)
	}
	if stats.Completeness() != 1 {
		t.Errorf("Completeness() = %v, want 1 for an empty task", stats.Completeness())
	}
}

func TestRunRejectsNilArguments(t *testing.T) {
	t.Run("nil producer", func(t *testing.T) {
		stats := Run[int, int](context.Background(), 1, 0, nil,
			func(ctx context.Context, job int, emit func(int)) error { return nil },
			func(int) {})
		if stats.FirstError == nil {
			t.Error("FirstError = nil, want an error")
		}
	})
	t.Run("nil process", func(t *testing.T) {
		stats := Run(context.Background(), 1, 0, sliceProducer([]int{1}), nil, func(int) {})
		if stats.FirstError == nil {
			t.Error("FirstError = nil, want an error")
		}
	})
	t.Run("nil emit", func(t *testing.T) {
		stats := Run(context.Background(), 1, 0, sliceProducer([]int{1}),
			func(ctx context.Context, job int, emit func(int)) error { return nil }, nil)
		if stats.FirstError == nil {
			t.Error("FirstError = nil, want an error")
		}
	})
}

// TestRunCountsProcessErrors 确认框架层失败被计数并保留第一个错误，
// 同时不影响其它 job 继续处理（单个目标失败不能拖垮整体）。
func TestRunCountsProcessErrors(t *testing.T) {
	jobs := make([]int, 10)
	for i := range jobs {
		jobs[i] = i
	}

	stats := Run(context.Background(), 3, 0,
		sliceProducer(jobs),
		func(ctx context.Context, job int, emit func(int)) error {
			if job%3 == 0 {
				return fmt.Errorf("boom %d", job)
			}
			emit(job)
			return nil
		},
		func(int) {},
	)

	if stats.Errors == 0 {
		t.Fatal("Errors = 0, want > 0")
	}
	if stats.FirstError == nil {
		t.Fatal("FirstError = nil, want set")
	}
	if stats.Processed+stats.Errors != stats.Submitted {
		t.Errorf("Processed(%d) + Errors(%d) != Submitted(%d)",
			stats.Processed, stats.Errors, stats.Submitted)
	}
	if stats.Processed == 0 {
		t.Error("Processed = 0, want the non-failing jobs still processed")
	}
}

// TestRunEmitPanicIsContained 确认一个坏消费者不会拖垮整个扫描。
func TestRunEmitPanicIsContained(t *testing.T) {
	jobs := []int{1, 2, 3, 4, 5}

	stats := Run(context.Background(), 2, 0,
		sliceProducer(jobs),
		func(ctx context.Context, job int, emit func(int)) error {
			emit(job)
			return nil
		},
		func(r int) {
			if r == 3 {
				panic("consumer exploded")
			}
		},
	)

	if stats.Processed != len(jobs) {
		t.Errorf("Processed = %d, want %d (panic must not stop the pool)", stats.Processed, len(jobs))
	}
	if stats.Errors == 0 {
		t.Error("Errors = 0, want the emit panic counted")
	}
}

// ---------------------------------------------------------------------------
// Run：取消语义
// ---------------------------------------------------------------------------

// TestRunCooperativeProducerStopsOnCancel 覆盖"生产者尊重取消"的路径。
//
// 关键点：已经投喂但没处理的 job 必须计入 Skipped，
// 这样上层才能知道"这次测量不完整"，而不是拿残缺统计当完整结果。
func TestRunCooperativeProducerStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const jobCount = 1000

	var processed int32
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	stats := Run(ctx, 4, 0,
		func(ctx context.Context, send func(int) bool) int {
			sent := 0
			for i := 0; i < jobCount; i++ {
				if !send(i) {
					return sent
				}
				sent++
			}
			return sent
		},
		func(ctx context.Context, job int, emit func(int)) error {
			// 模拟耗时处理，让取消有机会在中间发生。
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&processed, 1)
			emit(job)
			return nil
		},
		func(int) {},
	)

	if !stats.Canceled {
		t.Error("Canceled = false, want true")
	}
	if stats.Processed >= jobCount {
		t.Errorf("Processed = %d, want < %d (canceled early)", stats.Processed, jobCount)
	}
	if got := int(atomic.LoadInt32(&processed)); got > stats.Processed {
		t.Errorf("started %d jobs but Processed = %d; stats must not over-report", got, stats.Processed)
	}
	// 不完整必须可以从统计里看出来。
	if stats.Completeness() >= 1 {
		t.Errorf("Completeness() = %v, want < 1 for an interrupted run", stats.Completeness())
	}
	// 每个被投喂的 job 要么处理了、要么跳过了，不能凭空消失。
	if stats.Processed+stats.Skipped > stats.Submitted {
		t.Errorf("Processed(%d) + Skipped(%d) > Submitted(%d)",
			stats.Processed, stats.Skipped, stats.Submitted)
	}
}

// TestRunCancelUnblocksProducer 覆盖"生产者不尊重取消"的危险路径。
//
// 如果生产者在一个巨大的列表上无脑 send（不看 ctx），
// 取消后队列很快填满，此时必须有机制让它停下来，
// 否则整个进程会卡死——这是 worker pool 最容易被忽略的死锁点。
func TestRunCancelUnblocksProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	const jobCount = 100000

	var processed int32
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	done := make(chan RunStats, 1)
	go func() {
		done <- Run(ctx, 2, 1, // 故意用很小的队列，让生产者很快被堵住
			func(ctx context.Context, send func(int) bool) int {
				sent := 0
				for i := 0; i < jobCount; i++ {
					// 这里**故意**不检查 ctx：模拟写错的生产者。
					send(i)
					sent++
				}
				return sent
			},
			func(ctx context.Context, job int, emit func(int)) error {
				atomic.AddInt32(&processed, 1)
				time.Sleep(time.Millisecond)
				emit(job)
				return nil
			},
			func(int) {},
		)
	}()

	select {
	case stats := <-done:
		if !stats.Canceled {
			t.Error("Canceled = false, want true")
		}
		if stats.Processed >= jobCount {
			t.Errorf("Processed = %d, want a small number after cancel", stats.Processed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation: the producer deadlocked")
	}
}

// TestRunCancelBeforeStart 确认"还没开始就取消"不会处理任何 job。
func TestRunCancelBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stats := Run(ctx, 4, 0,
		sliceProducer([]int{1, 2, 3, 4, 5}),
		func(ctx context.Context, job int, emit func(int)) error {
			t.Error("process called for a canceled run")
			return nil
		},
		func(int) {},
	)

	if stats.Processed != 0 {
		t.Errorf("Processed = %d, want 0", stats.Processed)
	}
	if !stats.Canceled {
		t.Error("Canceled = false, want true")
	}
}

// TestRunCanceledDoesNotHangWithNoConsumer 确认取消后结果被安全丢弃。
func TestRunCanceledDoesNotHangWithNoConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan RunStats, 1)
	go func() {
		done <- Run(ctx, 4, 0,
			sliceProducer(make([]int, 1000)),
			func(ctx context.Context, job int, emit func(int)) error {
				emit(job)
				return nil
			},
			func(int) {
				// 消费者什么都不做，模拟"调用方已经走了"。
			},
		)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung after cancellation")
	}
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// startLocalListeners 启动 n 个本机监听，返回可探测的目标。
func startLocalListeners(t *testing.T, n int) ([]model.Target, func()) {
	t.Helper()

	var listeners []net.Listener
	var targets []model.Target

	for i := 0; i < n; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen #%d: %v", i, err)
		}
		listeners = append(listeners, listener)

		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}(listener)

		addrPort := listener.Addr().(*net.TCPAddr)
		target, err := model.NewTargetFromStrings(addrPort.IP.String(), addrPort.Port)
		if err != nil {
			t.Fatalf("target: %v", err)
		}
		targets = append(targets, target)
	}

	cleanup := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	return targets, cleanup
}

// TestRunnerStreamsResultsAndAggregatesStats 用真实本机监听验证完整链路：
// 目标 -> worker pool -> 真实 TCP 握手 -> 结果流 -> 统计。
func TestRunnerStreamsResultsAndAggregatesStats(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 5)
	defer cleanup()

	runner := NewRunner(RunnerConfig{
		Probe:     Config{Workers: 3, Timeout: 2 * time.Second},
		QueueSize: 2,
	})

	handle := runner.Start(context.Background(), targets)

	var results []ProbeResult
	for result := range handle.Results() {
		results = append(results, result)
	}
	stats := handle.Wait()

	if len(results) != len(targets) {
		t.Fatalf("results = %d, want %d", len(results), len(targets))
	}
	if stats.Total != len(targets) {
		t.Errorf("Total = %d, want %d", stats.Total, len(targets))
	}
	if stats.Completed != len(targets) {
		t.Errorf("Completed = %d, want %d", stats.Completed, len(targets))
	}
	if stats.Success != len(targets) {
		t.Errorf("Success = %d, want %d (all listeners are live)", stats.Success, len(targets))
	}
	if stats.Failed != 0 {
		t.Errorf("Failed = %d, want 0", stats.Failed)
	}
	if stats.Interrupted {
		t.Error("Interrupted = true, want false")
	}
	if stats.SuccessRate() != 1 {
		t.Errorf("SuccessRate() = %v, want 1", stats.SuccessRate())
	}
	if len(stats.LatencyMS) != len(targets) {
		t.Errorf("latency samples = %d, want %d", len(stats.LatencyMS), len(targets))
	}

	// 结果必须自洽，且端口/ID 与目标一致。
	seen := make(map[string]struct{}, len(results))
	for _, r := range results {
		if err := r.Valid(); err != nil {
			t.Errorf("invalid result: %v (%+v)", err, r)
		}
		if _, dup := seen[r.TargetID]; dup {
			t.Errorf("duplicate result for %s", r.TargetID)
		}
		seen[r.TargetID] = struct{}{}
	}
}

// TestRunnerCountsFailuresAndLoss 验证失败被正确分类与统计。
//
// 用两个"确定失败"的目标（关闭的端口）与一个成功目标混在一起，
// 确认失败不会影响成功目标，统计也分得清。
func TestRunnerCountsFailuresAndLoss(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 1)
	defer cleanup()

	// 找一个确定空闲的端口拿出来然后关闭监听。
	probeListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := probeListener.Addr().(*net.TCPAddr)
	if err := probeListener.Close(); err != nil {
		t.Fatal(err)
	}
	failedTarget, err := model.NewTargetFromStrings(closedAddr.IP.String(), closedAddr.Port)
	if err != nil {
		t.Fatal(err)
	}

	all := append(append([]model.Target{}, targets...), failedTarget)

	runner := NewRunner(RunnerConfig{
		Probe: Config{Workers: 2, Timeout: time.Second},
	})
	stats := runner.Run(context.Background(), all)

	if stats.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", stats.Completed)
	}
	if stats.Success != 1 {
		t.Errorf("Success = %d, want 1", stats.Success)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.SuccessRate() != 0.5 {
		t.Errorf("SuccessRate() = %v, want 0.5", stats.SuccessRate())
	}
	if len(stats.ErrorCounts) == 0 {
		t.Error("ErrorCounts is empty, want the failure classified")
	}
	// 失败分类必须是"算作线路证据"的那一类（这里是 refused 或 timeout）。
	for kind := range stats.ErrorCounts {
		if !kind.CountsTowardLoss() {
			t.Errorf("classified as %q, which must not count toward loss for a real dead port", kind)
		}
	}
}

// TestRunnerInterruptedMarksIncomplete 确认中断会被如实反映在统计里。
func TestRunnerInterruptedMarksIncomplete(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 1)
	defer cleanup()

	// 造一批"会超时"的目标：用不可路由地址 + 极短超时。
	// 这样取消一定发生在处理中间。
	var jobs []model.Target
	jobs = append(jobs, targets...)
	for i := 0; i < 50; i++ {
		target, err := model.NewTargetFromStrings("192.0.2.1", 10000+i)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, target)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	runner := NewRunner(RunnerConfig{
		Probe: Config{Workers: 2, Timeout: 500 * time.Millisecond},
	})
	stats := runner.Run(ctx, jobs)

	if stats.Completed >= len(jobs) {
		t.Errorf("Completed = %d, want < %d after cancellation", stats.Completed, len(jobs))
	}
	if !stats.Interrupted {
		t.Error("Interrupted = false, want true")
	}
	if stats.Total != len(jobs) {
		t.Errorf("Total = %d, want %d", stats.Total, len(jobs))
	}
}

// TestRunnerDefaultQueueSize 确认默认队列大小由并发数推导。
func TestRunnerDefaultQueueSize(t *testing.T) {
	runner := NewRunner(RunnerConfig{Probe: Config{Workers: 7}})
	if runner.queue != 14 {
		t.Errorf("queue = %d, want 14 (workers*2)", runner.queue)
	}

	runner = NewRunner(RunnerConfig{Probe: Config{Workers: 3}, QueueSize: 5})
	if runner.queue != 5 {
		t.Errorf("queue = %d, want the configured 5", runner.queue)
	}
}

// TestRunnerEmptyTargets 确认空目标列表不会挂死。
func TestRunnerEmptyTargets(t *testing.T) {
	runner := NewRunner(RunnerConfig{Probe: DefaultConfig()})
	stats := runner.Run(context.Background(), nil)

	if stats.Total != 0 || stats.Completed != 0 {
		t.Errorf("stats = %+v, want zero", stats)
	}
	if stats.Interrupted {
		t.Error("Interrupted = true, want false for an empty run")
	}
}

// TestStatsRates 覆盖统计比例函数的边界情况。
func TestStatsRates(t *testing.T) {
	empty := Stats{}
	if empty.SuccessRate() != 0 || empty.FailureRate() != 0 || empty.LossRate() != 0 {
		t.Error("empty stats should report 0 rates")
	}

	stats := Stats{
		Total:     10,
		Completed: 10,
		Success:   7,
		Failed:    3,
		ErrorCounts: map[ErrorType]int{
			ErrorTypeTimeout:  2,
			ErrorTypeCanceled: 1, // 不计入丢包
		},
	}
	if got := stats.SuccessRate(); got != 0.7 {
		t.Errorf("SuccessRate() = %v, want 0.7", got)
	}
	if got := stats.FailureRate(); got != 0.3 {
		t.Errorf("FailureRate() = %v, want 0.3", got)
	}
	// 丢包率只算 timeout，canceled 是主动放弃。
	if got := stats.LossRate(); got != 0.2 {
		t.Errorf("LossRate() = %v, want 0.2", got)
	}
}

// TestRunnerAbandonedResultsDoesNotHang 是回归测试。
//
// Runner.Start 最初忘记把结果送进 results channel，导致
// `for range handle.Results()` 立刻结束——调用方会以为"测完了"，
// 而实际上什么都没测到。这个测试锁住"结果确实被交付"。
//
// 同时验证另一半：调用方如果**提前**停止读取，运行必须能结束，
// 而不是让 worker 卡在无人接收的 channel 上（那会让 Ctrl+C 失效）。
func TestRunnerAbandonedResultsDoesNotHang(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 4)
	defer cleanup()

	// 造一批目标，让结果数量明显多于 channel 缓冲。
	jobs := make([]model.Target, 0, 200)
	for i := 0; i < 200; i++ {
		jobs = append(jobs, targets[i%len(targets)])
	}

	runner := NewRunner(RunnerConfig{
		Probe:     Config{Workers: 4, Timeout: time.Second},
		QueueSize: 2, // 故意很小，逼出"无人消费"的场景
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handle := runner.Start(ctx, jobs)

	// 只读 3 条就放弃，然后取消，模拟"调用方提前退出"。
	read := 0
	for range handle.Results() {
		read++
		if read == 3 {
			cancel()
			break
		}
	}

	done := make(chan Stats, 1)
	go func() { done <- handle.Wait() }()

	select {
	case stats := <-done:
		if stats.Total != len(jobs) {
			t.Errorf("Total = %d, want %d", stats.Total, len(jobs))
		}
		if stats.Completed >= len(jobs) {
			t.Errorf("Completed = %d, want < %d after abandoning the results", stats.Completed, len(jobs))
		}
		if !stats.Interrupted {
			t.Error("Interrupted = false, want true")
		}
		// 已经测到但没能交付的结果必须被计数：否则汇总里的
		// Completed 会莫名其妙偏小，而上层无法分辨原因。
		if stats.Dropped == 0 {
			t.Error("Dropped = 0, want > 0 when the consumer abandoned the results")
		}
		// 会计关系：测到并交付 + 测到但丢弃 <= 目标总数。
		// 差额是"因为取消而根本没被处理"的目标（已投喂但未处理，
		// 或压根没被投喂出去），它们不计入 Completed，也不计入 Dropped。
		if stats.Completed+stats.Dropped > len(jobs) {
			t.Errorf("Completed(%d) + Dropped(%d) > %d",
				stats.Completed, stats.Dropped, len(jobs))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Runner hung after the consumer stopped reading")
	}
}

// TestRunnerResultsSliceIsCopied 是回归测试。
//
// 早期实现的 ProbeResult 切片是"按已投喂数量预分配容量、逐条 append"，
// 结果交付给调用方的是**共享底层数组**的切片。调用方一旦保存这些切片，
// 后续写入会覆盖它们的内容。修复方式是每条结果独立成切片。
func TestRunnerResultsSliceIsCopied(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 2)
	defer cleanup()

	runner := NewRunner(RunnerConfig{
		Probe:     Config{Workers: 1, Timeout: time.Second},
		QueueSize: 1,
	})

	handle := runner.Start(context.Background(), targets)
	var collected []ProbeResult
	for result := range handle.Results() {
		collected = append(collected, result)
	}
	handle.Wait()

	// 记录第一次拿到的值，然后强制触发更多分配，最后比对。
	if len(collected) == 0 {
		t.Fatal("no results")
	}
	before := collected[0]

	// 再跑一轮，模拟"后续写入"。
	other := runner.Start(context.Background(), targets)
	for range other.Results() {
	}
	other.Wait()

	if collected[0] != before {
		t.Errorf("previously collected result changed: %+v -> %+v", before, collected[0])
	}
}

// 调用方后续修改不会影响 Runner 内部状态，反之亦然。
func TestRunnerStatsAreCopied(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 2)
	defer cleanup()

	runner := NewRunner(RunnerConfig{Probe: Config{Workers: 1, Timeout: time.Second}})
	stats := runner.Run(context.Background(), targets)

	if len(stats.LatencyMS) == 0 {
		t.Fatal("no latency samples")
	}
	// 修改调用方拿到的快照：不应影响 Runner 的内部状态。
	stats.LatencyMS[0] = -1
	if stats.ErrorCounts == nil {
		stats.ErrorCounts = make(map[ErrorType]int)
	}
	stats.ErrorCounts[ErrorTypeOther] = 99

	again := runner.Run(context.Background(), targets)
	if again.LatencyMS[0] == -1 {
		t.Error("latency slice is shared between runs")
	}
	if _, ok := again.ErrorCounts[ErrorTypeOther]; ok {
		t.Error("error counts map is shared between runs")
	}
}

// TestRunnerConcurrentRunsAreIsolated 确认同一个 Runner 可以被并发使用
// （Prober 是只读的，因此这是安全的，也不应互相污染统计）。
func TestRunnerConcurrentRunsAreIsolated(t *testing.T) {
	targets, cleanup := startLocalListeners(t, 3)
	defer cleanup()

	runner := NewRunner(RunnerConfig{Probe: Config{Workers: 2, Timeout: time.Second}})

	var wg sync.WaitGroup
	statsCh := make(chan Stats, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statsCh <- runner.Run(context.Background(), targets)
		}()
	}
	wg.Wait()
	close(statsCh)

	for stats := range statsCh {
		if stats.Completed != len(targets) {
			t.Errorf("Completed = %d, want %d", stats.Completed, len(targets))
		}
		if stats.Success != len(targets) {
			t.Errorf("Success = %d, want %d", stats.Success, len(targets))
		}
	}
}

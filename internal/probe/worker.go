package probe

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// Worker Pool
// ---------------------------------------------------------------------------
//
// 需求第 12 条：必须采用 Job Queue -> Worker Pool -> Result Channel，
// 不能每个目标起一个 goroutine。
//
// 这里实现的是一个**有界**的 worker pool：
//
//	Producer (投喂队列) -> N 个固定 worker -> emit 结果
//
// 关键性质（都有对应测试）：
//
//  1. 同时运行的 worker 数永不超过 N；
//  2. 每个 job 恰好被处理一次（不重试、不漏掉已投喂的 job）；
//  3. ctx 取消后，已投喂但未处理的 job 被**放弃**并如实计数，
//     而不是假装测完——断点续测依赖这个数字；
//  4. 结果边产生边交付（emit），不需要把所有结果堆在内存里；
//  5. worker 与 producer 都可能在 ctx 取消后继续推进，
//     因此 emit 与 Producer 在取消后必须允许"丢弃"而不阻塞。

// ProcessFunc 处理一个 job，并通过 emit 交付结果。
//
// 语义约定（这决定了上层怎么写）：
//
//   - 返回 nil 表示该 job 被正常处理（即使业务结论是"失败"）；
//   - 返回非 nil 表示**框架层**失败（例如外部进程无法启动），
//     与"这个目标测失败"是两件事；
//   - 必须在 ctx 取消后尽快返回；emit 在取消后允许被丢弃。
type ProcessFunc[T any, R any] func(ctx context.Context, job T, emit func(R)) error

// Producer 投喂 job，返回实际投喂的数量。
//
// send 在 ctx 取消后会返回 false，Producer 应当立即返回。
// 返回的计数只用于统计与日志，不参与正确性判断。
type Producer[T any] func(ctx context.Context, send func(T) bool) int

// RunStats 是一次 worker pool 运行的统计。
type RunStats struct {
	// Submitted 是实际投喂进队列并被 worker 取走的 job 数。
	Submitted int

	// Processed 是被完整处理的 job 数（ProcessFunc 返回 nil）。
	Processed int

	// Emitted 是交付的结果条数。
	//
	// 正常情况下 Emitted == Processed；被取消时可能小于 Submitted。
	Emitted int

	// Skipped 是已投喂但因为 ctx 取消而未被处理的 job 数。
	Skipped int

	// Errors 是 ProcessFunc 返回错误的次数（框架层失败）。
	Errors int

	// FirstError 是第一个框架层错误，便于上层给出可操作的提示。
	FirstError error

	// Canceled 表示运行期间 ctx 被取消。
	Canceled bool

	// Elapsed 是整个 pool 的运行时长。
	Elapsed time.Duration
}

// Completeness 返回"已处理 / 已提交"的比例，用于判断本次测量是否完整。
//
// 没有任何 job 时返回 1（空任务视为完整）。
func (s RunStats) Completeness() float64 {
	if s.Submitted == 0 {
		return 1
	}
	return float64(s.Processed) / float64(s.Submitted)
}

// Run 用 workers 个并发 worker 处理 Producer 投喂的 job。
//
// queueSize 是 job 队列与结果交付的缓冲大小（<=0 时使用 workers*2）。
// Producer、process、emit 任一为 nil 时返回带 FirstError 的空统计，
// 而不是 panic。
func Run[T any, R any](
	ctx context.Context,
	workers int,
	queueSize int,
	produce Producer[T],
	process ProcessFunc[T, R],
	emit func(R),
) RunStats {
	start := time.Now()

	if workers <= 0 {
		workers = 1
	}
	if queueSize <= 0 {
		queueSize = workers * 2
	}

	if produce == nil || process == nil || emit == nil {
		return RunStats{
			FirstError: errors.New("probe: Run requires non-nil produce, process and emit"),
			Elapsed:    time.Since(start),
		}
	}

	jobs := make(chan T, queueSize)

	var (
		mu       sync.Mutex
		stats    RunStats
		workerWG sync.WaitGroup
	)

	// record 在锁内更新共享统计。
	record := func(fn func()) {
		mu.Lock()
		fn()
		mu.Unlock()
	}

	// safeEmit 在 ctx 取消后丢弃结果，避免 worker 卡在无人消费的 channel 上。
	safeEmit := func(r R) {
		defer func() {
			// emit 由调用方提供，可能自身会 panic（例如向已关闭 channel 发送）。
			// 这里把 panic 转成一次计数，保证一个坏消费者不会拖垮整个扫描。
			if rec := recover(); rec != nil {
				record(func() {
					stats.Errors++
					if stats.FirstError == nil {
						stats.FirstError = errors.New("probe: emit panicked")
					}
				})
			}
		}()

		record(func() { stats.Emitted++ })
		emit(r)
	}

	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()

			for {
				var job T
				select {
				case j, ok := <-jobs:
					if !ok {
						return
					}
					job = j
				case <-ctx.Done():
					return
				}

				record(func() { stats.Submitted++ })

				// 取到 job 之后再次检查取消：跳过处理并如实计入 Skipped。
				if ctx.Err() != nil {
					record(func() {
						stats.Skipped++
						stats.Canceled = true
					})
					continue
				}

				err := process(ctx, job, safeEmit)
				if err != nil {
					record(func() {
						stats.Errors++
						if stats.FirstError == nil {
							stats.FirstError = err
						}
					})
					continue
				}

				record(func() { stats.Processed++ })
			}
		}()
	}

	// 投喂完成后关闭队列，worker 读到关闭即退出。
	total := produce(ctx, func(job T) bool {
		select {
		case jobs <- job:
			return true
		case <-ctx.Done():
			return false
		}
	})
	close(jobs)

	workerWG.Wait()

	stats.Elapsed = time.Since(start)
	if stats.Submitted == 0 {
		// Producer 一个 job 都没投出去（例如空列表或立刻被取消）。
		// 用 Producer 的返回值兜底，避免统计里 Submitted 恒为 0。
		stats.Submitted = total
	}
	if ctx.Err() != nil {
		stats.Canceled = true
	}
	return stats
}

// ---------------------------------------------------------------------------
// Runner：把 worker pool 与探测器组合成可直接使用的入口
// ---------------------------------------------------------------------------

// RunnerConfig 是 Runner 的配置。
type RunnerConfig struct {
	// Probe 是单目标探测配置（并发数、超时、拨号实现）。
	Probe Config

	// QueueSize 是 job 队列大小。<=0 时使用 workers*2。
	QueueSize int
}

// Handle 是一次正在运行的批处理的句柄。
//
// 用法：
//
//	handle := runner.Start(ctx, targets)
//	for result := range handle.Results() {   // 边产生边消费
//	    ...
//	}
//	stats := handle.Wait()                   // 全部完成后的统计
//
// Results 必须在 Wait 之前（或同时）被持续读取：channel 满了之后
// 未被读取的结果会被丢弃，这是刻意的——宁可丢结果也不要让
// worker 因为退出的消费者而永久阻塞。
type Handle struct {
	results <-chan ProbeResult
	poolWG  *sync.WaitGroup
	stats   *batchStats
}

// Results 返回结果 channel，它在所有 worker 退出后关闭。
func (h *Handle) Results() <-chan ProbeResult { return h.results }

// Wait 等待全部 worker 退出并返回统计。
//
// 重复调用是安全的（内部等待同一个 WaitGroup，统计只做一次快照）。
func (h *Handle) Wait() Stats {
	if h.poolWG != nil {
		h.poolWG.Wait()
	}
	return h.stats.snapshot()
}

// batchStats 是 Handle 内部累积统计的载体。
type batchStats struct {
	mu          sync.Mutex
	completed   int
	success     int
	failed      int
	dropped     int
	errorCounts map[ErrorType]int
	latency     []float64
	total       int
	canceled    bool
}

func (b *batchStats) observe(r ProbeResult) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.completed++
	if r.Success {
		b.success++
		b.latency = append(b.latency, r.LatencyMS)
		return
	}
	b.failed++
	if b.errorCounts == nil {
		b.errorCounts = make(map[ErrorType]int)
	}
	b.errorCounts[r.ErrorType]++
}

// markDropped 记录一条"已经测到、但没能交给消费者"的结果。
//
// 必须显式计数：静默丢结果会让汇总看起来像"目标变少了"，
// 而上层无法分辨"本来就少"还是"被丢了"。
func (b *batchStats) markDropped() {
	b.mu.Lock()
	b.dropped++
	b.mu.Unlock()
}

func (b *batchStats) snapshot() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 复制切片与 map：调用方拿到的统计不应因为后续写入而变化。
	latency := make([]float64, len(b.latency))
	copy(latency, b.latency)

	var counts map[ErrorType]int
	if b.errorCounts != nil {
		counts = make(map[ErrorType]int, len(b.errorCounts))
		for k, v := range b.errorCounts {
			counts[k] = v
		}
	}

	return Stats{
		Total:       b.total,
		Completed:   b.completed,
		Success:     b.success,
		Failed:      b.failed,
		Dropped:     b.dropped,
		ErrorCounts: counts,
		LatencyMS:   latency,
		Interrupted: b.canceled || b.completed < b.total,
	}
}

// Runner 并发探测一批目标。
type Runner struct {
	prober *Prober
	queue  int
}

// NewRunner 创建 Runner。
func NewRunner(cfg RunnerConfig) *Runner {
	prober := New(cfg.Probe)

	queue := cfg.QueueSize
	if queue <= 0 {
		queue = prober.Config().Workers * 2
	}

	return &Runner{prober: prober, queue: queue}
}

// Prober 返回底层探测器。
func (r *Runner) Prober() *Prober { return r.prober }

// Start 开始并发探测并在后台运行，立即返回可消费的句柄。
//
// 返回后必须持续读取 handle.Results() 直到它关闭，然后调用 Wait 取统计。
func (r *Runner) Start(ctx context.Context, targets []model.Target) *Handle {
	results := make(chan ProbeResult, r.queue)
	batch := &batchStats{total: len(targets)}

	var poolWG sync.WaitGroup
	poolWG.Add(1)
	go func() {
		defer poolWG.Done()

		// 结果同时做两件事：更新统计、交给消费者。
		//
		// 消费者退出（ctx 取消，或调用方提前停止读取）时丢弃结果，
		// 绝不让 worker 卡在无人接收的 channel 上——
		// "宁可丢结果也不死锁"是有意的取舍：死锁会让 Ctrl+C 无效。
		// 但丢弃必须被**计数**（Stats.Dropped），否则汇总会看起来
		// 像"目标变少了"而无法解释。
		deliver := func(result ProbeResult) {
			batch.observe(result)

			select {
			case results <- result:
			case <-ctx.Done():
				batch.markDropped()
			}
		}

		stats := Run(ctx, r.prober.Config().Workers, r.queue,
			func(ctx context.Context, send func(model.Target) bool) int {
				sent := 0
				for _, target := range targets {
					if !send(target) {
						break
					}
					sent++
				}
				return sent
			},
			func(ctx context.Context, target model.Target, emit func(ProbeResult)) error {
				// 单个目标的任何失败都不会返回 error：
				// 失败本身就是结果。因此这里永远返回 nil。
				emit(r.prober.Probe(ctx, target))
				return nil
			},
			deliver,
		)
		batch.mu.Lock()
		batch.canceled = stats.Canceled
		batch.mu.Unlock()
		close(results)
	}()

	return &Handle{results: results, poolWG: &poolWG, stats: batch}
}

// Run 是 Start + 排空结果 + 返回统计的便捷封装。
//
// 适用于不需要流式处理的场景（例如命令行一次性输出汇总、
// 或者在测试里只关心统计）。
func (r *Runner) Run(ctx context.Context, targets []model.Target) Stats {
	handle := r.Start(ctx, targets)
	for range handle.Results() {
		// 必须排空：否则 worker 会因为结果 channel 满而停滞。
	}
	return handle.Wait()
}

package probe

import (
	"context"
	"sync"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/worker"
)

// ---------------------------------------------------------------------------
// Worker Pool
// ---------------------------------------------------------------------------
//
// 需求第 12 条：必须采用 Job Queue -> Worker Pool -> Result Channel，
// 不能每个目标起一个 goroutine。
//
// 实现位于 internal/worker：并发与取消语义只有一份实现，
// 因此 probe（TCP 探测）与 trace（线路跟踪）不会因为"各写一套池子"
// 而出现行为漂移。下面保留 probe 自己的名字作为薄适配器。
//
// 池子的关键性质（都有对应测试）：
//
//  1. 同时运行的 worker 数永不超过 N；
//  2. 每个 job 恰好被处理一次（不重试、不漏掉已投喂的 job）；
//  3. ctx 取消后，已投喂但未处理的 job 被**放弃**并如实计数，
//     而不是假装测完——断点续测依赖这个数字；
//  4. 结果边产生边交付（emit），不需要把所有结果堆在内存里；
//  5. worker 与 producer 都可能在 ctx 取消后继续推进，
//     因此 emit 与 Producer 在取消后必须允许"丢弃"而不阻塞。

// ProcessFunc / Producer / RunStats 是 internal/worker 中同名类型的别名。
//
// 用别名而不是重新定义：probe.Run 与 worker.Run 接受的就是同一组类型，
// 中间不需要任何转换，也不会出现"两套看起来一样但不通用"的类型。
type (
	// ProcessFunc 见 internal/worker.ProcessFunc。
	ProcessFunc[T any, R any] = worker.ProcessFunc[T, R]

	// Producer 见 internal/worker.Producer。
	Producer[T any] = worker.Producer[T]

	// RunStats 见 internal/worker.RunStats。
	RunStats = worker.RunStats
)

// Run 用 workers 个并发 worker 处理 Producer 投喂的 job。
//
// 实现委托给 internal/worker：并发与取消语义只有一份实现，
// 因此 probe 与 trace（线路跟踪）不会因为"各写一套池子"而出现行为漂移。
// 这个包装保留下来是因为本包的测试与调用方习惯用 probe 的名字。
func Run[T any, R any](
	ctx context.Context,
	workers int,
	queueSize int,
	produce Producer[T],
	process ProcessFunc[T, R],
	emit func(R),
) RunStats {
	return worker.Run(ctx, workers, queueSize, produce, process, emit)
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

	// OnTarget 在每个目标**开始**被探测时调用（可为 nil）。
	//
	// 存在的意义是让界面能显示"正在测哪个 IP"：只靠结果回调
	// 只能看到已经测完的目标，看不到当前这一个。
	//
	// 它会被多个 worker **并发**调用，实现必须自己保证线程安全，
	// 并且必须尽快返回——它就处在探测的热路径上。
	OnTarget func(target model.Target)
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
	prober   *Prober
	queue    int
	onTarget func(target model.Target)
}

// NewRunner 创建 Runner。
func NewRunner(cfg RunnerConfig) *Runner {
	prober := New(cfg.Probe)

	queue := cfg.QueueSize
	if queue <= 0 {
		queue = prober.Config().Workers * 2
	}

	return &Runner{prober: prober, queue: queue, onTarget: cfg.OnTarget}
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
				// 通知"开始测这个目标"。放在 Probe 之前，
				// 这样界面看到的就是**正在测**的 IP，而不是已经测完的。
				//
				// 回调由调用方保证线程安全（多个 worker 会并发进入这里），
				// 而且必须很快返回——它挡在真正探测之前。
				if r.onTarget != nil {
					r.onTarget(target)
				}

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

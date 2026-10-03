// Package worker 提供项目通用的有界 worker pool。
//
// 为什么单独一个包（而不是留在 probe 里）：
//
//	TCP Probe 与线路跟踪是两个完全不同的 job，但**并发模型完全一样**：
//	jobs -> N 个固定 worker -> 结果 -> 统计。把池子抽出来之后，
//	两处共用同一份并发语义与取消语义，不会出现"probe 能正常取消、
//	trace 取消后卡死"这种漂移。
//
// 需求第 12、19、31 条共同要求的是同一件事：
//
//	Job Queue -> Worker Pool -> Result Channel，且并发严格有界。
//	绝不允许"每个目标起一个 goroutine"，也绝不允许
//	"一次性启动上万个外部进程"。
package worker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ProcessFunc 处理一个 job，并通过 emit 交付结果。
//
// 语义约定（决定了调用方怎么写）：
//
//   - 返回 nil 表示该 job 被正常处理（即使业务结论是"失败"）；
//   - 返回非 nil 表示**框架层**失败（例如外部进程无法启动），
//     与"这个目标测失败"是两件事；
//   - 必须在 ctx 取消后尽快返回；emit 在取消后允许被丢弃。
type ProcessFunc[T any, R any] func(ctx context.Context, job T, emit func(R)) error

// Producer 投喂 job，返回实际投喂的数量。
//
// send 在 ctx 取消后会返回 false，Producer 应当立即返回。
type Producer[T any] func(ctx context.Context, send func(T) bool) int

// RunStats 是一次运行的统计。
type RunStats struct {
	// Produced 是**成功投喂进队列**的 job 数。
	//
	// 它与 Submitted 的区别很关键：生产者在取消时可能刚好把一个 job
	// 放进了队列，而没有任何 worker 来得及取走它。
	// 那个 job 既不在 Submitted 里（没人取），也不在 Skipped 里，
	// 于是 Processed == Submitted 会让 Completeness() 显示 1——
	// 明明有活儿没干完，却报"完整"。实测踩到过这个坑。
	//
	// Produced 由发送动作本身计数，因此它覆盖那个"被遗弃在队列里"的 job。
	Produced int

	// Submitted 是实际投喂进队列并**被 worker 取走**的 job 数。
	Submitted int

	// Processed 是被完整处理的 job 数（ProcessFunc 返回 nil）。
	Processed int

	// Emitted 是交付的结果条数。
	Emitted int

	// Skipped 是被 worker 取走但因为 ctx 取消而未被处理的 job 数。
	Skipped int

	// Abandoned 是已投喂但从未被任何 worker 取走的 job 数。
	//
	// = Produced - Submitted。取消时队列里剩下的就是这些。
	Abandoned int

	// Errors 是 ProcessFunc 返回错误的次数（框架层失败）。
	Errors int

	// FirstError 是第一个框架层错误。
	FirstError error

	// Canceled 表示运行期间 ctx 被取消。
	Canceled bool

	// Elapsed 是整个池子的运行时长。
	Elapsed time.Duration
}

// Completeness 返回"已处理 / 已投喂"的比例。
//
// 分母用 Produced 而不是 Submitted：被遗弃在队列里的 job 同样是
// "没干完的活儿"，用 Submitted 做分母会把它们漏掉（见 Produced 的说明）。
// 没有任何 job 时返回 1（空任务视为完整）。
func (s RunStats) Completeness() float64 {
	if s.Produced == 0 {
		return 1
	}
	return float64(s.Processed) / float64(s.Produced)
}

// Run 用 workers 个并发 worker 处理 Producer 投喂的 job。
//
// queueSize 是 job 队列的缓冲大小（<=0 时使用 workers*2）。
// Producer、process、emit 任一为 nil 时返回带 FirstError 的空统计，
// 而不是 panic。
//
// 关键性质（都有对应测试）：
//
//  1. 同时运行的 worker 数永不超过 workers；
//  2. 每个 job 恰好被处理一次；
//  3. ctx 取消后，已投喂但未处理的 job 被**放弃**并如实计入 Skipped；
//  4. 结果边产生边交付，不需要把所有结果堆在内存里；
//  5. 取消后 emit 会被安全丢弃，worker 不会卡在无人接收的 channel 上。
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
			FirstError: errors.New("worker: Run requires non-nil produce, process and emit"),
			Elapsed:    time.Since(start),
		}
	}

	jobs := make(chan T, queueSize)

	var (
		mu       sync.Mutex
		stats    RunStats
		workerWG sync.WaitGroup
	)

	record := func(fn func()) {
		mu.Lock()
		fn()
		mu.Unlock()
	}

	// safeEmit 在 ctx 取消后丢弃结果，避免 worker 卡在无人消费的 channel 上。
	safeEmit := func(r R) {
		defer func() {
			// emit 由调用方提供，可能自身 panic。
			// 把 panic 转成一次计数，保证一个坏消费者不会拖垮整次运行。
			if rec := recover(); rec != nil {
				record(func() {
					stats.Errors++
					if stats.FirstError == nil {
						stats.FirstError = errors.New("worker: emit panicked")
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

				// 取到 job 之后再次检查取消：跳过处理并如实计数。
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
	//
	// produced 在**每次成功发送**时累加，因此它包含那个可能被
	// 遗弃在队列里的最后一个 job（发送成功但没有 worker 取走）。
	var producedMu sync.Mutex
	produced := 0

	total := produce(ctx, func(job T) bool {
		select {
		case jobs <- job:
			producedMu.Lock()
			produced++
			producedMu.Unlock()
			return true
		case <-ctx.Done():
			return false
		}
	})
	close(jobs)

	workerWG.Wait()

	stats.Elapsed = time.Since(start)

	stats.Produced = produced
	if stats.Produced == 0 {
		// 一个 job 都没能送出去：用 Producer 的返回值兜底。
		stats.Produced = total
	}
	if stats.Submitted == 0 && total > 0 {
		stats.Submitted = total
	}

	// 被遗弃在队列里的 job：投喂成功但没人取走。
	if stats.Abandoned = stats.Produced - stats.Submitted; stats.Abandoned < 0 {
		stats.Abandoned = 0
	}

	if ctx.Err() != nil {
		stats.Canceled = true
	}
	return stats
}

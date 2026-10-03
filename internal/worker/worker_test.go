package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 说明：池子的通用行为（并发上限、恰好一次、取消语义、生产者死锁）
// 由 internal/probe 的测试通过 probe.Run 覆盖——probe.Run 直接委托到这里。
// 本文件只覆盖 worker 独有的行为，避免把同一批测试写两遍。

// TestRunContainsEmitPanic 验证一个坏消费者不会拖垮整次运行。
//
// 这是 internal/probe 里那份实现**没有**保护的路径：emit 由调用方提供，
// 它可能因为向已关闭的 channel 发送而 panic。若不接住，
// 一个消费者的小 bug 会让整个扫描崩掉，而已经测到的结果全部丢失。
func TestRunContainsEmitPanic(t *testing.T) {
	jobs := []int{1, 2, 3, 4, 5}

	stats := Run(context.Background(), 2, 0,
		func(ctx context.Context, send func(int) bool) int {
			sent := 0
			for _, job := range jobs {
				if !send(job) {
					break
				}
				sent++
			}
			return sent
		},
		func(ctx context.Context, job int, emit func(int)) error {
			emit(job)
			return nil
		},
		func(value int) {
			if value == 3 {
				panic("consumer exploded")
			}
		},
	)

	if stats.Processed != len(jobs) {
		t.Errorf("Processed = %d, want %d (a emit panic must not stop the pool)", stats.Processed, len(jobs))
	}
	if stats.Errors == 0 {
		t.Error("Errors = 0, want the emit panic counted")
	}
	if stats.FirstError == nil {
		t.Error("FirstError = nil, want a recorded error")
	}
}

// TestRunRejectsNilArguments 验证参数缺失时返回错误而不是 panic。
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
		produce := func(ctx context.Context, send func(int) bool) int { return 0 }
		stats := Run(context.Background(), 1, 0, produce, nil, func(int) {})
		if stats.FirstError == nil {
			t.Error("FirstError = nil, want an error")
		}
	})

	t.Run("nil emit", func(t *testing.T) {
		produce := func(ctx context.Context, send func(int) bool) int { return 0 }
		stats := Run(context.Background(), 1, 0, produce,
			func(ctx context.Context, job int, emit func(int)) error { return nil }, nil)
		if stats.FirstError == nil {
			t.Error("FirstError = nil, want an error")
		}
	})
}

// TestRunClampsWorkerCount 验证 0 / 负数并发不会挂死。
func TestRunClampsWorkerCount(t *testing.T) {
	jobs := []int{1, 2, 3}

	for _, workers := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			stats := Run(context.Background(), workers, 0,
				func(ctx context.Context, send func(int) bool) int {
					for _, job := range jobs {
						if !send(job) {
							break
						}
					}
					return len(jobs)
				},
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

// TestRunRespectsWorkerCeiling 验证并发上限。
//
// 需求第 12、19 条：绝不能无限创建 goroutine。
func TestRunRespectsWorkerCeiling(t *testing.T) {
	jobs := make([]int, 200)
	for i := range jobs {
		jobs[i] = i
	}

	for _, workers := range []int{1, 2, 8, 32} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var active, peak int32

			stats := Run(context.Background(), workers, 4,
				func(ctx context.Context, send func(int) bool) int {
					sent := 0
					for _, job := range jobs {
						if !send(job) {
							break
						}
						sent++
					}
					return sent
				},
				func(ctx context.Context, job int, emit func(int)) error {
					current := atomic.AddInt32(&active, 1)
					for {
						old := atomic.LoadInt32(&peak)
						if current <= old || atomic.CompareAndSwapInt32(&peak, old, current) {
							break
						}
					}
					// 制造重叠窗口，否则 worker 会串行完成，测不出上限。
					time.Sleep(200 * time.Microsecond)
					atomic.AddInt32(&active, -1)
					emit(job)
					return nil
				},
				func(int) {},
			)

			if stats.Processed != len(jobs) {
				t.Fatalf("Processed = %d, want %d", stats.Processed, len(jobs))
			}
			if got := int(atomic.LoadInt32(&peak)); got > workers {
				t.Errorf("peak concurrency = %d, want <= %d", got, workers)
			}
		})
	}
}

// TestRunCancelUnblocksUncooperativeProducer 验证取消后不会死锁。
//
// 生产者如果无脑 send（不看 ctx 返回值），队列很快填满；
// 必须有机制让它停下来，否则整个进程卡死——这是池子最容易被忽略的死锁点。
func TestRunCancelUnblocksUncooperativeProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	const jobCount = 100000

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
					// 故意不检查返回值：模拟写错的生产者。
					send(i)
					sent++
				}
				return sent
			},
			func(ctx context.Context, job int, emit func(int)) error {
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

// TestRunProcessErrorDoesNotStopOthers 验证框架层失败不中断其它 job。
func TestRunProcessErrorDoesNotStopOthers(t *testing.T) {
	jobs := make([]int, 10)
	for i := range jobs {
		jobs[i] = i
	}

	stats := Run(context.Background(), 3, 0,
		func(ctx context.Context, send func(int) bool) int {
			for _, job := range jobs {
				if !send(job) {
					break
				}
			}
			return len(jobs)
		},
		func(ctx context.Context, job int, emit func(int)) error {
			if job%3 == 0 {
				return errors.New("framework failure")
			}
			emit(job)
			return nil
		},
		func(int) {},
	)

	if stats.Errors == 0 {
		t.Fatal("Errors = 0, want > 0")
	}
	if stats.Processed == 0 {
		t.Error("Processed = 0, want the non-failing jobs still processed")
	}
	if stats.Processed+stats.Errors != stats.Submitted {
		t.Errorf("Processed(%d) + Errors(%d) != Submitted(%d)",
			stats.Processed, stats.Errors, stats.Submitted)
	}
}

// TestRunStatsCompleteness 覆盖比例函数的边界。
func TestRunStatsCompleteness(t *testing.T) {
	if got := (RunStats{}).Completeness(); got != 1 {
		t.Errorf("empty Completeness() = %v, want 1", got)
	}
	if got := (RunStats{Submitted: 4, Processed: 1}).Completeness(); got != 0.25 {
		t.Errorf("Completeness() = %v, want 0.25", got)
	}
}

// TestRunIsContentFreeOfGlobalState 验证池子可以并发复用。
//
// 两个 Run 同时跑，统计必须各自独立——池子不持有全局状态。
func TestRunIsContentFreeOfGlobalState(t *testing.T) {
	run := func(n int) RunStats {
		return Run(context.Background(), 4, 0,
			func(ctx context.Context, send func(int) bool) int {
				for i := 0; i < n; i++ {
					if !send(i) {
						break
					}
				}
				return n
			},
			func(ctx context.Context, job int, emit func(int)) error {
				emit(job)
				return nil
			},
			func(int) {},
		)
	}

	var wg sync.WaitGroup
	results := make([]RunStats, 4)
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = run(50)
		}(i)
	}
	wg.Wait()

	for i, stats := range results {
		if stats.Processed != 50 {
			t.Errorf("run %d Processed = %d, want 50", i, stats.Processed)
		}
		if stats.Errors != 0 {
			t.Errorf("run %d Errors = %d, want 0", i, stats.Errors)
		}
	}
}

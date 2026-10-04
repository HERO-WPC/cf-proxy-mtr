package service

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/probe"
)

// blockingDialer 是一个"每个目标都恰好耗时 timeout"的确定性拨号器。
//
// 为什么需要它：并发性只能靠**时间**证明，而真实网络的时间不可控
// （同样的目标可能立刻被拒、也可能等到超时）。用这个拨号器，
// 每个探测都必然占用完整的 timeout，于是
//
//	串行耗时 ≈ 目标数 × timeout
//	并行耗时 ≈ timeout
//
// 两者的差别足够大，测试不会因为网络抖动而随机失败。
func blockingDialer() probe.DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		// 等 ctx 的 deadline 到期。探测层用 context.WithTimeout
		// 包了一层，因此这里等 ctx.Done() 就等于"耗满 timeout"。
		<-ctx.Done()
		return nil, errors.New("blocking dialer: deadline reached")
	}
}

// countingDialer 记录并发峰值，用来证明"同时确实在跑多个探测"。
type countingDialer struct {
	running  atomic.Int64
	maxSeen  atomic.Int64
	inflight atomic.Int64
}

func (d *countingDialer) dial() probe.DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		cur := d.running.Add(1)
		d.inflight.Add(1)
		for {
			peak := d.maxSeen.Load()
			if cur <= peak || d.maxSeen.CompareAndSwap(peak, cur) {
				break
			}
		}
		defer func() {
			d.running.Add(-1)
			d.inflight.Add(-1)
		}()

		<-ctx.Done()
		return nil, errors.New("counting dialer: deadline reached")
	}
}

// TestRunCSVScanProbesInParallel 是并发性的**回归测试**。
//
// 它守的是一个真实出现过的 bug：探测被写成 `for targets` 串行循环，
// 60 个目标跑了 32 秒，而 `--workers 100` 照样被打印出来——
// 使用者以为并发生效，实际上完全没有。这类"声明了并发却串行执行"
// 的缺陷不会报错、不会有日志。
//
// 判据是**直接观测到的并发峰值**，而不是墙钟时间。
//
// 以前这里比对"并行耗时 < 串行耗时的一半"，那在单跑时没问题，
// 但整个测试套件并行跑二十多个包时会被资源争抢拖慢：实测在
// `go test ./...` 里偶发失败，单独跑却只用 317ms。用墙钟时间证明
// "是否并发"本身就是绕路——失败时也说不清是没并发还是机器忙。
//
// 拨号器直接数在飞的探测个数：串行实现永远只有 1，真并发必然 >1。
// 这个判据与机器快慢、负载都无关。
func TestRunCSVScanProbesInParallel(t *testing.T) {
	const (
		targets   = 8
		timeoutMS = 300
	)

	ports := make([]int, 0, targets)
	for i := 0; i < targets; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dialer := &countingDialer{}

	cfg := probe.DefaultConfig()
	cfg.Workers = targets // 每个目标一个 worker
	cfg.Timeout = timeoutMS * time.Millisecond
	cfg.Dialer = dialer.dial()

	start := time.Now()
	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:    out,
		probeOverride: &cfg,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}
	if result.Probed != targets {
		t.Fatalf("probed = %d, want %d", result.Probed, targets)
	}

	// 判据是**观测到的并发峰值**：串行实现永远只有 1 个探测在飞，
	// 真并发必然大于 1。这与机器快慢、当前负载都无关，
	// 而墙钟断言在整套测试并行跑时会被拖慢而误报。
	peak := dialer.maxSeen.Load()
	if peak < 2 {
		t.Errorf("peak concurrent probes = %d, want >1: the probes are not running concurrently "+
			"(workers=%d, targets=%d)", peak, cfg.Workers, targets)
	}

	// 时间只作为信息记录，不再作为判据。
	serialFloor := time.Duration(targets) * cfg.Timeout
	t.Logf("%d 个目标 / %d 并发 / 每个 %.0fms：耗时 %s（串行约 %s），并发峰值 %d",
		targets, cfg.Workers, cfg.Timeout.Seconds()*1000,
		elapsed.Round(time.Millisecond), serialFloor, peak)
}

// TestRunCSVScanRespectsWorkerLimit 验证并发数**不会超过**配置值。
//
// 与上一个测试互补：那个证明"确实并发了"，这个证明"没有并发过头"。
// 并发超限会打满对端与本机端口，是另一类真实事故。
func TestRunCSVScanRespectsWorkerLimit(t *testing.T) {
	const (
		targets   = 12
		workers   = 3
		timeoutMS = 250
	)

	ports := make([]int, 0, targets)
	for i := 0; i < targets; i++ {
		ports = append(ports, listenLocal(t))
	}
	svc := testService(t, writeCache(t, ports...))

	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dialer := &countingDialer{}

	cfg := probe.DefaultConfig()
	cfg.Workers = workers
	cfg.Timeout = timeoutMS * time.Millisecond
	cfg.Dialer = dialer.dial()

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:    out,
		probeOverride: &cfg,
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}
	if result.Probed != targets {
		t.Fatalf("probed = %d, want %d", result.Probed, targets)
	}

	if peak := dialer.maxSeen.Load(); peak > int64(workers) {
		t.Errorf("peak concurrency = %d, want at most %d", peak, workers)
	}
	if peak := dialer.maxSeen.Load(); peak < 2 {
		t.Errorf("peak concurrency = %d; the probes never overlapped", peak)
	}
}

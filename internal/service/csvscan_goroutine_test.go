package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
)

// 本文件守一个**协程生命周期**的缺陷。
//
// == 缺陷是什么 ==
//
// 前缀抓取为了与 TCP 探测并行，是在探测**之前**就启动的后台协程
// （见 asnprefix.Warmer）。但"跟踪引擎不可用"这条路径以前不碰这个
// 协程就返回了，于是它继续跑：白下一次整表（约 8.6 MB），
// 而且**在 RunCSVScan 返回之后继续往调用方的日志回调里写字**。
//
// 后者是一个真实的数据竞争。CI 的 `-race` 报的就是它：
//
//	Read  at ... bytes.Buffer.String()      cli_test.go:40（测试在读缓冲区）
//	Write at ... fmt.Fprintf → Logf          cmd_scan.go:295
//	             service.log → asnprefix.Load ← 后台预热协程
//
// 更糟的是那次写入可能落在一个已经被丢弃的缓冲区上。
//
// == 为什么不用 -race 测 ==
//
// 本项目的开发环境没有 C 工具链，跑不了 -race（那需要 CGO）。
// 但这件事可以在**行为层面**确定性地测：让前缀源慢一点，
// 然后断言"返回之后日志条数不再增长"。修好之前它必然失败。

// delayedPrefixServer 是一个"要等一会儿才回答"的前缀数据源。
//
// 它尊重请求的 context：这样被取消时能立刻返回，
// 于是 Cancel 不会为了等它而白白阻塞。
func delayedPrefixServer(t *testing.T, delay time.Duration, hits *int32) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		select {
		case <-time.After(delay):
			// 一个合法但空的全量映射表：让 Load 走完它的解析路径。
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte{0x1f, 0x8b, 0x08, 0x00}) // gzip magic，内容不完整
		case <-r.Context().Done():
			return
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// countingLogf 记录日志条数，供"返回后是否还在写"的断言使用。
type countingLogf struct {
	mu    sync.Mutex
	lines []string
}

func (c *countingLogf) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, format)
}

func (c *countingLogf) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

// TestNoBackgroundFetchAfterScanReturns 验证扫描返回后不再有后台请求。
//
// 这条测试针对的是"后台协程比函数活得久"这个缺陷，而判据是**请求次数**
// 而不是日志条数。
//
// 为什么不用日志条数：`asnprefix.Load` 只在**最终选定数据源**时才记一条，
// 而那要等重试耗尽与 120 秒的下载超时。用它当断言要么等两分钟，
// 要么窗口太短看不见——实测我先写的就是那样：窗口内日志根本没变，
// 于是"修复被禁用"时测试照样通过，等于没有护栏。
//
// 请求次数是即时的：抓取一旦被取消就不会再有新请求。它衡量的正是
// 真正的要求——后台工作停了没有。协程退了就不可能再往调用方的
// 输出里写字，CI 的 -race 覆盖的是这个推论的另一半。
func TestNoBackgroundFetchAfterScanReturns(t *testing.T) {
	const fetchDelay = 200 * time.Millisecond

	var hits int32
	server := delayedPrefixServer(t, fetchDelay, &hits)

	ports := []int{listenLocal(t)}
	out := filepath.Join(t.TempDir(), "results.csv")

	logf := &countingLogf{}
	svc := newServiceForTest(t, logf.logf, writeCache(t, ports...))

	cfg := probe.DefaultConfig()
	cfg.Timeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:    out,
		probeOverride: &cfg,
		Trace:         true,
		// 引擎不可用：这条路径就是缺陷发生的地方。
		TraceConfig: TraceOptions{Binary: "definitely-not-installed-nexttrace-xyz"},
		// 前缀源很慢，保证扫描返回时抓取还在进行中。
		ASNPrefixOptions: asnprefix.Options{BulkURL: server.URL, Dir: t.TempDir(), TTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}
	if result.TraceUnavailable == "" {
		t.Fatal("expected the trace engine to be unavailable, so this test exercises that path")
	}

	// 等待时间要盖过一次重试退避（fetchBackoff 是 400ms），
	// 否则"还在重试"与"已经停了"分不出来。
	afterReturn := atomic.LoadInt32(&hits)
	time.Sleep(1500 * time.Millisecond)
	settled := atomic.LoadInt32(&hits)

	if settled != afterReturn {
		t.Errorf("prefix source was requested %d more time(s) after RunCSVScan returned "+
			"(%d -> %d): a background goroutine is still working after the caller moved on",
			settled-afterReturn, afterReturn, settled)
	}
}

// TestScanCancelsPrefixFetchWhenTraceIsUnavailable 验证取消**不会变成等下载**。
//
// 这条守的不是"有没有取消"（那是上一条的职责），而是一个容易写坏的地方：
// Cancel 会等待后台协程退出，如果它只是把 ctx 一取消就完事、而下载
// 并不知道自己被取消，那这个等待就会变成"照样等满整张表"——
// 缺陷从"白下 8.6 MB"变成"白等几十秒"，对使用者一样糟。
//
// 所以这里用一个 3 秒的源，断言扫描明显快于它。
func TestScanCancelsPrefixFetchWhenTraceIsUnavailable(t *testing.T) {
	const fetchDelay = 3 * time.Second

	var hits int32
	server := delayedPrefixServer(t, fetchDelay, &hits)

	ports := []int{listenLocal(t)}
	out := filepath.Join(t.TempDir(), "results.csv")

	logf := &countingLogf{}
	svc := newServiceForTest(t, logf.logf, writeCache(t, ports...))

	cfg := probe.DefaultConfig()
	cfg.Timeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:    out,
		probeOverride: &cfg,
		Trace:         true,
		TraceConfig:   TraceOptions{Binary: "definitely-not-installed-nexttrace-xyz"},
		ASNPrefixOptions: asnprefix.Options{
			BulkURL: server.URL, Dir: t.TempDir(), TTL: time.Hour,
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	// 抓取要 3 秒。既然用不到，返回就不该等到它结束。
	// 留足余量：这里要证明的是"没有傻等"，不是精确的耗时。
	if elapsed > fetchDelay {
		t.Errorf("scan took %s although the prefix fetch (%s) is useless here "+
			"and should have been cancelled", elapsed.Round(time.Millisecond), fetchDelay)
	}
}

// newServiceForTest 与 testService 相同，但允许注入日志回调。
func newServiceForTest(t *testing.T, logf func(string, ...any), cachePath string) *Service {
	t.Helper()

	dir := t.TempDir()
	svc := testService(t, cachePath)
	svc.opts.Logf = logf
	_ = dir
	return svc
}

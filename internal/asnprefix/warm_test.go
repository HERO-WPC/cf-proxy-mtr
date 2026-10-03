package asnprefix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件锁定"抓取要并发"与"预热要真的提前开始"。
//
// 为什么值得单独测：两者都是**性能性质**，功能上完全看不出来。
// 串行抓取照样能跑对，只是 TCP 扫完之后要干等 57 秒；
// 预热不生效也照样能跑对，只是那等待又回来了。
// 没有测试的话，某次重构把并发去掉不会有任何提示。

// TestLoadFetchesConcurrently 验证抓取是并发的。
//
// 用一个人为放慢的处理器 + 记录并发峰值：串行时峰值恒为 1。
func TestLoadFetchesConcurrently(t *testing.T) {
	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()

		// 人为放慢：并发才可能重叠，串行时峰值必然为 1。
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()

		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	// 12 个 ASN：足够看出并发，又不至于让测试变慢。
	asns := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		asns = append(asns, "AS"+itoaForTest(i))
	}

	resolver := Load(context.Background(), Options{
		Dir:         t.TempDir(),
		BaseURL:     server.URL,
		ASNs:        asns,
		HTTPClient:  server.Client(),
		Concurrency: 4,
	})

	if !resolver.Ready() {
		t.Fatal("resolver is not ready")
	}
	if asns, _, _, _ := resolver.Size(); asns != 12 {
		t.Errorf("loaded %d ASNs, want 12", asns)
	}

	mu.Lock()
	gotPeak := peak
	mu.Unlock()

	if gotPeak < 2 {
		t.Errorf("peak concurrency = %d; the fetches ran one at a time", gotPeak)
	}
	if gotPeak > 4 {
		t.Errorf("peak concurrency = %d, want at most the configured 4", gotPeak)
	}
}

// TestLoadConcurrencyDoesNotExceedLimit 验证并发不会超过配置值。
//
// 与上一个互补：那个证明"确实并发了"，这个证明"没有并发过头"。
func TestLoadConcurrencyDoesNotExceedLimit(t *testing.T) {
	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()

		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	asns := make([]string, 0, 16)
	for i := 1; i <= 16; i++ {
		asns = append(asns, "AS"+itoaForTest(i))
	}

	Load(context.Background(), Options{
		Dir:         t.TempDir(),
		BaseURL:     server.URL,
		ASNs:        asns,
		HTTPClient:  server.Client(),
		Concurrency: 2,
	})

	mu.Lock()
	gotPeak := peak
	mu.Unlock()

	if gotPeak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", gotPeak)
	}
}

// TestLoadConcurrentWritesAreSafe 验证并发写入 map 不丢数据。
//
// 本机跑不了 -race，因此用"全部 ASN 都必须就绪"来代替：
// 少了锁会导致 map 竞争，表现就是随机少几个 ASN。
func TestLoadConcurrentWritesAreSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	const count = 40
	asns := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		asns = append(asns, "AS"+itoaForTest(i))
	}

	resolver := Load(context.Background(), Options{
		Dir:         t.TempDir(),
		BaseURL:     server.URL,
		ASNs:        asns,
		HTTPClient:  server.Client(),
		Concurrency: 8,
	})

	loaded, _, _, _ := resolver.Size()
	if loaded != count {
		t.Errorf("loaded %d of %d ASNs; concurrent writes lost entries", loaded, count)
	}
}

// TestWarmDoesNotBlock 验证预热是后台进行的。
//
// 这是"TCP 完了不卡顿"的实现基础：Warm 必须立刻返回，
// 否则就等于把抓取搬到了调用点上。
func TestWarmDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-release // 卡住，直到测试放行
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	warmer := Warm(context.Background(), Options{
		Dir:        t.TempDir(),
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})

	// Warm 必须已经返回（我们就在这行），且抓取已经开始。
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() == 0 {
		t.Fatal("Warm returned but never started fetching")
	}

	// 还没放行，因此不可能就绪——这证明 Get 之前是真正在后台跑。
	if warmer.Ready() {
		t.Error("Ready() = true while the server is still holding the request")
	}

	close(release)

	// Get 会等待完成，且必须拿到数据。
	resolver := warmer.Get()
	if resolver == nil || !resolver.Ready() {
		t.Fatal("Get returned without usable data after the fetch was released")
	}
	if got := resolver.Match("1.1.1.1"); len(got) != 1 {
		t.Errorf("Match = %v, want one match", got)
	}
	if !warmer.Ready() {
		t.Error("Ready() = false after Get returned")
	}
}

// TestWarmGetIsIdempotent 验证重复 Get 安全且结果一致。
func TestWarmGetIsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	warmer := Warm(context.Background(), Options{
		Dir:        t.TempDir(),
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})

	first := warmer.Get()
	second := warmer.Get()
	if first != second {
		t.Error("Get returned different resolvers on repeated calls")
	}
}

// TestWarmNilSafe 验证 nil Warmer 不 panic。
//
// 调用方在很多分支下会拿到 nil（没开跟踪、明确关掉），
// 让它们到处判空只会增加噪声。
func TestWarmNilSafe(t *testing.T) {
	var warmer *Warmer

	if warmer.Ready() {
		t.Error("a nil warmer reported Ready()")
	}
	if got := warmer.Get(); got != nil {
		t.Errorf("a nil warmer returned %v", got)
	}
}

// TestWarmWithCanceledContextReturnsWhatItHas 验证取消后仍能用已有数据。
//
// 半份前缀比没有强：它至少能认出已经抓到的那些线路。
func TestWarmWithCanceledContextReturnsWhatItHas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"1.1.1.0/24"})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消

	warmer := Warm(ctx, Options{
		Dir:        t.TempDir(),
		BaseURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
	})

	// 不该卡住，也不该 panic。
	done := make(chan *Resolver, 1)
	go func() { done <- warmer.Get() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Get did not return after the context was canceled")
	}
}

// itoaForTest 生成 ASN 编号字符串（避免引入 strconv 只为测试用）。
func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

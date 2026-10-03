package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 事件广播器
// ---------------------------------------------------------------------------

// TestEventBrokerKeepsHistory 验证新订阅者能拿到历史。
//
// 页面刷新后日志面板不该是空的：用户需要看到刚才发生了什么。
func TestEventBrokerKeepsHistory(t *testing.T) {
	broker := newEventBroker()
	broker.Info("第一条")
	broker.Good("第二条")

	events, cancel := broker.Subscribe()
	defer cancel()

	got := drain(t, events, 2)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Text != "第一条" || got[1].Text != "第二条" {
		t.Errorf("history = %q, %q", got[0].Text, got[1].Text)
	}
}

// TestEventBrokerAssignsIncreasingSeq 验证序号递增。
//
// 前端靠序号去重：SSE 重连时后端会重发历史，
// 没有序号就会重复显示。
func TestEventBrokerAssignsIncreasingSeq(t *testing.T) {
	broker := newEventBroker()
	for i := 1; i <= 5; i++ {
		broker.Info("第 %d 条", i)
	}

	events, cancel := broker.Subscribe()
	defer cancel()

	got := drain(t, events, 5)
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not increasing: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
}

// TestEventBrokerTrimsHistory 验证历史上限生效。
//
// 长时间运行不能让内存随事件数无限增长。
func TestEventBrokerTrimsHistory(t *testing.T) {
	broker := newEventBroker()
	broker.limit = 10

	for i := 0; i < 100; i++ {
		broker.Info("第 %d 条", i)
	}

	recent := broker.Recent(0)
	if len(recent) != 10 {
		t.Fatalf("history length = %d, want 10", len(recent))
	}
	// 保留的应当是**最新**的 10 条。
	if !strings.Contains(recent[len(recent)-1].Text, "99") {
		t.Errorf("newest event = %q, want it to be #99", recent[len(recent)-1].Text)
	}
}

// TestEventBrokerEmitDoesNotBlock 验证发布不会被慢订阅者拖住。
//
// 事件是"告知"不是"数据"：卡住测量去送日志不可接受。
func TestEventBrokerEmitDoesNotBlock(t *testing.T) {
	broker := newEventBroker()

	// 订阅后完全不消费。
	_, cancel := broker.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			broker.Info("事件 %d", i)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a slow subscriber")
	}
}

// TestEventBrokerConcurrentEmitIsSafe 验证并发发布安全。
//
// OnTarget 会被多个 worker 并发调用，这条路径必须线程安全
// （本机用不了 -race，所以这里用并发压力代替）。
func TestEventBrokerConcurrentEmitIsSafe(t *testing.T) {
	broker := newEventBroker()

	events, cancel := broker.Subscribe()
	defer cancel()

	// 边发边收，模拟真实并发。
	var consumer sync.WaitGroup
	consumer.Add(1)
	go func() {
		defer consumer.Done()
		for range events {
		}
	}()

	var producers sync.WaitGroup
	for p := 0; p < 8; p++ {
		producers.Add(1)
		go func(id int) {
			defer producers.Done()
			for i := 0; i < 200; i++ {
				broker.Info("worker %d 事件 %d", id, i)
			}
		}(p)
	}
	producers.Wait()
	cancel()
	consumer.Wait()

	// 序号必须唯一（自增在同一把锁里做，重复就说明有数据竞争）。
	broker.mu.Lock()
	total := broker.seq
	history := len(broker.history)
	broker.mu.Unlock()

	if total != 1600 {
		t.Errorf("seq = %d, want 1600", total)
	}
	if history > broker.limit {
		t.Errorf("history = %d, exceeds limit %d", history, broker.limit)
	}
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// TestLogEndpointRequiresToken 验证日志接口也要 token。
func TestLogEndpointRequiresToken(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	for _, path := range []string{"/api/log", "/api/events/log"} {
		status, body := request(t, http.MethodGet, server.URL()+path, "", "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 (body=%s)", path, status, body)
		}
	}
}

// TestLogEndpointReturnsHistory 验证 /api/log 返回历史事件。
func TestLogEndpointReturnsHistory(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))
	server.events.Info("测试事件一")
	server.events.Warn("测试事件二")

	status, body := request(t, http.MethodGet, server.URL()+"/api/log", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", status, body)
	}

	var payload struct {
		Events []uiEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(payload.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(payload.Events))
	}
	if payload.Events[1].Level != levelWarn {
		t.Errorf("level = %q, want %q", payload.Events[1].Level, levelWarn)
	}
}

// TestScanEmitsLogEvents 验证一次扫描会产生完整的日志序列。
//
// 界面的日志面板完全依赖这条通路；它断了不会报错，
// 只会让面板一直空着——正是用户抱怨的那种"看不到在干什么"。
func TestScanEmitsLogEvents(t *testing.T) {
	port := listenLocal(t)
	server := newTestServer(t, writeCache(t, port))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":1500,"workers":2}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: status = %d (body=%s)", status, body)
	}
	waitIdle(t, server, 60*time.Second)

	events := server.events.Recent(0)
	if len(events) == 0 {
		t.Fatal("no log events were emitted")
	}

	var texts []string
	for _, event := range events {
		texts = append(texts, event.Text)
	}
	joined := strings.Join(texts, "\n")
	t.Logf("日志事件:\n%s", joined)

	// 必须包含：开始、正在测某个目标、阶段完成、总体完成。
	wants := []string{
		"开始测量",
		"正在测量",
		"TCP 探测完成",
		"测量完成",
	}
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Errorf("log is missing %q\n--- got ---\n%s", want, joined)
		}
	}

	// 必须出现被测目标的 ID，否则"当前测试的 IP"无从显示。
	if !strings.Contains(joined, "127.0.0.1:") {
		t.Errorf("log does not mention the target being measured:\n%s", joined)
	}
}

// TestScanStatusReportsCurrentTargetDuringScan 验证扫描期间
// /api/scan/status 会报告当前目标（页面刷新后靠它恢复显示）。
func TestScanStatusReportsCurrentTargetDuringScan(t *testing.T) {
	// 目标多点，保证扫描期间有足够时间查询状态。
	ports := make([]int, 0, 12)
	for i := 0; i < 12; i++ {
		ports = append(ports, listenLocal(t))
	}
	server := newTestServer(t, writeCache(t, ports...))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":3000,"workers":2}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: status = %d (body=%s)", status, body)
	}

	// 轮询等待"当前目标"出现。
	deadline := time.Now().Add(20 * time.Second)
	var current string
	for time.Now().Before(deadline) {
		_, body := request(t, http.MethodGet, server.URL()+"/api/scan/status", testToken, "", nil)
		var payload struct {
			Scanning      bool   `json:"scanning"`
			CurrentTarget string `json:"current_target"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err == nil && payload.CurrentTarget != "" {
			current = payload.CurrentTarget
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if current == "" {
		t.Error("current_target stayed empty during a scan; the UI could not show what is being measured")
	} else if !strings.HasPrefix(current, "127.0.0.1:") {
		t.Errorf("current_target = %q, want a 127.0.0.1 target", current)
	}

	waitIdle(t, server, 60*time.Second)

	// 扫描结束后必须清空：否则页面会一直显示一个早已测完的 IP。
	_, body = request(t, http.MethodGet, server.URL()+"/api/scan/status", testToken, "", nil)
	var after struct {
		CurrentTarget string `json:"current_target"`
	}
	if err := json.Unmarshal([]byte(body), &after); err != nil {
		t.Fatal(err)
	}
	if after.CurrentTarget != "" {
		t.Errorf("current_target = %q after the scan finished, want empty", after.CurrentTarget)
	}
}

// TestFailedScanEmitsErrorEvent 验证失败也会写进日志面板。
//
// 用户看不到失败原因就等于没有反馈——而界面是他们唯一的信息来源。
func TestFailedScanEmitsErrorEvent(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, _ := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"limit":-1}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	waitIdle(t, server, 30*time.Second)

	var texts []string
	for _, event := range server.events.Recent(0) {
		texts = append(texts, event.Level+" "+event.Text)
	}
	joined := strings.Join(texts, "\n")

	if !strings.Contains(joined, "error") {
		t.Errorf("no error-level event was emitted:\n%s", joined)
	}
	if !strings.Contains(joined, "开始测量") {
		t.Errorf("the scan start was not logged:\n%s", joined)
	}
}

// drain 从通道读取 n 条事件（带超时）。
func drain(t *testing.T, events <-chan uiEvent, n int) []uiEvent {
	t.Helper()

	out := make([]uiEvent, 0, n)
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case event, open := <-events:
			if !open {
				return out
			}
			out = append(out, event)
		case <-deadline:
			return out
		}
	}
	return out
}

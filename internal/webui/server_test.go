package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/applog"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

const testToken = "test-token-0123456789"

// newTestServer 起一个测试用的服务。
//
// listen 固定为 127.0.0.1:0（系统分配端口），因此不会与真实服务冲突。
func newTestServer(t *testing.T, cachePath string) *Server {
	t.Helper()

	dir := t.TempDir()
	svc := service.New(service.Options{
		DBPath:       filepath.Join(dir, "results.db"),
		IdentityPath: filepath.Join(dir, "collector.json"),
		Source: source.Config{
			URL:         source.DefaultURL,
			FallbackURL: "",
			CachePath:   cachePath,
			Retries:     0,
		},
		Logf: t.Logf,
	})

	server, err := New(Config{
		Listen:  "127.0.0.1:0",
		Service: svc,
		Logger:  applog.Discard(),
		Token:   testToken,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return server
}

// listenLocal 起一个真实的本机监听，返回端口。
func listenLocal(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// writeCache 写一份指向本机监听的目标缓存。
func writeCache(t *testing.T, ports ...int) string {
	t.Helper()

	data := make([]string, 0, len(ports))
	for _, port := range ports {
		data = append(data, fmt.Sprintf(
			`{"ip":"127.0.0.1","port":[%d],"latitude":"0","longitude":"0","country":"CN","city":"Hangzhou"}`,
			port))
	}
	body := fmt.Sprintf(
		`{"generated_at":"2026-10-03T00:00:00","list":{"ips":%d},"data":[%s]}`,
		len(ports), strings.Join(data, ","))

	parsed, err := source.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseJSON fixture: %v", err)
	}
	if err := model.ValidateAll(parsed.Targets); err != nil {
		t.Fatalf("fixture produced an invalid target: %v", err)
	}

	meta := parsed.Meta
	meta.URL = source.DefaultURL
	meta.Format = "json"

	path := filepath.Join(t.TempDir(), "all.json")
	if err := source.WriteCache(path, meta, parsed.Stats, parsed.Targets); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	return path
}

// request 发一个请求，返回状态码与响应体。
func request(t *testing.T, method, url string, token string, body string, mutate func(*http.Request)) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(payload)
}

// ---------------------------------------------------------------------------
// 访问控制
// ---------------------------------------------------------------------------

// TestAPIRequiresToken 验证没有 token 时所有 /api 都被拒绝。
//
// 单机部署也必须做这件事：绑在 127.0.0.1 上**不等于**只有本机能访问，
// 浏览器里任何网页都能向 127.0.0.1 发请求。
func TestAPIRequiresToken(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))
	base := server.URL()

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/stats"},
		{http.MethodGet, "/api/scan/status"},
		{http.MethodGet, "/api/events"},
		{http.MethodPost, "/api/scan"},
		{http.MethodPost, "/api/scan/stop"},
	}

	for _, endpoint := range endpoints {
		t.Run(endpoint.method+" "+endpoint.path, func(t *testing.T) {
			status, body := request(t, endpoint.method, base+endpoint.path, "", "", nil)
			if status != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body=%s)", status, body)
			}
		})
	}
}

// TestAPIRejectsWrongToken 验证错误的 token 被拒绝。
func TestAPIRejectsWrongToken(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	for _, wrong := range []string{"", "wrong", testToken + "x", testToken[:len(testToken)-1]} {
		status, _ := request(t, http.MethodGet, server.URL()+"/api/stats", wrong, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", wrong, status)
		}
	}
}

// TestAPIAcceptsCorrectToken 验证正确的 token 能通过。
func TestAPIAcceptsCorrectToken(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodGet, server.URL()+"/api/stats", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

// TestAPIAcceptsTokenInQuery 验证 token 也能从查询参数读。
//
// SSE 无法自定义请求头（EventSource 不支持），因此这条路径是必需的。
func TestAPIAcceptsTokenInQuery(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodGet,
		server.URL()+"/api/scan/status?token="+testToken, "", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

// TestRejectsNonLoopbackHost 验证非回环 Host 被拒绝（DNS rebinding 防护）。
//
// 攻击者可以让 evil.com 解析到 127.0.0.1：浏览器发出的请求看起来是访问
// 本机，Host 头却是 evil.com。只检查 token 不足以挡住这种情况，
// 因为页面可能是攻击者自己提供的。
func TestRejectsNonLoopbackHost(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodGet, server.URL()+"/api/stats", testToken, "",
		func(req *http.Request) { req.Host = "evil.example.com" })
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", status, body)
	}
}

// TestRejectsCrossOriginWrite 验证跨站发起的变更请求被拒绝（CSRF）。
//
// 同源策略不会阻止"跨站发起请求"，只阻止读取响应——
// 而启动一次扫描的副作用已经发生了。
func TestRejectsCrossOriginWrite(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken, "{}",
		func(req *http.Request) { req.Header.Set("Origin", "https://evil.example.com") })
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a cross-origin POST (body=%s)", status, body)
	}
}

// TestAllowsSameOriginWrite 验证同源请求正常。
func TestAllowsSameOriginWrite(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"limit":1,"timeout_ms":1000}`,
		func(req *http.Request) { req.Header.Set("Origin", server.URL()) })
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", status, body)
	}
	waitIdle(t, server, 30*time.Second)
}

// TestAllowsRequestWithoutOrigin 验证没有 Origin 的请求被放行。
//
// curl 之类的工具不会带 Origin。这类请求仍然需要 token，
// 因此放行不会降低安全性。
func TestAllowsRequestWithoutOrigin(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"limit":1,"timeout_ms":1000}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a token-bearing request without Origin (body=%s)",
			status, body)
	}
	waitIdle(t, server, 30*time.Second)
}

// ---------------------------------------------------------------------------
// 页面
// ---------------------------------------------------------------------------

// TestIndexServesPageWithoutToken 验证页面本身不需要 token。
//
// 页面里没有数据，token 是通过 URL 参数交给它的；
// 若页面也要 token，用户就无法"第一次打开"。
func TestIndexServesPageWithoutToken(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodGet, server.URL()+"/", "", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "cf-route-tester") {
		t.Error("the page does not look like our UI")
	}
	if !strings.Contains(body, "开始扫描") {
		t.Error("the page is missing the scan controls")
	}
}

// TestPageHasSecurityHeaders 验证安全响应头存在。
func TestPageHasSecurityHeaders(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	resp, err := http.Get(server.URL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get("Content-Security-Policy"); got == "" {
		t.Error("missing Content-Security-Policy")
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestUnknownPathIsNotFound 验证未知路径返回 404。
func TestUnknownPathIsNotFound(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, _ := request(t, http.MethodGet, server.URL()+"/nope", testToken, "", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// ---------------------------------------------------------------------------
// 扫描控制
// ---------------------------------------------------------------------------

// TestScanStartStopAndResult 验证能启动扫描、拿到结果、统计随之更新。
func TestScanStartStopAndResult(t *testing.T) {
	port := listenLocal(t)
	server := newTestServer(t, writeCache(t, port))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 启动。
	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":2000,"workers":2}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: status = %d, want 202 (body=%s)", status, body)
	}

	// 等它跑完。
	waitIdle(t, server, 60*time.Second)

	// 状态里应当有结果。
	status, body = request(t, http.MethodGet, server.URL()+"/api/scan/status", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status: status = %d", status)
	}
	var statusBody struct {
		Scanning bool         `json:"scanning"`
		Last     *scanOutcome `json:"last"`
	}
	if err := json.Unmarshal([]byte(body), &statusBody); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, body)
	}
	if statusBody.Scanning {
		t.Error("scanning = true after the scan finished")
	}
	if statusBody.Last == nil || statusBody.Last.Summary == nil {
		t.Fatalf("no last-scan summary: %s", body)
	}
	if got := statusBody.Last.Summary.ProbeSuccess; got != 1 {
		t.Errorf("probe_success = %d, want 1 (a real listener is accepting)", got)
	}
	if got := statusBody.Last.Summary.Stored; got != 1 {
		t.Errorf("stored = %d, want 1", got)
	}

	// 统计必须反映出落库的数据（这是"真的写进去了"的证据）。
	status, body = request(t, http.MethodGet, server.URL()+"/api/stats", testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("stats: status = %d", status)
	}
	var statsBody struct {
		Storage struct {
			Tables map[string]int64 `json:"Tables"`
		} `json:"Storage"`
	}
	if err := json.Unmarshal([]byte(body), &statsBody); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if got := statsBody.Storage.Tables["measurements"]; got != 1 {
		t.Errorf("measurements = %d, want 1", got)
	}

	_ = ctx
}

// TestConcurrentScanIsRejected 验证同时只允许一个扫描。
//
// 两个并发扫描对本工具毫无意义（网络与 CPU 都被打满），
// 而且会让本地数据库出现两个同时写入的会话。
func TestConcurrentScanIsRejected(t *testing.T) {
	// 用较多目标让第一次扫描持续足够久，能稳定地撞上第二次请求。
	ports := make([]int, 0, 8)
	for i := 0; i < 8; i++ {
		ports = append(ports, listenLocal(t))
	}
	server := newTestServer(t, writeCache(t, ports...))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":3000}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("first scan: status = %d (body=%s)", status, body)
	}

	// 立刻再发一次：应当被拒绝。
	status, body = request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":3000}`, nil)
	if status != http.StatusConflict {
		t.Errorf("second scan: status = %d, want 409 (body=%s)", status, body)
	}

	// 收尾。
	_, _ = request(t, http.MethodPost, server.URL()+"/api/scan/stop", testToken, "", nil)
	waitIdle(t, server, 60*time.Second)
}

// TestStopWithoutScanIsConflict 验证没有扫描时停止返回冲突而不是成功。
//
// 静默返回成功会让界面显示"已停止"，而实际上什么都没发生。
func TestStopWithoutScanIsConflict(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan/stop", testToken, "", nil)
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body=%s)", status, body)
	}
}

// TestScanRejectsGetMethod 验证用 GET 启动扫描被拒绝。
func TestScanRejectsGetMethod(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, _ := request(t, http.MethodGet, server.URL()+"/api/scan", testToken, "", nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// TestScanReportsUsageError 验证参数错误被映射成 400 而不是 500。
//
// 状态码错了会让界面把"用户填错了"显示成"程序出故障了"。
func TestScanReportsUsageError(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	// 负数 limit 是用法错误。
	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"limit":-1}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the scan runs in the background)", status)
	}

	waitIdle(t, server, 30*time.Second)

	// 后台执行的结果里应当是 usage 分类，而不是崩溃。
	_, body = request(t, http.MethodGet, server.URL()+"/api/scan/status", testToken, "", nil)
	var statusBody struct {
		Last *scanOutcome `json:"last"`
	}
	if err := json.Unmarshal([]byte(body), &statusBody); err != nil {
		t.Fatal(err)
	}
	if statusBody.Last == nil {
		t.Fatal("no outcome recorded")
	}
	if statusBody.Last.Kind != "usage" {
		t.Errorf("kind = %q, want usage (error=%q)", statusBody.Last.Kind, statusBody.Last.Error)
	}
}

// TestScanRejectsMalformedBody 验证格式错误的请求体被拒绝。
func TestScanRejectsMalformedBody(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"workers": not-json`, nil)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", status, body)
	}
}

// TestScanAcceptsEmptyBody 验证空请求体合法（全部走默认值）。
func TestScanAcceptsEmptyBody(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	status, body := request(t, http.MethodPost, server.URL()+"/api/scan?token="+testToken,
		"", "", nil)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", status, body)
	}
	waitIdle(t, server, 60*time.Second)
}

// ---------------------------------------------------------------------------
// 进度
// ---------------------------------------------------------------------------

// TestProgressHubBroadcasts 验证进度能广播给多个订阅者。
//
// 单订阅者的实现会在"打开两个标签页"时静默失效：
// 先连上的那个永远停在旧进度。
func TestProgressHubBroadcasts(t *testing.T) {
	hub := service.NewProgressHub()

	first, cancelFirst := hub.Subscribe()
	defer cancelFirst()
	second, cancelSecond := hub.Subscribe()
	defer cancelSecond()

	hub.Publish(testProgressEvent(3, 10))

	for name, ch := range map[string]<-chan scheduler.ProgressEvent{
		"first":  first,
		"second": second,
	} {
		select {
		case event := <-ch:
			if event.Completed != 3 {
				t.Errorf("%s received completed=%d, want 3", name, event.Completed)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s received nothing", name)
		}
	}
}

// TestProgressHubPublishDoesNotBlock 验证发布不会被慢订阅者拖住。
//
// 宁可丢事件也不能让扫描停下来等一个慢客户端。
func TestProgressHubPublishDoesNotBlock(t *testing.T) {
	hub := service.NewProgressHub()

	// 订阅后完全不消费，把缓冲区填满。
	_, cancel := hub.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 远超缓冲区容量。
		for i := 0; i < 500; i++ {
			hub.Publish(testProgressEvent(i, 1000))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

// TestProgressHubSnapshotForLateSubscriber 验证后连上的订阅者
// 立刻拿到当前状态，而不是空白等到下一次更新。
func TestProgressHubSnapshotForLateSubscriber(t *testing.T) {
	hub := service.NewProgressHub()
	hub.Publish(testProgressEvent(7, 10))

	late, cancel := hub.Subscribe()
	defer cancel()

	select {
	case event := <-late:
		if event.Completed != 7 {
			t.Errorf("late subscriber got completed=%d, want the snapshot 7", event.Completed)
		}
	case <-time.After(2 * time.Second):
		t.Error("late subscriber received no snapshot")
	}
}

// TestProgressHubUnsubscribeStopsDelivery 验证取消订阅后不再收到事件，
// 且不会 panic（向已关闭的通道发送会 panic）。
func TestProgressHubUnsubscribeStopsDelivery(t *testing.T) {
	hub := service.NewProgressHub()
	ch, cancel := hub.Subscribe()
	cancel()

	// 关闭后再发布：不得 panic。
	for i := 0; i < 50; i++ {
		hub.Publish(testProgressEvent(i, 100))
	}

	// 通道应当已关闭。
	select {
	case _, open := <-ch:
		if open {
			// 可能还有缓冲的事件，再读一次确认最终会关闭。
			select {
			case _, open2 := <-ch:
				if open2 {
					t.Error("channel is still open after cancel")
				}
			case <-time.After(time.Second):
				t.Error("channel did not close after cancel")
			}
		}
	case <-time.After(time.Second):
		t.Error("channel did not close after cancel")
	}
}

// TestScanPublishesProgressOverSSE 验证扫描进度真的能通过 SSE 送达。
//
// 这条通路断了不会报错，只会让界面永远停在 0%——
// 因此必须有一个"端到端真的收到了"的测试。
func TestScanPublishesProgressOverSSE(t *testing.T) {
	port := listenLocal(t)
	server := newTestServer(t, writeCache(t, port))

	// 订阅 SSE。
	req, err := http.NewRequest(http.MethodGet,
		server.URL()+"/api/events?token="+testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// 启动扫描。
	status, body := request(t, http.MethodPost, server.URL()+"/api/scan", testToken,
		`{"timeout_ms":1500}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: status = %d (body=%s)", status, body)
	}

	// 读取 SSE 流，直到看到 progress 事件或超时。
	//
	// 显式用一个 stopReader 通道结束读取协程：否则测试结束后它仍然阻塞在
	// Read 上，属于协程泄漏（在 -race 与长期运行的测试里会变成噪声）。
	events := make(chan string, 8)
	stopReader := make(chan struct{})
	defer close(stopReader)

	go func() {
		defer close(events)
		buf := make([]byte, 4096)
		for {
			select {
			case <-stopReader:
				return
			default:
			}

			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case events <- string(buf[:n]):
				case <-stopReader:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	deadline := time.After(45 * time.Second)
	sawProgress := false
	for !sawProgress {
		select {
		case chunk, open := <-events:
			if !open {
				t.Fatal("the SSE stream closed before any progress event")
			}
			if strings.Contains(chunk, "event: progress") {
				sawProgress = true
			}
		case <-deadline:
			t.Fatal("no progress event arrived over SSE within 45s")
		}
	}

	waitIdle(t, server, 60*time.Second)
}

// TestDoneIsOpenWhileRunning 验证"服务运行期间 Done() 不会触发"。
//
// 这条测试是为一个真实 bug 加的：done 通道如果忘了在 New 里创建，
// Done() 会返回一个已关闭的通道，于是调用方
// `select { case <-server.Done(): }` 立刻命中、认为服务已经退出——
// 表现为**服务启动后立刻关闭**，而日志里"正在监听"与"已停止"
// 的时间戳只差 1 毫秒，非常难从现象反推原因。
func TestDoneIsOpenWhileRunning(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	select {
	case <-server.Done():
		t.Fatal("Done() fired while the server is still serving; " +
			"a caller selecting on it would shut down immediately")
	case <-time.After(300 * time.Millisecond):
		// 正确：运行期间不触发。
	}

	// 服务必须真的还能响应（不是"通道没关但服务也没起来"）。
	status, _ := request(t, http.MethodGet, server.URL()+"/api/stats", testToken, "", nil)
	if status != http.StatusOK {
		t.Errorf("stats status = %d, want 200 while running", status)
	}
}

// TestDoneFiresAfterShutdown 验证关闭后 Done() 会触发。
//
// 与上一条是配对的两个方向：只测一个方向会漏掉"从来不发信号"
// 或"一开始就发信号"其中的一种。
func TestDoneFiresAfterShutdown(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	// 清理里会再关一次（幂等），这里显式提前关闭。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-server.Done():
		// 正确。
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not fire after Shutdown")
	}

	// 重复关闭不得 panic（closeOnce 的作用）。
	if err := server.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// TestAddrAndURLAreUsable 验证 URL 指向真正可以连上的地址。
//
// 绑定 0.0.0.0 时 Addr() 返回的是不可访问的地址，
// 而用户需要的是能点开的地址——两者混用会给出一个打不开的链接。
func TestAddrAndURLAreUsable(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	if server.Addr() == nil {
		t.Fatal("Addr() is nil after Start")
	}
	url := server.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("URL() = %q, want a loopback http URL", url)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url + "/")
	if err != nil {
		t.Fatalf("the URL returned by URL() is not reachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// PageURL 必须带上 token，否则用户打开后无法工作。
	if !strings.Contains(server.PageURL(), "token=") {
		t.Errorf("PageURL() = %q, want it to carry the token", server.PageURL())
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// testProgressEvent 构造一个进度事件。
func testProgressEvent(completed, total int) scheduler.ProgressEvent {
	return scheduler.ProgressEvent{
		Phase:     scheduler.PhaseProbe,
		Completed: completed,
		Total:     total,
	}
}

// waitIdle 等到不再有扫描在跑。
func waitIdle(t *testing.T, server *Server, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !server.isScanning() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the scan did not finish within %s", timeout)
}

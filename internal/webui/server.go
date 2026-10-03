// Package webui 提供本项目的图形界面：一个内置的 HTTP 服务 + 嵌入式页面。
//
// 为什么选"内置 Web 服务"而不是原生界面框架：
//
//   - 项目承诺单文件二进制、零运行时依赖。原生绑定要么需要 CGO
//     （破坏 CGO_ENABLED=0，也就破坏了交叉编译与"Windows 双击即用"），
//     要么需要随包分发平台运行库。
//   - Wails 之类的方案会把 npm 构建链带进仓库，
//     新贡献者必须先装 Node 才能构建。
//   - 前端用原生 HTML/CSS/JS 并通过 go:embed 打进二进制，
//     因此 clone 之后 `go build ./...` 依然就够了。
//
// 关于安全（**单机部署也需要**）：
//
// 把服务绑在 127.0.0.1 上**不等于**只有本机能访问。浏览器里任何一个
// 网页都能向 http://127.0.0.1:<port> 发请求，而本工具的能力是
// "对外发起大量网络连接"——被人诱导着发起一次扫描并非无害。
// 因此这里做了三件事：
//
//  1. 启动时生成**进程级随机 token**，所有 /api 请求都必须带上；
//  2. 校验 Origin/Referer，拒绝跨站发起的变更请求（CSRF）；
//  3. 只允许本机来源（Host 检查），并默认只绑定 127.0.0.1。
//
// 这些措施都是"默认开启且不可关闭"的：不提供 --no-auth 之类的开关，
// 因为一旦有开关，它就会在网络不通时被当成解决办法。
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/applog"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

//go:embed assets/index.html
var assets embed.FS

// 默认参数。
const (
	// DefaultListen 是默认监听地址。
	//
	// 只绑本机：这个工具会主动发起大量外部连接，
	// 暴露到局域网上等于把"从你的网络发起扫描"的能力借给别人。
	DefaultListen = "127.0.0.1:0"

	// DefaultShutdownTimeout 是优雅退出的等待上限。
	DefaultShutdownTimeout = 5 * time.Second
)

// Config 是服务配置。
type Config struct {
	// Listen 是监听地址（host:port）。端口为 0 时由系统分配。
	Listen string

	// Service 是编排层。
	Service *service.Service

	// Logger 记录服务端事件（日志文件是脱离命令行运行时的唯一排查入口）。
	Logger *applog.Logger

	// OpenBrowser 为真时启动后打开浏览器。
	OpenBrowser bool

	// BrowserOpener 允许测试注入"打开浏览器"的实现。
	BrowserOpener func(url string) error

	// Token 允许测试固定 token（留空则随机生成）。
	Token string
}

// Server 是图形界面的 HTTP 服务。
type Server struct {
	cfg   Config
	token string
	hub   *service.ProgressHub

	// 单次扫描约束：本工具一次跑满 CPU/网络，并发跑两次毫无意义，
	// 而且会让本地数据库出现两个同时写入的会话。
	scanMu     sync.Mutex
	scanCancel context.CancelFunc
	scanBusy   bool
	lastResult *scanOutcome

	httpServer *http.Server
	listener   net.Listener

	// done 在服务关闭后关闭，用于通知等待方。
	done      chan struct{}
	closeOnce sync.Once
}

// scanOutcome 是一次扫描的结局（成功或失败）。
type scanOutcome struct {
	// StartedAt / FinishedAt 是本次扫描的时间范围。
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Error 是失败原因（成功时为空）。
	Error string `json:"error,omitempty"`

	// Kind 是错误分类，便于界面给出不同的提示。
	Kind string `json:"kind,omitempty"`

	// Summary 是成功时的结果摘要。
	Summary *ScanSummary `json:"summary,omitempty"`
}

// ScanSummary 是扫描结果的展示摘要。
//
// 只挑界面需要的字段，而不是把 scheduler.Result 整个序列化出去：
// 后者有二十多个字段，大部分对界面没有意义，而且会让内部结构
// 意外地变成"对外接口"，以后改不动。
type ScanSummary struct {
	SessionID         string  `json:"session_id"`
	Resumed           bool    `json:"resumed"`
	TargetsTotal      int     `json:"targets_total"`
	TargetsPending    int     `json:"targets_pending"`
	ProbeSuccess      int     `json:"probe_success"`
	ProbeFailed       int     `json:"probe_failed"`
	Stored            int     `json:"stored"`
	TraceStored       int     `json:"trace_stored"`
	TraceSkipped      bool    `json:"trace_skipped"`
	TraceUnavailable  string  `json:"trace_unavailable,omitempty"`
	Interrupted       bool    `json:"interrupted"`
	SessionFinished   bool    `json:"session_finished"`
	ElapsedSeconds    float64 `json:"elapsed_seconds"`
	SourceURL         string  `json:"source_url"`
	SourceFromCache   bool    `json:"source_from_cache"`
	LastSessionSaved  bool    `json:"last_session_saved"`
	LastSessionErrMsg string  `json:"last_session_error,omitempty"`
}

// New 创建服务。
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("webui: Service is required")
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = DefaultListen
	}
	if cfg.Logger == nil {
		cfg.Logger = applog.Discard()
	}

	token := cfg.Token
	if token == "" {
		generated, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("webui: generate token: %w", err)
		}
		token = generated
	}

	return &Server{
		cfg:   cfg,
		token: token,
		hub:   service.NewProgressHub(),
		// done 必须在这里创建。
		//
		// 漏掉这一行的后果不是"关闭通知失效"，而是**服务启动后立刻退出**：
		// Done() 在 done 为 nil 时返回一个已关闭的通道，于是调用方的
		// `select { case <-server.Done(): }` 立即命中，认为服务已经退出。
		// 这个 bug 实测出现过（监听日志与停止日志时间戳相同）。
		done: make(chan struct{}),
	}, nil
}

// Token 返回本次运行的访问 token。
func (s *Server) Token() string { return s.token }

// URL 返回服务地址（须在 Start 之后调用）。
func (s *Server) URL() string {
	if s.listener == nil {
		return ""
	}
	// 绑定 0.0.0.0 / :: 时浏览器不能访问这些地址，
	// 需要换成 127.0.0.1；这是"能点开"与"看起来对"的区别。
	addr := s.listener.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(addr.Port))
}

// PageURL 返回带 token 的页面地址。
func (s *Server) PageURL() string {
	return s.URL() + "/?token=" + s.token
}

// Start 开始监听。
//
// 它**不阻塞**：监听与提供服务在后台进行，调用方可以用 Wait 等待退出。
func (s *Server) Start() error {
	if s.listener != nil {
		return errors.New("webui: already started")
	}

	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", s.cfg.Listen, err)
	}
	s.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/scan/stop", s.handleStop)
	mux.HandleFunc("/api/scan/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)

	s.httpServer = &http.Server{
		Handler:           s.withSecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：SSE 是长连接，写超时会把进度推送掐断。
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Logger.Error("webui: server stopped", "error", err)
		}
	}()

	s.cfg.Logger.Info("webui: listening", "url", s.URL())

	if s.cfg.OpenBrowser {
		opener := s.cfg.BrowserOpener
		if opener == nil {
			opener = openBrowser
		}
		// 开浏览器失败只是不方便，不是错误：用户完全可以自己复制地址。
		if err := opener(s.PageURL()); err != nil {
			s.cfg.Logger.Warn("webui: could not open the browser; open the URL manually",
				"url", s.PageURL(), "error", err)
		}
	}
	return nil
}

// Done 返回一个在服务退出后关闭的通道。
//
// 用通道而不是 Wait() 方法：调用方通常要 select 它和信号处理，
// 而一个阻塞方法在那种场景里很难用。
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) error {
	// 先取消正在跑的扫描：否则关闭时可能丢掉尚未落库的结果。
	s.cancelScan()

	// 通知等待方。放在 httpServer.Shutdown 之前：
	// 等待方关心的是"该收尾了"，而不是"端口已释放"。
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})

	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// Addr 返回监听地址。
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// withSecurityHeaders 加上一组保守的安全响应头。
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面只由本服务提供，不需要任何外部资源，
		// 因此 CSP 可以收得很紧（不允许内联脚本之外的任何来源）。
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// authorize 校验 token、Host 与 Origin。
//
// 返回 false 时已经把响应写好了。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	// 1) Host 必须是本机地址：防止 DNS rebinding
	//    （外部域名解析到 127.0.0.1 后带着合法 Origin 来访问）。
	if !isLocalHost(r.Host) {
		writeError(w, http.StatusForbidden, "requests must use a loopback host")
		return false
	}

	// 2) token：所有 /api 请求都要带。
	//    用常数时间比较，避免通过响应时间逐字节猜 token。
	presented := r.Header.Get("X-Auth-Token")
	if presented == "" {
		presented = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return false
	}

	// 3) 变更类请求必须来自本服务的页面（CSRF）。
	//    同源策略不会阻止"跨站发起请求"，只会阻止读取响应——
	//    而发起一次扫描的副作用已经发生了。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return false
		}
	}
	return true
}

// sameOrigin 判断请求是否来自本服务自己的页面。
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// 没有 Origin 的变更请求：可能来自 curl 等工具（合理），
		// 也可能来自某些跨站表单提交。用 Referer 兜一层；
		// 两者都没有时放行——因为没有 token 也到不了这里，
		// 而 token 只有本服务的页面知道。
		referer := r.Header.Get("Referer")
		if referer == "" {
			return true
		}
		return strings.HasPrefix(referer, s.URL()+"/") || strings.HasPrefix(referer, s.URL())
	}

	// Origin 必须与本服务的地址一致（忽略端口为 0 的差异）。
	self := s.URL()
	return origin == self || strings.HasPrefix(origin, self)
}

// isLocalHost 判断 Host 头是不是回环地址。
//
// 这是防 DNS rebinding 的关键：攻击者可以让 evil.com 解析到 127.0.0.1，
// 这样浏览器发出的请求**看起来**是访问本机，Host 头却是 evil.com。
func isLocalHost(host string) bool {
	if host == "" {
		return false
	}
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
	}
	hostname = strings.Trim(hostname, "[]")

	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

// handleIndex 返回页面。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	page, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		s.cfg.Logger.Error("webui: cannot read embedded page", "error", err)
		writeError(w, http.StatusInternalServerError, "page unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面本身不含数据，但把 token 交给它是必要的：
	// 它是本服务自己发出的页面，且 token 只存在于内存与这次响应里。
	if _, err := w.Write(page); err != nil {
		s.cfg.Logger.Warn("webui: writing page failed", "error", err)
	}
}

// handleStats 返回数据库概览。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	stats, err := s.cfg.Service.Stats(ctx)
	if err != nil {
		s.cfg.Logger.Warn("webui: stats failed", "error", err)
		writeError(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, statsPayload{Stats: stats, Scanning: s.isScanning(), Last: s.lastOutcome()})
}

// scanRequest 是启动扫描的请求体。
type scanRequest struct {
	Workers       int    `json:"workers"`
	TimeoutMS     int    `json:"timeout_ms"`
	Limit         int    `json:"limit"`
	Resume        bool   `json:"resume"`
	SessionID     string `json:"session_id"`
	Trace         bool   `json:"trace"`
	TraceBinary   string `json:"trace_binary"`
	TraceMode     string `json:"trace_mode"`
	TraceWorkers  int    `json:"trace_workers"`
	TraceTimeoutS int    `json:"trace_timeout_s"`

	Country   string `json:"country"`
	Province  string `json:"province"`
	City      string `json:"city"`
	ISP       string `json:"isp"`
	ASN       string `json:"asn"`
	IPVersion string `json:"ip_version"`
}

// handleScan 启动一次扫描。
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req scanRequest
	// 允许空请求体（全部走默认值），但拒绝格式错误的内容。
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.scanMu.Lock()
	if s.scanBusy {
		s.scanMu.Unlock()
		writeError(w, http.StatusConflict, "a scan is already running")
		return
	}
	s.scanBusy = true
	s.lastResult = nil
	s.hub.Reset()

	// 用 context.WithCancel 而不是请求的 ctx：扫描不该因为
	// 用户刷新页面或关掉标签页就被取消。停止扫描走 /api/scan/stop。
	ctx, cancel := context.WithCancel(context.Background())
	s.scanCancel = cancel
	s.scanMu.Unlock()

	s.cfg.Logger.Info("webui: scan requested",
		"workers", req.Workers, "limit", req.Limit, "resume", req.Resume, "trace", req.Trace)

	go s.runScan(ctx, req, cancel)

	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// runScan 在后台执行扫描，并把结局记下来供界面查询。
func (s *Server) runScan(ctx context.Context, req scanRequest, cancel context.CancelFunc) {
	defer cancel()

	opts := service.ScanOptions{
		Workers:   req.Workers,
		Timeout:   durationMS(req.TimeoutMS),
		Limit:     req.Limit,
		Resume:    req.Resume,
		SessionID: strings.TrimSpace(req.SessionID),
		Trace:     req.Trace,
		TraceConfig: service.TraceOptions{
			Binary:  strings.TrimSpace(req.TraceBinary),
			Mode:    strings.TrimSpace(req.TraceMode),
			Workers: req.TraceWorkers,
			Timeout: durationSeconds(req.TraceTimeoutS),
		},
		Collector: collectorFrom(req),
		Progress:  s.hub.Publish,
	}

	started := time.Now().UTC()
	result, err := s.cfg.Service.RunScan(ctx, opts)
	finished := time.Now().UTC()

	outcome := &scanOutcome{StartedAt: started, FinishedAt: finished}
	if err != nil {
		outcome.Error = err.Error()
		outcome.Kind = classify(err)
		s.cfg.Logger.Error("webui: scan failed", "kind", outcome.Kind, "error", err)
	} else {
		summary := summarize(result)
		outcome.Summary = &summary
		s.cfg.Logger.Info("webui: scan finished",
			"session", summary.SessionID, "stored", summary.Stored, "traces", summary.TraceStored)
	}

	s.scanMu.Lock()
	s.lastResult = outcome
	s.scanBusy = false
	s.scanCancel = nil
	s.scanMu.Unlock()
}

// handleStop 请求停止当前扫描。
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	if !s.cancelScan() {
		writeError(w, http.StatusConflict, "no scan is running")
		return
	}
	s.cfg.Logger.Info("webui: scan stop requested")
	writeJSON(w, http.StatusOK, map[string]any{"stopping": true})
}

// handleStatus 返回当前扫描状态与最近一次结果。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	last, ok := s.hub.Last()
	writeJSON(w, http.StatusOK, map[string]any{
		"scanning": s.isScanning(),
		"last":     s.lastOutcome(),
		"progress": progressPayload(last, ok),
	})
}

// handleEvents 用 SSE 推送进度。
//
// 用 SSE 而不是 WebSocket：只需 net/http，单向推送正好够用，
// 且浏览器断线后会自动重连（进度本来就是"最新状态"语义）。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	// 反向代理场景下禁用缓冲（即便单机也留着，代价为零）。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, cancel := s.hub.Subscribe()
	defer cancel()

	// 心跳：让浏览器与中间层知道连接还活着，
	// 也让本端能及时发现客户端已经断开。
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case event, open := <-events:
			if !open {
				return
			}
			if !writeSSE(w, "progress", progressPayload(event, true)) {
				return
			}
			flusher.Flush()

		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// isScanning 报告是否有扫描在跑。
func (s *Server) isScanning() bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return s.scanBusy
}

// lastOutcome 返回最近一次扫描的结局。
func (s *Server) lastOutcome() *scanOutcome {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.lastResult == nil {
		return nil
	}
	outcome := *s.lastResult
	return &outcome
}

// cancelScan 取消正在跑的扫描，返回是否真的有扫描被取消。
func (s *Server) cancelScan() bool {
	s.scanMu.Lock()
	cancel := s.scanCancel
	s.scanMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// progressPayload 把进度事件转成界面用的结构。
func progressPayload(event scheduler.ProgressEvent, ok bool) map[string]any {
	if !ok {
		return nil
	}
	return map[string]any{
		"phase":     string(event.Phase),
		"completed": event.Completed,
		"total":     event.Total,
		"success":   event.Success,
		"failed":    event.Failed,
	}
}

// summarize 把调度器结果压成界面需要的字段。
func summarize(result *service.ScanResult) ScanSummary {
	return ScanSummary{
		SessionID:         result.SessionID,
		Resumed:           result.Resumed,
		TargetsTotal:      result.TargetsTotal,
		TargetsPending:    result.TargetsPending,
		ProbeSuccess:      result.Probe.Success,
		ProbeFailed:       result.Probe.Failed,
		Stored:            result.Stored,
		TraceStored:       result.TraceStored,
		TraceSkipped:      result.TraceSkipped,
		TraceUnavailable:  result.TraceUnavailable,
		Interrupted:       result.Interrupted,
		SessionFinished:   result.SessionFinished,
		ElapsedSeconds:    result.Elapsed().Seconds(),
		SourceURL:         result.SourceURL,
		SourceFromCache:   result.SourceFromCache,
		LastSessionSaved:  result.LastSessionSaved,
		LastSessionErrMsg: result.LastSessionError,
	}
}

// collectorFrom 把请求里的画像字段转成模型（只取非空项）。
func collectorFrom(req scanRequest) model.CollectorProfile {
	return model.CollectorProfile{
		Country:   strings.TrimSpace(req.Country),
		Province:  strings.TrimSpace(req.Province),
		City:      strings.TrimSpace(req.City),
		ISP:       strings.TrimSpace(req.ISP),
		ASN:       strings.TrimSpace(req.ASN),
		IPVersion: model.IPVersion(strings.TrimSpace(req.IPVersion)),
	}
}

// classify 把错误归成界面能区分对待的类别。
func classify(err error) string {
	switch {
	case err == nil:
		return ""
	case service.IsUsage(err):
		return "usage"
	case service.IsNoTargets(err):
		return "no_targets"
	case service.IsNoSession(err):
		return "no_session"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

// statusFor 把错误映射成 HTTP 状态码。
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case service.IsUsage(err):
		return http.StatusBadRequest
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSONBody 解析请求体，并限制大小。
func decodeJSONBody(r *http.Request, target any) error {
	// 空请求体是合法的：界面可以只点"开始"而全部走默认值。
	if r.Body == nil {
		return nil
	}
	// 1 MiB 远大于任何合理请求，但能挡住"用一个巨大 body 打满内存"。
	limited := io.LimitReader(r.Body, 1<<20)

	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid request body: %v", err)
	}
	return nil
}

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// 响应头已经发出，无法改成错误码；只能记录。
		return
	}
}

// writeError 写一个错误响应。
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}

// writeSSE 写一条 SSE 事件，报告是否成功。
func writeSSE(w io.Writer, event string, payload any) bool {
	blob, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, blob); err != nil {
		return false
	}
	return true
}

// durationMS 把毫秒转成时长（<=0 返回 0，表示用默认值）。
func durationMS(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// durationSeconds 把秒转成时长（<=0 返回 0）。
func durationSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// randomToken 生成一个随机 token。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// statsPayload 是 /api/stats 的响应。
type statsPayload struct {
	*service.Stats
	Scanning bool         `json:"scanning"`
	Last     *scanOutcome `json:"last_scan"`
}

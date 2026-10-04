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
	"bufio"
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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/applog"
	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
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

	// ASNPrefixDir 是 ASN 前缀缓存目录（空表示用默认值）。
	ASNPrefixDir string

	// DefaultCSVPath 是结果 CSV 的默认路径（请求里没给时使用）。
	//
	// 结果只进 CSV：没有数据库、没有会话、没有续测。
	DefaultCSVPath string
}

// DefaultCSVPath 是结果文件的默认位置。
//
// 用固定文件名而不是每次带时间戳：使用者关心的是"结果在哪"，
// 一个名字固定的文件比一堆时间戳文件好找。想保留多轮结果时
// 勾选「追加」或自己指定 --out。
const DefaultCSVPath = "data/results.csv"

// Server 是图形界面的 HTTP 服务。
type Server struct {
	cfg   Config
	token string
	hub   *service.CSVProgressHub

	// events 是界面日志面板的事件流（与 hub 分工见 events.go）。
	events *eventBroker

	// 单次扫描约束：本工具一次跑满 CPU/网络，并发跑两次毫无意义，
	// 而且会让本地数据库出现两个同时写入的会话。
	scanMu     sync.Mutex
	scanCancel context.CancelFunc
	scanBusy   bool
	lastResult *scanOutcome

	// currentTarget 是当前正在探测的目标（供页面刷新后恢复显示）。
	currentMu     sync.Mutex
	currentTarget string

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
// 只挑界面需要的字段，而不是把内部结构整个序列化出去：
// 后者会让内部结构意外地变成"对外接口"，以后改不动。
type ScanSummary struct {
	// OutputPath 是结果 CSV 的路径——界面要把它显示给用户，
	// 否则用户不知道去哪儿找结果。
	OutputPath string `json:"output_path"`

	Targets     int     `json:"targets"`
	Probed      int     `json:"probed"`
	Succeeded   int     `json:"succeeded"`
	Failed      int     `json:"failed"`
	Traced      int     `json:"traced"`
	TracedOK    int     `json:"traced_ok"`
	RowsWritten int     `json:"rows_written"`
	WriteErrors int     `json:"write_errors"`
	Interrupted bool    `json:"interrupted"`
	ElapsedSecs float64 `json:"elapsed_seconds"`

	SourceURL       string `json:"source_url"`
	SourceFromCache bool   `json:"source_from_cache"`

	// TraceUnavailable 说明"要求跟踪但引擎不可用"的原因（可空）。
	TraceUnavailable string `json:"trace_unavailable,omitempty"`
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
		cfg:    cfg,
		token:  token,
		hub:    service.NewProgressHubCSV(),
		events: newEventBroker(),
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
	mux.HandleFunc("/api/events/log", s.handleEventsLog)
	mux.HandleFunc("/api/log", s.handleLog)

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

// handleStats 返回结果文件的状态。
//
// 刻意**不**再报告数据库统计：扫描结果只进 CSV，
// 显示"数据库里有几行"只会让人以为数据存在库里。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	payload := map[string]any{
		"path":        s.resultsPath(),
		"scanning":    s.isScanning(),
		"last_scan":   s.lastOutcome(),
		"rows":        int64(0),
		"size_bytes":  int64(0),
		"modified_at": "",
	}

	info, err := os.Stat(s.resultsPath())
	if err == nil {
		payload["size_bytes"] = info.Size()
		payload["modified_at"] = info.ModTime().UTC().Format(time.RFC3339)
		// 行数减 1 是表头；文件里只有表头时算 0 行。
		if rows, countErr := countCSVRows(s.resultsPath()); countErr == nil {
			if rows > 0 {
				rows--
			}
			payload["rows"] = rows
		}
	}

	writeJSON(w, http.StatusOK, payload)
}

// resultsPath 返回当前结果文件路径。
func (s *Server) resultsPath() string {
	if path := strings.TrimSpace(s.cfg.DefaultCSVPath); path != "" {
		return path
	}
	return DefaultCSVPath
}

// countCSVRows 数一个 CSV 的数据行数（含表头）。
//
// 逐行数而不是读进内存：结果文件可能有几十万行，
// 首页完全没必要为了显示一个数字把它读进来。
func countCSVRows(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	var count int64
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		_, readErr := reader.ReadString('\n')
		count++
		if readErr != nil {
			// 最后一行没有换行符也算一行，但 EOF 之后不该再多算一次。
			if errors.Is(readErr, io.EOF) {
				count--
			}
			break
		}
	}
	if count < 0 {
		count = 0
	}
	return count, nil
}

// scanRequest 是启动扫描的请求体。
type scanRequest struct {
	Workers       int    `json:"workers"`
	TimeoutMS     int    `json:"timeout_ms"`
	Limit         int    `json:"limit"`
	Trace         bool   `json:"trace"`
	TraceBinary   string `json:"trace_binary"`
	TraceMode     string `json:"trace_mode"`
	TraceWorkers  int    `json:"trace_workers"`
	TraceTimeoutS int    `json:"trace_timeout_s"`

	// DataProvider 是 GeoIP 数据源（ASN/运营商/地区的来源）。
	DataProvider string `json:"data_provider"`

	// PowProvider 是 NextTrace API v3 的 PoW 令牌源。
	PowProvider string `json:"pow_provider"`

	// NoASNPrefix 为真时不用本地 ASN 前缀识别线路。
	NoASNPrefix bool `json:"no_asn_prefix"`

	// NoAutoDownload 为真时不在缺 nexttrace 时自动下载。
	NoAutoDownload bool `json:"no_auto_download"`

	// OutputPath 是结果 CSV 的路径（留空用默认值）。
	//
	// 结果只进 CSV：没有会话、没有数据库、没有续测。
	OutputPath string `json:"output_path"`

	// Append 为真时追加到已有文件而不是覆盖。
	Append bool `json:"append"`

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
		"workers", req.Workers, "limit", req.Limit, "trace", req.Trace,
		"output", req.OutputPath, "append", req.Append)
	s.emitScanStart(req)

	go s.runScan(ctx, req, cancel)

	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// runScan 在后台执行扫描，并把结局记下来供界面查询。
//
// 结果**只写 CSV**：没有会话、没有数据库、没有续测。
// 每一行测完就刷盘，因此中途停掉也不会丢已完成的测量。
func (s *Server) runScan(ctx context.Context, req scanRequest, cancel context.CancelFunc) {
	defer cancel()

	outputPath := strings.TrimSpace(req.OutputPath)
	if outputPath == "" {
		outputPath = s.cfg.DefaultCSVPath
	}
	if strings.TrimSpace(outputPath) == "" {
		outputPath = DefaultCSVPath
	}

	opts := service.CSVScanOptions{
		OutputPath: outputPath,
		Append:     req.Append,
		Workers:    req.Workers,
		Timeout:    durationMS(req.TimeoutMS),
		Limit:      req.Limit,
		Trace:      req.Trace,
		TraceConfig: service.TraceOptions{
			Binary:       strings.TrimSpace(req.TraceBinary),
			Mode:         strings.TrimSpace(req.TraceMode),
			DataProvider: strings.TrimSpace(req.DataProvider),
			PowProvider:  strings.TrimSpace(req.PowProvider),
			// 图形界面默认自动下载；勾选框可以关掉。
			AutoDownload: !req.NoAutoDownload,
			Workers:      req.TraceWorkers,
			Timeout:      durationSeconds(req.TraceTimeoutS),
		},
		// Progress 既推给进度条，也翻译成日志行（阶段完成时才写一条）。
		Progress: s.emitCSVProgress,
		// OnTarget 只更新"当前正在测哪个 IP"（供状态接口与页面恢复显示），
		// 不写日志行——每个目标的结果由 OnProbe 输出。
		OnTarget: s.emitTarget,
		// OnProbe 输出每个 IP 的延迟与连通性（含落地地区）。
		OnProbe: s.emitProbe,
		// OnTrace 输出线路：ASN 编号 + 线路名称 + 落地地区。
		OnTrace: s.emitTrace,
		// 本地 ASN 前缀识别（默认开启）：无限、不限流、无需账号。
		NoASNPrefix: req.NoASNPrefix,
		ASNPrefixOptions: asnprefix.Options{
			Dir: s.cfg.ASNPrefixDir,
		},
	}

	started := time.Now().UTC()
	result, err := s.cfg.Service.RunCSVScan(ctx, opts)
	finished := time.Now().UTC()

	outcome := &scanOutcome{StartedAt: started, FinishedAt: finished}
	if err != nil {
		outcome.Error = err.Error()
		outcome.Kind = classify(err)
		s.cfg.Logger.Error("webui: scan failed", "kind", outcome.Kind, "error", err)
	} else {
		summary := summarizeCSVScan(result)
		outcome.Summary = &summary
		s.cfg.Logger.Info("webui: scan finished",
			"csv", summary.OutputPath, "rows", summary.RowsWritten, "errors", summary.WriteErrors)
	}

	s.emitScanEnd(outcome)

	s.scanMu.Lock()
	s.lastResult = outcome
	s.scanBusy = false
	s.scanCancel = nil
	s.scanMu.Unlock()

	// 清掉"当前目标"：扫描结束后它已经没有意义，
	// 留着会让页面显示一个早已测完的 IP。
	s.currentMu.Lock()
	s.currentTarget = ""
	s.currentMu.Unlock()
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

	// 当前目标也一并返回：页面刷新后日志流的"正在测量 X"可能已经
	// 被历史裁剪掉，靠这条恢复显示。
	s.currentMu.Lock()
	current := s.currentTarget
	s.currentMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"scanning":       s.isScanning(),
		"last":           s.lastOutcome(),
		"progress":       progressPayload(last, ok),
		"current_target": current,
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
func progressPayload(event service.ProgressEvent, ok bool) map[string]any {
	if !ok {
		return nil
	}
	return map[string]any{
		"phase":          event.Phase,
		"completed":      event.Completed,
		"total":          event.Total,
		"success":        event.Success,
		"failed":         event.Failed,
		"current_target": event.CurrentTarget,
	}
}

// summarizeCSVScan 把扫描结果压成界面需要的字段。
func summarizeCSVScan(result *service.CSVScanResult) ScanSummary {
	return ScanSummary{
		OutputPath:       result.OutputPath,
		Targets:          result.Targets,
		Probed:           result.Probed,
		Succeeded:        result.Succeeded,
		Failed:           result.Failed,
		Traced:           result.Traced,
		TracedOK:         result.TracedOK,
		RowsWritten:      result.RowsWritten,
		WriteErrors:      result.Errors,
		Interrupted:      result.Interrupted,
		ElapsedSecs:      result.Elapsed().Seconds(),
		SourceURL:        result.SourceURL,
		SourceFromCache:  result.SourceFromCache,
		TraceUnavailable: result.TraceUnavailable,
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

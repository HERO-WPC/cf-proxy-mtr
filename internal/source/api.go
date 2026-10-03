package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 数据源默认值。
//
// 这些常量是"默认配置"，不是硬编码策略：CLI 参数可以覆盖，
// 因此数据源迁移时不需要改代码。
const (
	// DefaultURL 是主数据源（JSON）。
	DefaultURL = "https://zip.cm.edu.kg/all.json"

	// DefaultFallbackURL 是备用数据源（纯文本 IP:Port#CC）。
	DefaultFallbackURL = "https://zip.cm.edu.kg/all.txt"

	// DefaultCachePath 是本地缓存路径。
	DefaultCachePath = "data/all.json"

	// DefaultTimeout 是单次 HTTP 请求的超时时间。
	DefaultTimeout = 30 * time.Second

	// DefaultRetries 是失败后额外重试的次数（不含首次请求）。
	DefaultRetries = 3

	// DefaultRetryBackoff 是首次重试前的等待时间，之后按倍数递增。
	DefaultRetryBackoff = 800 * time.Millisecond

	// maxRetryBackoff 限制单次重试等待上限，避免指数退避失控。
	maxRetryBackoff = 10 * time.Second

	// maxBodyBytes 限制读取的数据源大小（64 MiB）。
	//
	// 当前 all.json 约 10 MiB。设置上限是为了避免上游异常或恶意响应
	// 导致内存被无限占用。
	maxBodyBytes = 64 << 20

	// userAgent 是本工具的 User-Agent。
	//
	// 明确标识自己，便于数据源方统计与联系；
	// 不包含任何机器 / 用户标识。
	userAgent = "cf-route-tester"
)

// Config 是数据源（all.json / all.txt）相关配置。
//
// 零值不是"可用配置"，请使用 DefaultConfig 获取默认值后再覆盖字段。
type Config struct {
	// URL 是主数据源地址。
	URL string

	// FallbackURL 是备用数据源地址。留空表示不使用备用源。
	FallbackURL string

	// CachePath 是本地缓存文件路径。
	CachePath string

	// Timeout 是单次 HTTP 请求超时。
	Timeout time.Duration

	// Retries 是每个数据源地址失败后额外重试的次数。
	Retries int

	// RetryBackoff 是首次重试前的等待时间，后续按 2 倍递增。
	RetryBackoff time.Duration

	// Proxy 是 HTTP(S) 代理地址，例如 "http://127.0.0.1:10808"。
	//
	// 留空表示：优先读取环境变量 HTTP_PROXY / HTTPS_PROXY，两者都没有时
	// 直接连接。显式配置的代理优先于环境变量。
	Proxy string

	// Client 允许注入自定义 HTTP 客户端（测试使用）。
	// 非 nil 时 Timeout / Proxy 不再生效。
	Client *http.Client
}

// DefaultConfig 返回数据源默认配置。
func DefaultConfig() Config {
	return Config{
		URL:          DefaultURL,
		FallbackURL:  DefaultFallbackURL,
		CachePath:    DefaultCachePath,
		Timeout:      DefaultTimeout,
		Retries:      DefaultRetries,
		RetryBackoff: DefaultRetryBackoff,
	}
}

// normalize 补齐零值并做基本校验。
//
// 返回 error 表示配置本身错误（例如 URL 不合法），
// 这类错误属于"全局错误"，应当终止当前任务。
func (c *Config) normalize() error {
	if strings.TrimSpace(c.URL) == "" {
		c.URL = DefaultURL
	}
	if strings.TrimSpace(c.CachePath) == "" {
		c.CachePath = DefaultCachePath
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Retries < 0 {
		c.Retries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = DefaultRetryBackoff
	}

	for _, raw := range []string{c.URL, c.FallbackURL} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if _, err := url.Parse(raw); err != nil {
			return fmt.Errorf("invalid source url %q: %w", raw, err)
		}
	}
	if strings.TrimSpace(c.Proxy) != "" {
		if _, err := url.Parse(c.Proxy); err != nil {
			return fmt.Errorf("invalid proxy url %q: %w", c.Proxy, err)
		}
	}
	return nil
}

// urls 返回按优先级排列的数据源地址（主源在前，备用源在后）。
func (c Config) urls() []string {
	out := make([]string, 0, 2)
	if u := strings.TrimSpace(c.URL); u != "" {
		out = append(out, u)
	}
	if u := strings.TrimSpace(c.FallbackURL); u != "" && u != strings.TrimSpace(c.URL) {
		out = append(out, u)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fetcher
// ---------------------------------------------------------------------------

// Fetcher 负责从数据源下载原始内容。
//
// 它只负责"取回字节"，不负责解析，也不负责写缓存：
// 职责分开后，解析可以单独测试（不需要网络），缓存策略也可以单独测试。
type Fetcher struct {
	cfg    Config
	client *http.Client
}

// NewFetcher 根据配置创建 Fetcher。
//
// 返回 error 只在配置本身非法时出现（例如 URL 无法解析）。
func NewFetcher(cfg Config) (*Fetcher, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &Fetcher{cfg: cfg, client: buildClient(cfg)}, nil
}

// Config 返回生效后的配置（已补齐默认值）。
func (f *Fetcher) Config() Config { return f.cfg }

// buildClient 构造 HTTP 客户端。
//
// 关键点：
//   - 显式设置超时，避免请求无限挂起；
//   - 代理优先使用显式配置，其次使用环境变量；
//   - 只信任系统根证书，不做任何 TLS 校验降级。
func buildClient(cfg Config) *http.Client {
	if cfg.Client != nil {
		return cfg.Client
	}

	transport := &http.Transport{
		Proxy:                 proxyFunc(cfg.Proxy),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		// 不允许自动跟随到其它域名的重定向带来的意外行为：
		// 默认策略即可（最多 10 跳），但显式写出便于后续审查。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// proxyFunc 返回代理选择函数。
//
// Proxy 为空时回退到环境变量（http.ProxyFromEnvironment）；
// 显式配置时忽略环境变量，保证命令行参数优先。
func proxyFunc(explicit string) func(*http.Request) (*url.URL, error) {
	if strings.TrimSpace(explicit) != "" {
		u, err := url.Parse(explicit)
		if err != nil {
			// 配置错误已在 normalize 中拦截；这里保守地不使用代理。
			return nil
		}
		return http.ProxyURL(u)
	}
	return http.ProxyFromEnvironment
}

// ---------------------------------------------------------------------------
// 下载
// ---------------------------------------------------------------------------

// FetchResult 是一次下载尝试的结果。
type FetchResult struct {
	// Body 是原始响应内容（未解析）。
	Body []byte

	// URL 是实际成功的地址。
	URL string

	// StatusCode 是 HTTP 状态码。
	StatusCode int

	// ContentType 是响应头里的 Content-Type。
	ContentType string

	// Attempts 是实际发出的请求次数（含失败与重试）。
	Attempts int

	// Errors 是各次失败的原因摘要，便于诊断"为什么用了备用源"。
	Errors []string
}

// Fetch 依次尝试主源与备用源，返回第一个成功的结果。
//
// 每个地址最多尝试 1+Retries 次，使用指数退避。
// 全部失败时返回 error，error 中包含每个地址的失败原因，
// 使"下载失败明确提示"具备可操作性。
//
// 单个地址失败不会中断整体流程（继续尝试下一个地址），
// 这符合"外部依赖异常必须可恢复"的原则。
func (f *Fetcher) Fetch(ctx context.Context) (*FetchResult, error) {
	urls := f.cfg.urls()
	if len(urls) == 0 {
		return nil, errors.New("no data source url configured")
	}

	res := &FetchResult{}

	for _, u := range urls {
		body, status, ctype, attempts, err := f.fetchOne(ctx, u)
		res.Attempts += attempts
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", u, err))
			// context 被取消（例如 Ctrl+C）时不再尝试其它地址。
			if ctx.Err() != nil {
				break
			}
			continue
		}

		res.Body = body
		res.URL = u
		res.StatusCode = status
		res.ContentType = ctype
		return res, nil
	}

	return nil, fmt.Errorf("all data sources failed: %s", strings.Join(res.Errors, "; "))
}

// fetchOne 从单个地址下载，带重试与退避。
//
// 返回 attempts 表示实际发出的请求数（包括失败的请求），
// 便于调用方统计与测试重试次数。
func (f *Fetcher) fetchOne(ctx context.Context, rawURL string) (body []byte, status int, ctype string, attempts int, err error) {
	var lastErr error

	for attempt := 0; attempt <= f.cfg.Retries; attempt++ {
		if attempt > 0 {
			// 重试前等待：指数退避，但对 context 取消立即响应。
			if werr := sleepCtx(ctx, backoff(f.cfg.RetryBackoff, attempt)); werr != nil {
				return nil, 0, "", attempts, werr
			}
		}

		attempts++
		b, st, ct, reqErr := f.do(ctx, rawURL)
		if reqErr == nil {
			return b, st, ct, attempts, nil
		}
		lastErr = reqErr

		// 不可重试的错误（例如 404、格式错误）立即返回，避免无意义等待。
		if !retryable(reqErr) {
			break
		}
	}

	return nil, 0, "", attempts, lastErr
}

// do 发出一次 HTTP GET 并读取完整响应体。
func (f *Fetcher) do(ctx context.Context, rawURL string) (body []byte, status int, ctype string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, "", &permanentError{fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		// 关闭失败无补救手段，但显式忽略以避免静默泄漏连接。
		_ = resp.Body.Close()
	}()

	status = resp.StatusCode
	ctype = resp.Header.Get("Content-Type")

	if status != http.StatusOK {
		// 读取少量响应体用于诊断，但不把整个错误页塞进日志。
		// 4xx 视为不可重试，5xx 视为可重试。
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		e := fmt.Errorf("unexpected http status %d (%s): %s",
			status, http.StatusText(status), truncate(strings.TrimSpace(string(snippet)), 200))
		if status >= 400 && status < 500 {
			return nil, status, ctype, &permanentError{e}
		}
		return nil, status, ctype, e
	}

	reader, err := decompressBody(resp)
	if err != nil {
		return nil, status, ctype, err
	}

	b, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil {
		return nil, status, ctype, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBodyBytes {
		return nil, status, ctype, &permanentError{
			fmt.Errorf("response body exceeds %d bytes limit", maxBodyBytes),
		}
	}
	if len(b) == 0 {
		// 空响应几乎总是上游故障，重试一次是合理的。
		return nil, status, ctype, errors.New("empty response body")
	}

	// 一些中间层（CDN / 门户）会用 200 + HTML 返回错误页。
	// 这种响应不是数据源，直接判定为失败并尝试下一个地址。
	if looksLikeHTML(b) {
		return nil, status, ctype, &permanentError{fmt.Errorf(
			"response looks like HTML, not a data source (content-type=%q)", ctype)}
	}

	return b, status, ctype, nil
}

// decompressBody 处理 Content-Encoding。
//
// Go 的 Transport 只在"没有手动设置 Accept-Encoding"时才自动解压。
// 我们显式设置了 Accept-Encoding: gzip（为了明确告诉对方能力），
// 因此这里必须自己处理 gzip，否则会把压缩字节当成 JSON 解析。
func decompressBody(resp *http.Response) (io.Reader, error) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return resp.Body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, &permanentError{fmt.Errorf("gzip: %w", err)}
		}
		return zr, nil
	default:
		return nil, &permanentError{fmt.Errorf("unsupported content-encoding %q", enc)}
	}
}

// looksLikeHTML 判断响应体是否像 HTML 文档。
func looksLikeHTML(b []byte) bool {
	head := bytes.TrimSpace(b)
	if len(head) > 256 {
		head = head[:256]
	}
	lower := strings.ToLower(string(head))
	for _, marker := range []string{"<!doctype html", "<html", "<head>", "<body"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 错误分类与辅助
// ---------------------------------------------------------------------------

// permanentError 包装"重试没有意义"的错误。
//
// 例如：HTTP 404、Content-Encoding 不支持、响应是 HTML、
// 响应体超过上限。这类错误会立即结束该地址的尝试，
// 但不会终止整体流程——下一个数据源仍会尝试。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// retryable 报告错误是否值得重试。
func retryable(err error) bool {
	var pe *permanentError
	return !errors.As(err, &pe)
}

// backoff 计算第 attempt 次重试前的等待时间（attempt 从 1 开始）。
func backoff(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= maxRetryBackoff {
			return maxRetryBackoff
		}
	}
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}

// sleepCtx 等待指定时长，但在 context 取消时立即返回错误。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ContentLengthHint 返回适合日志展示的大小描述。
func ContentLengthHint(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 2, 64) + " MiB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 2, 64) + " KiB"
	default:
		return strconv.Itoa(n) + " B"
	}
}

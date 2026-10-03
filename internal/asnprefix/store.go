package asnprefix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
)

// 默认参数。
const (
	// DefaultBaseURL 是 RouteViews 的 ASN→前缀查询接口。
	//
	// 选它的原因：不需要账号、不需要 token、没有 observed 限流，
	// 而且数据直接来自 BGP 表（RouteViews 是公认的公共路由归档）。
	DefaultBaseURL = "https://api.routeviews.org/asn"

	// DefaultDir 是前缀缓存目录（相对工作目录）。
	DefaultDir = "data/asnprefix"

	// DefaultTTL 是缓存的有效期。
	//
	// 7 天是个折中：前缀会变，但一条骨干线路的 IP 段不会天天变；
	// 而每次跟踪都去抓 30 个 ASN 的前缀，既慢又没必要。
	DefaultTTL = 7 * 24 * time.Hour

	// DefaultTimeout 是单次 HTTP 请求的超时。
	DefaultTimeout = 20 * time.Second

	// DefaultConcurrency 是并发抓取的 ASN 数。
	//
	// 抓取必须并发：30 个 ASN 各有 v4/v6 两个请求，共 60 次。
	// 串行实测要 57 秒——而那段时间里跟踪阶段完全没开始，
	// 使用者看到的是"TCP 秒完，然后卡住一分钟"。
	//
	// 取 8 而不是 30：每个 ASN 内部还有两个顺序请求，
	// 因此实际并发约 16 个连接，既够快又不会把对端当压测。
	DefaultConcurrency = 8

	// maxResponseBytes 限制单个响应体大小，防止异常响应打爆内存。
	maxResponseBytes = 8 << 20 // 8 MiB；实测最大约 200 KB
)

// Options 是构造 Resolver 的参数。
type Options struct {
	// Dir 是缓存目录（空表示用 DefaultDir）。
	Dir string

	// BaseURL 是接口地址（空表示用 DefaultBaseURL）。
	BaseURL string

	// TTL 是缓存有效期（<=0 表示用 DefaultTTL）。
	TTL time.Duration

	// Timeout 是单次请求超时（<=0 表示用 DefaultTimeout）。
	Timeout time.Duration

	// Concurrency 是并发抓取的 ASN 数（<=0 表示用 DefaultConcurrency）。
	Concurrency int

	// ASNs 是要抓取的 ASN 列表（空表示用 asnmap.Known()）。
	//
	// 默认取自 asnmap 而不是另立一份清单：要认的线路就是
	// asnmap 里那些，两边各存一份迟早不一致。
	ASNs []string

	// HTTPClient 允许注入客户端（测试用）。
	HTTPClient *http.Client

	// Now 允许注入时钟（测试用）。
	Now func() time.Time

	// Logf 接收非致命的说明（可为 nil）。
	//
	// 抓取失败**不是**致命错误：有缓存就用缓存，
	// 没缓存就是"线路名认不出来"，而路径本身照样测得到。
	Logf func(format string, args ...any)
}

// Resolver 按 IP 段判断某个 IP 属于哪几条已知线路。
type Resolver struct {
	mu sync.RWMutex

	// byASN 是每个 ASN 的前缀集合。
	byASN map[string]*Set

	// asnOrder 是 ASN 的固定顺序（asnmap 的顺序，按编号升序）。
	//
	// 固定顺序是必要的：同一个 IP 可能落在多条线路的段里
	// （转售、代理、嵌套宣告），谁先出现必须稳定，
	// 否则同一份数据两次运行会给出不同的线路名。
	asnOrder []string

	opts Options
}

// Load 构造 Resolver：优先读缓存，缓存缺失或过期时抓取。
//
// 它**不返回错误**。理由：线路名是锦上添花，拿不到不该让
// 整轮测量失败——探测结果是主要产出，而且已经在写 CSV 了。
// 具体发生了什么通过 Logf 说明。
func Load(ctx context.Context, opts Options) *Resolver {
	opts = withDefaults(opts)

	asns := opts.ASNs
	if len(asns) == 0 {
		for _, route := range asnmap.Known() {
			asns = append(asns, route.ASN)
		}
	}

	resolver := &Resolver{
		byASN:    make(map[string]*Set, len(asns)),
		asnOrder: asns,
		opts:     opts,
	}

	var (
		fetched   atomic.Int64
		fromCache atomic.Int64
		failed    atomic.Int64
	)

	// **并发**加载：串行实测要 57 秒，而这段时间里跟踪完全没开始，
	// 使用者看到的是"TCP 秒完，然后卡住一分钟"。
	//
	// 每个 ASN 一个任务，由固定数量的 worker 消费。
	// 结果写进 byASN，因此必须加锁——Match 等读路径也用它。
	workers := opts.Concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	if workers > len(asns) {
		workers = len(asns)
	}

	tasks := make(chan string)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for asn := range tasks {
				set, src, err := resolver.loadOne(ctx, asn)
				if err != nil {
					failed.Add(1)
					continue
				}
				if set == nil {
					continue
				}

				resolver.mu.Lock()
				resolver.byASN[asn] = set
				resolver.mu.Unlock()

				switch src {
				case sourceNetwork:
					fetched.Add(1)
				case sourceCache:
					fromCache.Add(1)
				}
			}
		}()
	}

	canceled := false
	for _, asn := range asns {
		select {
		case tasks <- asn:
		case <-ctx.Done():
			canceled = true
		}
		if canceled {
			break
		}
	}
	close(tasks)
	wg.Wait()

	failedCount := int(failed.Load())
	if failedCount > 0 {
		opts.Logf("%d/%d ASN 的前缀不可用（线路名会少认一些）", failedCount, len(asns))
	}

	resolver.mu.RLock()
	ready := len(resolver.byASN)
	resolver.mu.RUnlock()

	if canceled {
		opts.Logf("抓取被取消：就绪 %d/%d 个 ASN", ready, len(asns))
	} else {
		opts.Logf("就绪 %d/%d 个 ASN（缓存 %d，新抓 %d）",
			ready, len(asns), fromCache.Load(), fetched.Load())
	}
	if ready == 0 {
		opts.Logf("没有任何前缀数据，线路名将无法识别")
	}

	return resolver
}

// source 说明一份前缀是从哪儿来的（仅用于日志口径）。
type source int

const (
	sourceUnknown source = iota
	sourceCache
	sourceNetwork
)

// loadOne 取得一个 ASN 的前缀集合，并说明来源。
func (r *Resolver) loadOne(ctx context.Context, asn string) (*Set, source, error) {
	if prefixes, err := r.cachedPrefixes(asn); err == nil {
		set, buildErr := buildSet(prefixes, r.opts.Logf)
		if buildErr == nil {
			return set, sourceCache, nil
		}
	}

	fetched, fetchErr := r.fetch(ctx, asn)
	if fetchErr != nil {
		// 抓取失败时退回**过期缓存**：过期的前缀仍然比没有强，
		// 因为骨干线路的 IP 段变化很慢。
		if stale, staleErr := r.readCache(asn); staleErr == nil {
			r.opts.Logf("%s 抓取失败（%v），使用过期缓存", asn, fetchErr)
			set, buildErr := buildSet(stale, r.opts.Logf)
			if buildErr == nil {
				return set, sourceCache, nil
			}
		}
		return nil, sourceUnknown, fetchErr
	}

	// 抓到了空列表（该 ASN 没有该地址族的前缀）也要缓存，
	// 否则每次运行都会再去问一遍同一个"没有"。
	if writeErr := r.writeCache(asn, fetched); writeErr != nil {
		// 写不进缓存不影响本次使用，只影响下次。
		r.opts.Logf("缓存 %s 写入失败：%v", asn, writeErr)
	}

	set, buildErr := buildSet(fetched, r.opts.Logf)
	if buildErr != nil {
		return nil, sourceUnknown, buildErr
	}
	return set, sourceNetwork, nil
}

// buildSet 把前缀列表变成集合。
func buildSet(prefixes []string, logf func(string, ...any)) (*Set, error) {
	set, skipped, err := NewSet(prefixes)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		logf("跳过 %d 条无法解析的前缀", skipped)
	}
	return set, nil
}

// fetch 从 RouteViews 抓一个 ASN 的 IPv4 与 IPv6 前缀。
func (r *Resolver) fetch(ctx context.Context, asn string) ([]string, error) {
	number := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(asn)), "AS")
	if number == "" {
		return nil, fmt.Errorf("asnprefix: empty asn")
	}

	var all []string
	var errs []error

	// 两个地址族分开抓：接口按 af 参数区分，而一次请求拿不到两者。
	for _, af := range []string{"4", "6"} {
		prefixes, err := r.fetchOne(ctx, number, af)
		if err != nil {
			errs = append(errs, fmt.Errorf("af=%s: %w", af, err))
			continue
		}
		all = append(all, prefixes...)
	}

	// 两个地址族都失败才算失败；只有一个成功仍然有用
	// （纯 IPv4 网络里 af=6 失败完全不影响）。
	if len(all) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return all, nil
}

// fetchOne 抓一个 ASN 单个地址族的前缀。
func (r *Resolver) fetchOne(ctx context.Context, number, af string) ([]string, error) {
	url := fmt.Sprintf("%s/%s?af=%s", strings.TrimRight(r.opts.BaseURL, "/"), number, af)

	reqCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 明确要求 JSON：这个接口默认就返回 JSON，但带上更稳妥。
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 404 表示这个 ASN 没有该地址族的前缀，不是错误。
		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	return decodePrefixes(body)
}

// decodePrefixes 解析接口返回的 JSON 数组。
//
// 上游返回的是形如 ["1.71.103.0/24", ...] 的数组。
// 也容忍空响应（当成"没有前缀"）。
func decodePrefixes(body []byte) ([]string, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, nil
	}

	var prefixes []string
	if err := json.Unmarshal([]byte(trimmed), &prefixes); err != nil {
		return nil, fmt.Errorf("asnprefix: decode response: %w", err)
	}
	return prefixes, nil
}

// httpClient 返回可用的 HTTP 客户端。
func (r *Resolver) httpClient() *http.Client {
	if r.opts.HTTPClient != nil {
		return r.opts.HTTPClient
	}
	return &http.Client{Timeout: r.opts.Timeout}
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

// cacheFile 返回某个 ASN 的缓存文件路径。
func (r *Resolver) cacheFile(asn string) string {
	name := strings.ToUpper(strings.TrimSpace(asn))
	return filepath.Join(r.opts.Dir, name+".json")
}

// cachedPrefixes 读取**未过期**的缓存。
func (r *Resolver) cachedPrefixes(asn string) ([]string, error) {
	info, err := os.Stat(r.cacheFile(asn))
	if err != nil {
		return nil, err
	}
	if r.opts.Now().Sub(info.ModTime()) > r.opts.TTL {
		return nil, fmt.Errorf("asnprefix: cache for %s expired", asn)
	}
	return r.readCache(asn)
}

// readCache 读取缓存，不检查有效期。
func (r *Resolver) readCache(asn string) ([]string, error) {
	blob, err := os.ReadFile(r.cacheFile(asn))
	if err != nil {
		return nil, err
	}
	var prefixes []string
	if err := json.Unmarshal(blob, &prefixes); err != nil {
		return nil, fmt.Errorf("asnprefix: parse cache for %s: %w", asn, err)
	}
	return prefixes, nil
}

// writeCache 原子写入缓存。
//
// 先写临时文件再改名：直接覆盖时若中途失败（磁盘满、进程被杀），
// 会留下一个半截的 JSON，而下次读取会把"缓存损坏"当成"没有缓存"，
// 于是又要联网——原子写避免这一类无谓的重抓。
func (r *Resolver) writeCache(asn string, prefixes []string) error {
	if err := os.MkdirAll(r.opts.Dir, 0o755); err != nil {
		return err
	}

	blob, err := json.Marshal(prefixes)
	if err != nil {
		return err
	}

	target := r.cacheFile(asn)
	temp, err := os.CreateTemp(r.opts.Dir, filepath.Base(target)+".tmp*")
	if err != nil {
		return err
	}
	tempName := temp.Name()

	if _, err := temp.Write(blob); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	return os.Rename(tempName, target)
}

// withDefaults 填默认值。
func withDefaults(opts Options) Options {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = DefaultDir
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return opts
}

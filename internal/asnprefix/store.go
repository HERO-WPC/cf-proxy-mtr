package asnprefix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
)

// 默认参数。
const (
	// DefaultDir 是前缀缓存目录（相对工作目录）。
	DefaultDir = "data/asnprefix"

	// DefaultTTL 是缓存的有效期。
	//
	// 7 天是个折中：前缀会变，但一条骨干线路的 IP 段不会天天变；
	// 而每次跟踪都去下载整张映射表，既慢又没必要。
	DefaultTTL = 7 * 24 * time.Hour

	// DefaultTimeout 是单次 HTTP 请求的超时（用于按 ASN 的备用源）。
	DefaultTimeout = 20 * time.Second

	// DefaultBulkTimeout 是全量映射表的下载超时。
	//
	// 比单 ASN 的超时宽松得多：实测 8.6 MB 要 9 秒左右，
	// 慢一点的网络需要更多余量。
	DefaultBulkTimeout = 120 * time.Second

	// fetchAttempts 是最大尝试次数。
	//
	// 上游会暂时性失败（实测同一批请求上一分钟失败、下一分钟正常），
	// 而一次抖动就意味着某些线路的前缀整个缺失。
	fetchAttempts = 3

	// fetchBackoff 是首次重试前的等待，之后翻倍。
	fetchBackoff = 400 * time.Millisecond
)

// Options 是构造 Resolver 的参数。
type Options struct {
	// Dir 是缓存目录（空表示用 DefaultDir）。
	Dir string

	// BulkURL 是全量映射表的地址（空表示用 DefaultBulkURL）。
	BulkURL string

	// TTL 是缓存有效期（<=0 表示用 DefaultTTL）。
	TTL time.Duration

	// BulkTimeout 是下载全量映射表的超时（<=0 表示用 DefaultBulkTimeout）。
	BulkTimeout time.Duration

	// ASNs 是要识别的 ASN 列表（空表示用 asnmap.Known()）。
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

// Load 构造 Resolver。
//
// 加载策略是"**先看缓存，缓存不全就整张表下载一次**"：
//
//  1. 30 个 ASN 的缓存全部新鲜 -> 直接返回，**不联网**；
//  2. 否则下载一次全量映射表，从中抽出这 30 个 ASN 的段，
//     并写进缓存；
//  3. 下载失败则退回已有缓存（**过期也比没有强**）。
//
// 为什么不是"每个 ASN 查一次"：那样会得到**半成品**状态——
// 60 个请求成功 18 个，于是 12 条线路静默失去识别能力，
// 而使用者只看到"线路名少了几条"。整张表下载是**全有或全无**，
// 而且请求数从 60 降到 1。
//
// 它**不返回错误**。理由：线路名是锦上添花，拿不到不该让
// 整轮测量失败——探测结果是主要产出，而且已经在写 CSV 了。
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

	// ---- 1) 缓存全新鲜？那就完全不联网 ----
	if resolver.loadAllFromFreshCache() {
		opts.Logf("就绪 %d/%d 个 ASN（全部命中缓存，未联网）", len(resolver.byASN), len(asns))
		return resolver
	}

	// ---- 2) 整张表下载一次 ----
	wanted := make(map[string]bool, len(asns))
	for _, asn := range asns {
		wanted[normalizeASNKey(asn)] = true
	}

	bulk, err := resolver.fetchBulk(ctx, wanted)
	if err != nil {
		opts.Logf("全量映射表不可用：%v", err)

		// 退回已有缓存（含过期）。半份前缀仍然能认出部分线路，
		// 比完全认不出来强。
		resolver.loadAllFromAnyCache()
		opts.Logf("退回本地缓存：就绪 %d/%d 个 ASN（线路名可能少认一些）",
			len(resolver.byASN), len(asns))
		return resolver
	}

	// ---- 3) 落到内存与磁盘 ----
	var withData int
	for _, asn := range asns {
		key := normalizeASNKey(asn)
		prefixes := bulk[key]

		// 即使这个 ASN 在表里没有段也要写缓存（空数组）：
		// 空结果同样是一次有效回答，否则每次运行都会重新下载整张表
		// 只为再一次确认"它真的没有段"。
		if writeErr := resolver.writeCache(asn, prefixes); writeErr != nil {
			opts.Logf("缓存 %s 写入失败：%v", asn, writeErr)
		}

		if len(prefixes) == 0 {
			continue
		}
		set, _, buildErr := NewSet(prefixes)
		if buildErr != nil {
			continue
		}
		if set.Len() == 0 {
			continue
		}

		resolver.mu.Lock()
		resolver.byASN[asn] = set
		resolver.mu.Unlock()
		withData++
	}

	opts.Logf("就绪 %d/%d 个 ASN（本次下载全量映射表后推导）", withData, len(asns))
	if withData == 0 {
		opts.Logf("没有任何前缀数据，线路名将无法识别")
	}

	return resolver
}

// loadAllFromFreshCache 尝试只用新鲜缓存填满 resolver。
//
// 返回 true 表示**全部** ASN 都从新鲜缓存里拿到了（可以完全不联网）。
// 只要有一个缺失或过期就返回 false——那时整张表下载一次更划算，
// 也比"一部分新一部分旧"更可预测。
func (r *Resolver) loadAllFromFreshCache() bool {
	loaded := make(map[string]*Set, len(r.asnOrder))

	for _, asn := range r.asnOrder {
		prefixes, err := r.cachedPrefixes(asn)
		if err != nil {
			return false
		}
		set, _, buildErr := NewSet(prefixes)
		if buildErr != nil {
			return false
		}
		loaded[asn] = set
	}

	r.mu.Lock()
	r.byASN = loaded
	r.mu.Unlock()
	return true
}

// loadAllFromAnyCache 用**任何**可用的缓存填满 resolver（不检查有效期）。
//
// 只在下载失败时使用：此时过期的前缀仍然有价值。
func (r *Resolver) loadAllFromAnyCache() {
	for _, asn := range r.asnOrder {
		if _, ok := r.byASN[asn]; ok {
			continue
		}
		prefixes, err := r.readCache(asn)
		if err != nil {
			continue
		}
		set, _, buildErr := NewSet(prefixes)
		if buildErr != nil || set.Len() == 0 {
			continue
		}
		r.mu.Lock()
		r.byASN[asn] = set
		r.mu.Unlock()
	}
}

// normalizeASNKey 把 ASN 统一成 "AS<number>" 形式。
//
// asnmap 用 "AS4809"，而全量表里是裸数字 "4809"，
// 两边对不上就会一个都匹配不到——那种错误不会有任何提示。
func normalizeASNKey(asn string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(asn))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "AS") {
		return trimmed
	}
	return "AS" + trimmed
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

// cacheFile 返回某个 ASN 的缓存文件路径。
func (r *Resolver) cacheFile(asn string) string {
	// 用规范化后的名字做文件名：asnmap 给的是 "AS4809"，
	// 而手工构造时可能传 "4809"，两者必须落到同一个文件。
	name := normalizeASNKey(asn)
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
//
// 空数组是合法内容（表示"这个 ASN 没有段"），因此不能用
// "读到了但长度为 0"当成失败。
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
// 于是又要下载整张表——原子写避免这一类无谓的重下。
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

// httpClient 返回可用的 HTTP 客户端。
func (r *Resolver) httpClient() *http.Client {
	if r.opts.HTTPClient != nil {
		return r.opts.HTTPClient
	}
	return &http.Client{Timeout: r.opts.BulkTimeout}
}

// withDefaults 填默认值。
func withDefaults(opts Options) Options {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = DefaultDir
	}
	if strings.TrimSpace(opts.BulkURL) == "" {
		opts.BulkURL = DefaultBulkURL
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.BulkTimeout <= 0 {
		opts.BulkTimeout = DefaultBulkTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	} else {
		// 统一加模块前缀：调用方（service）只提供自己的 Logf，
		// 不该被迫在每条消息里重复模块名。
		inner := opts.Logf
		opts.Logf = func(format string, args ...any) {
			inner("asnprefix: "+format, args...)
		}
	}
	return opts
}

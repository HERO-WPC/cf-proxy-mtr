package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// LoadOptions 控制一次"取得目标列表"的行为。
//
// 这些字段与 CLI 参数一一对应，但刻意不依赖任何 CLI 概念，
// 使后续阶段（scan / probe）可以直接复用同一套策略。
type LoadOptions struct {
	// Refresh 为真时忽略缓存，强制走网络。
	Refresh bool

	// ForceFetch 为真时，即使缓存命中也不使用缓存，必须联网下载。
	//
	// 与 Refresh 的区别：Refresh 表示"不要因为缓存新鲜就跳过下载"，
	// ForceFetch 语义更强，用于 "-f" 这类明确要求重新下载的场景。
	// 当前 CLI 中两者都映射到 --refresh。
	ForceFetch bool

	// CachePath 覆盖配置中的缓存路径（留空则使用 Config.CachePath）。
	CachePath string

	// CacheTTL 覆盖默认缓存有效期（<=0 表示使用 DefaultCacheTTL）。
	CacheTTL time.Duration

	// AllowStale 为真时，网络失败后允许使用过期缓存。
	AllowStale bool

	// WantConfig 为真时额外返回缓存中记录的"原始配置内容"。
	//
	// 目标列表会被归一化并去重，因此无法从目标反推出原始的 aliases / 备注等字段。
	// 需要保留这些字段的调用方（例如后续阶段生成配置文件）应当打开该选项。
	WantConfig bool

	// Now 允许注入当前时间（测试使用）。零值表示使用 time.Now()。
	Now func() time.Time
}

// Result 是 Load 的结果。
type Result struct {
	// Targets 是去重后的完整目标列表。
	Targets []model.Target

	// Meta 是数据源元数据。
	Meta SourceMeta

	// Stats 是解析统计。来自缓存时是当时解析的统计。
	Stats ParseStats

	// Warnings 是解析告警（已限量）。
	Warnings []string

	// FromCache 表示目标列表来自本地缓存文件。
	FromCache bool

	// Stale 表示使用了已过期缓存（网络失败后的降级路径）。
	Stale bool

	// CachePath 是实际使用的缓存路径；为空表示没有写入缓存
	// （例如 FetchOnly 模式或缓存写入失败）。
	CachePath string

	// CacheWritten 表示本次调用是否写入了缓存。
	CacheWritten bool

	// CacheError 记录缓存写入失败的原因。
	//
	// 缓存只是加速手段：写入失败不影响本次测量，
	// 因此这里记录为警告而不是让整次调用失败。
	CacheError error

	// FetchedAt 是成功下载的时间（来自缓存时为缓存写入时间）。
	FetchedAt time.Time

	// Bytes 是下载到的原始响应大小（来自缓存时为 0）。
	Bytes int

	// Attempts 是实际发出的 HTTP 请求次数（来自缓存时为 0）。
	Attempts int

	// FetchErrors 是各数据源地址的失败原因摘要。
	FetchErrors []string

	// Raw 是原始响应内容（仅 FetchOnly 模式返回，便于测试与排查）。
	Raw []byte
}

// RawConfig 是从缓存文件中提取的"原始配置内容"。
//
// 之所以需要它：Load 会把 aliases 等目标级配置归一化成
// model.Location，无法从中恢复原始字段。需要保留原始字段的调用方
// 应使用 LoadOptions.WantConfig。
type RawConfig struct {
	// JSON 是缓存的原始 JSON 内容。
	JSON []byte

	// Source 是缓存记录的数据源地址。
	Source string

	// WrittenAt 是缓存写入时间。
	WrittenAt time.Time
}

// ErrNoData 表示没有任何可用数据（缓存与网络都失败或都为空）。
var ErrNoData = errors.New("no target data available")

// debugf 在设置 CF_ROUTE_TESTER_DEBUG=1 时输出诊断信息到 stderr。
//
// 用于排查"到底走了缓存还是网络"这类问题。默认静默，
// 不影响正常输出，也不写任何敏感信息。
func debugf(format string, args ...any) {
	if os.Getenv("CF_ROUTE_TESTER_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
}

// Loader 实现"取得完整目标列表"的完整策略：
//
//	新鲜缓存 ──────────────► 直接使用
//	    │ 未命中 / 过期 / --refresh
//	    ▼
//	下载主源 ── 失败 ──► 下载备用源 ── 失败 ──► 回退过期缓存（AllowStale）
//	    │ 成功
//	    ▼
//	解析（容错）──► 写缓存（原子）──► 返回
type Loader struct {
	cfg     Config
	fetcher *Fetcher
}

// NewLoader 创建 Loader 并校验配置。
//
// 配置非法（URL 无法解析等）时返回 error，属于全局错误。
func NewLoader(cfg Config) (*Loader, error) {
	fetcher, err := NewFetcher(cfg)
	if err != nil {
		return nil, err
	}
	return &Loader{cfg: fetcher.Config(), fetcher: fetcher}, nil
}

// Config 返回生效配置。
func (l *Loader) Config() Config { return l.cfg }

// FetchOnly 只下载并解析，不读取也不写入缓存。
//
// 用途：
//   - `cf-route-tester fetch --no-cache`；
//   - 测试网络与解析链路，而不污染本地缓存。
func (l *Loader) FetchOnly(ctx context.Context) (*Result, error) {
	fr, err := l.fetcher.Fetch(ctx)
	if err != nil {
		return nil, err
	}

	parsed, format, err := ParseAuto(fr.Body)
	if err != nil {
		return nil, fmt.Errorf("%s (%s, %s): %w",
			fr.URL, format, ContentLengthHint(len(fr.Body)), err)
	}

	parsed.Meta.URL = fr.URL
	if parsed.Meta.Format == "" {
		parsed.Meta.Format = format
	}

	return &Result{
		Targets:     parsed.Targets,
		Meta:        parsed.Meta,
		Stats:       parsed.Stats,
		Warnings:    parsed.Warnings,
		FetchedAt:   time.Now().UTC(),
		Bytes:       len(fr.Body),
		Attempts:    fr.Attempts,
		FetchErrors: fr.Errors,
		Raw:         fr.Body,
	}, nil
}

// Load 按策略取得目标列表，并按需写入缓存。
func (l *Loader) Load(ctx context.Context, opts LoadOptions) (*Result, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	cachePath := strings.TrimSpace(opts.CachePath)
	if cachePath == "" {
		cachePath = l.cfg.CachePath
	}

	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}

	wantNetwork := opts.Refresh || opts.ForceFetch

	// 1) 尝试命中新鲜缓存。
	if !wantNetwork {
		if res, ok := l.tryCache(cachePath, ttl, now()); ok {
			return res, nil
		}
	}
	debugf("load: going to network cachePath=%q wantNetwork=%v urls=%v", cachePath, wantNetwork, l.cfg.urls())

	// 2) 走网络。
	fr, fetchErr := l.fetcher.Fetch(ctx)
	if fetchErr == nil {
		// 注意：这里先解析，再写缓存。
		// 解析失败说明数据源内容不可用，此时不应覆盖已有缓存。
		parsed, format, perr := ParseAuto(fr.Body)
		if perr == nil {
			parsed.Meta.URL = fr.URL
			if parsed.Meta.Format == "" {
				parsed.Meta.Format = format
			}

			res := &Result{
				Targets:     parsed.Targets,
				Meta:        parsed.Meta,
				Stats:       parsed.Stats,
				Warnings:    parsed.Warnings,
				FetchedAt:   now().UTC(),
				Bytes:       len(fr.Body),
				Attempts:    fr.Attempts,
				FetchErrors: fr.Errors,
				CachePath:   cachePath,
			}
			if len(parsed.Warnings) > 0 {
				res.Stats.Reasons["parse warnings"] = len(parsed.Warnings)
			}

			if err := WriteCache(cachePath, parsed.Meta, parsed.Stats, parsed.Targets); err != nil {
				res.CacheError = err
			} else {
				res.CacheWritten = true
			}
			return res, nil
		}
		fetchErr = fmt.Errorf("%s (%s, %s): %w",
			fr.URL, format, ContentLengthHint(len(fr.Body)), perr)
	}

	// 3) 网络失败：在允许时回退到过期缓存。
	if opts.AllowStale {
		if res, ok := l.tryStaleCache(cachePath); ok {
			res.FetchErrors = append(res.FetchErrors, fetchErr.Error())
			return res, nil
		}
	}

	// 4) 彻底失败：把"网络/数据源失败"的原因原样上抛，
	//    由上层决定提示与退出码。
	return nil, fetchErr
}

// tryCache 在缓存存在、可解析、未过期且与当前主数据源匹配时返回缓存结果。
func (l *Loader) tryCache(cachePath string, ttl time.Duration, now time.Time) (*Result, bool) {
	info, err := InspectCache(cachePath, now)
	if err != nil {
		// 缓存缺失、损坏或 schema 不匹配：一律当作未命中，
		// 不打断主流程（下一次成功下载会重写缓存）。
		debugf("tryCache: inspect failed path=%q err=%v", cachePath, err)
		return nil, false
	}
	if info.TargetCount == 0 {
		return nil, false
	}
	if ttl > 0 && info.Age > ttl {
		debugf("tryCache: expired path=%q age=%s ttl=%s", cachePath, info.Age, ttl)
		return nil, false
	}
	if !l.sourceMatches(info.SourceURL) {
		debugf("tryCache: source mismatch path=%q cached=%q primary=%q",
			cachePath, info.SourceURL, l.cfg.URL)
		return nil, false
	}
	return l.readCacheResult(cachePath, now)
}

// tryStaleCache 在网络失败后读取缓存，不检查新鲜度与数据源匹配。
//
// 这里刻意放宽 source 匹配：用户已经明确允许使用过期缓存，
// 此时"有数据可用"比"数据来自哪个源"更重要；
// 结果中的 Stale 标记与 FetchErrors 会让上层如实告知用户。
func (l *Loader) tryStaleCache(cachePath string) (*Result, bool) {
	return l.readCacheResult(cachePath, time.Now())
}

// readCacheResult 完整读取缓存并转换为 Result。
func (l *Loader) readCacheResult(cachePath string, now time.Time) (*Result, bool) {
	meta, stats, targets, writtenAt, err := ReadCache(cachePath)
	if err != nil || len(targets) == 0 {
		return nil, false
	}

	return &Result{
		Targets:   targets,
		Meta:      *meta,
		Stats:     stats,
		FromCache: true,
		Stale:     now.Sub(writtenAt) > DefaultCacheTTL,
		FetchedAt: writtenAt,
		CachePath: cachePath,
	}, true
}

// sourceMatches 判断缓存记录的数据源是否就是**本次运行的主数据源**。
//
// 为什么只与主源比较，而不是与 [主源, 备用源] 比较：
// 备用源的内容与主源不同（例如 all.txt 只有国家代码、没有城市与坐标），
// 若把"由备用源写入的缓存"当成主源的缓存命中，就会出现
// "--url all.json 却返回 all.txt 解析结果" 这种静默的数据替换。
//
// 因此约定：缓存只与当前主源匹配，不匹配就重新下载。
// 结果正确性优先于"省一次下载"。
func (l *Loader) sourceMatches(cachedURL string) bool {
	cached := strings.TrimSpace(cachedURL)
	if cached == "" {
		// 早期缓存没有记录源地址：保守接受，
		// 避免因为一个可选字段就让所有旧缓存失效。
		return true
	}
	primary := strings.TrimSpace(l.cfg.URL)
	if primary == "" {
		return true
	}
	return strings.EqualFold(primary, cached)
}

// LoadRaw 读取缓存文件中的原始内容。
//
// 用于需要保留原始字段的场景（见 RawConfig 说明）。
func LoadRaw(cachePath string) (*RawConfig, error) {
	blob, err := readFileLimited(cachePath, maxBodyBytes)
	if err != nil {
		return nil, err
	}

	var head struct {
		WrittenAt time.Time `json:"written_at"`
		Source    struct {
			URL string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(blob, &head); err != nil {
		return nil, fmt.Errorf("decode cache %q: %w", cachePath, err)
	}

	return &RawConfig{
		JSON:      blob,
		Source:    head.Source.URL,
		WrittenAt: head.WrittenAt,
	}, nil
}

// ---------------------------------------------------------------------------
// 格式自动识别
// ---------------------------------------------------------------------------

// ParseAuto 自动识别内容格式并解析。
//
// 识别规则（按顺序）：
//
//  1. 去掉 BOM 与前置空白后，首字符是 '{' 或 '[' -> 按 JSON 解析；
//  2. 否则 -> 按文本列表解析。
//
// 这样无论上游把 JSON 内容放在 all.txt、还是把文本放在 all.json，
// 都能正确处理：格式判断依据内容，而不是 URL 后缀或 Content-Type。
//
// 返回的第二个值是实际识别的格式（"json" / "text"）。
func ParseAuto(data []byte) (*ParseResult, string, error) {
	if len(bytes.TrimSpace(stripBOM(data))) == 0 {
		return nil, "", errors.New("empty content")
	}

	if looksLikeJSON(data) {
		res, err := ParseJSON(data)
		if err != nil {
			return nil, "json", err
		}
		return res, "json", nil
	}

	res, err := ParseText(data)
	if err != nil {
		return nil, "text", err
	}
	return res, "text", nil
}

// looksLikeJSON 判断内容是否像 JSON。
func looksLikeJSON(data []byte) bool {
	head := stripBOM(data)
	head = bytes.TrimLeft(head, " \t\r\n")
	if len(head) == 0 {
		return false
	}
	return head[0] == '{' || head[0] == '['
}

// stripBOM 去掉 UTF-8 BOM。
//
// 一些 Windows 工具生成的文本文件会带 BOM，
// 不去掉的话 JSON 解析会直接失败。
func stripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

// readFileLimited 读取文件，并限制最大读取字节数。
//
// 限制大小是为了避免误把巨大文件（例如把 all.json 指向了数据库）读进内存。
func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	blob, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(blob)) > limit {
		return nil, fmt.Errorf("file %q exceeds %d bytes limit", path, limit)
	}
	return blob, nil
}

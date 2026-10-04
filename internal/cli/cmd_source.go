package cli

import (
	"context"
	"flag"

	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// sourceParams 是"如何取得目标列表"的参数集合。
//
// probe / scan / trace 都需要同一套数据源参数，因此集中定义一次，
// 避免三个子命令各写一套、行为逐渐漂移。
type sourceParams struct {
	url         string
	fallbackURL string
	cachePath   string
	refresh     bool

	// cacheFirst 要回"新鲜缓存直接生效、不联网"的旧行为。
	//
	// 默认（false）是网络优先：每次都先试着下载最新的目标列表。
	// 需要离线可复现时（CI 用固定缓存跑固定目标）才打开它。
	cacheFirst bool
	timeout    durationFlag
	retries    int
	proxy      string
}

// registerSourceFlags 把数据源参数注册到 FlagSet 上。
func registerSourceFlags(fs *flag.FlagSet, p *sourceParams) {
	fs.StringVar(&p.url, "url", source.DefaultURL, "数据源地址（默认 all.json）")
	fs.StringVar(&p.fallbackURL, "fallback-url", source.DefaultFallbackURL, "备用数据源地址（留空表示禁用）")
	fs.StringVar(&p.cachePath, "cache", source.DefaultCachePath, "本地缓存路径")
	fs.BoolVar(&p.refresh, "refresh", false, "忽略缓存，强制重新下载目标列表")
	fs.BoolVar(&p.cacheFirst, "source-cache-first", false,
		"优先使用本地缓存，不去联网（默认是网络优先：先下载，下载不到才用缓存）")
	fs.Var(&p.timeout, "source-timeout", "下载目标列表的单次 HTTP 超时（默认 30s）")
	fs.IntVar(&p.retries, "source-retries", source.DefaultRetries, "下载目标列表的重试次数")
	fs.StringVar(&p.proxy, "proxy", "", "HTTP(S) 代理地址，例如 http://127.0.0.1:10808")
}

// toConfig 把参数转成 source.Config。
//
// 抽出来是为了让所有子命令（包括 web）用同一套转换规则：
// 各自手写一份迟早会出现"某个命令忘了传 proxy"这类漂移。
func (p sourceParams) toConfig() source.Config {
	cfg := source.DefaultConfig()
	cfg.URL = p.url
	cfg.FallbackURL = p.fallbackURL
	cfg.CachePath = p.cachePath
	cfg.Retries = p.retries
	cfg.Proxy = p.proxy
	if p.timeout.set {
		cfg.Timeout = p.timeout.d
	}
	return cfg
}

// loadTargets 按参数取得目标列表。
//
// **网络优先**：先试着下载最新的目标列表并写缓存；下载失败才退回
// 本地缓存（不看新鲜度）。--source-cache-first 可以要回"缓存优先"。
//
// 这样 probe / scan 的使用者不需要先手工执行一次 fetch，
// 又能默认拿到最新目标——上游随时会增删目标，用一份几小时前的
// 列表意味着这段时间新增的目标一个都测不到，且没有提示。
func loadTargets(ctx context.Context, p sourceParams) (*source.Result, error) {
	loader, err := source.NewLoader(p.toConfig())
	if err != nil {
		// 配置错误属于全局错误：终止当前任务。
		return nil, err
	}

	return loader.Load(ctx, source.LoadOptions{
		Refresh:    p.refresh,
		ForceFetch: p.refresh,
		CachePath:  p.cachePath,
		CacheFirst: p.cacheFirst,
	})
}

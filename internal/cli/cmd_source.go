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
	timeout     durationFlag
	retries     int
	proxy       string
}

// registerSourceFlags 把数据源参数注册到 FlagSet 上。
func registerSourceFlags(fs *flag.FlagSet, p *sourceParams) {
	fs.StringVar(&p.url, "url", source.DefaultURL, "数据源地址（默认 all.json）")
	fs.StringVar(&p.fallbackURL, "fallback-url", source.DefaultFallbackURL, "备用数据源地址（留空表示禁用）")
	fs.StringVar(&p.cachePath, "cache", source.DefaultCachePath, "本地缓存路径")
	fs.BoolVar(&p.refresh, "refresh", false, "忽略缓存，强制重新下载目标列表")
	fs.Var(&p.timeout, "source-timeout", "下载目标列表的单次 HTTP 超时（默认 30s）")
	fs.IntVar(&p.retries, "source-retries", source.DefaultRetries, "下载目标列表的重试次数")
	fs.StringVar(&p.proxy, "proxy", "", "HTTP(S) 代理地址，例如 http://127.0.0.1:10808")
}

// loadTargets 按参数取得目标列表。
//
// 行为与 fetch 一致：缓存新鲜则用缓存，否则联网下载并写缓存；
// 网络失败时允许回退过期缓存。这样 probe / scan 的用户
// 不需要先手工执行一次 fetch。
func loadTargets(ctx context.Context, p sourceParams) (*source.Result, error) {
	cfg := source.DefaultConfig()
	cfg.URL = p.url
	cfg.FallbackURL = p.fallbackURL
	cfg.CachePath = p.cachePath
	cfg.Retries = p.retries
	cfg.Proxy = p.proxy
	if p.timeout.set {
		cfg.Timeout = p.timeout.d
	}

	loader, err := source.NewLoader(cfg)
	if err != nil {
		// 配置错误属于全局错误：终止当前任务。
		return nil, err
	}

	return loader.Load(ctx, source.LoadOptions{
		Refresh:    p.refresh,
		ForceFetch: p.refresh,
		CachePath:  p.cachePath,
		AllowStale: true,
	})
}

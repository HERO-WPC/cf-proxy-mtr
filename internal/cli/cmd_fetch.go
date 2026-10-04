package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/source"
)

// fetchParams 是 `cf-route-tester fetch` 的参数。
type fetchParams struct {
	url         string
	fallbackURL string
	cachePath   string
	noCache     bool
	refresh     bool

	// cacheFirst 要回"新鲜缓存直接生效、不联网"的行为。
	//
	// fetch 的语义是"去把目标列表下载下来"，因此默认每次都下载；
	// 需要"只是确保本地有一份"时（脚本里反复调用）打开它。
	cacheFirst bool
	allowStale bool
	timeout    time.Duration
	retries    int
	proxy      string
	verbose    bool
}

// newFetchCommand 构造 fetch 子命令。
func newFetchCommand() Command {
	return Command{
		Name:    "fetch",
		Summary: "下载并缓存 all.json 目标列表",
		Usage:   ClientName + " fetch [flags]",
		Run:     runFetch,
		Flags: func(w io.Writer) {
			var p fetchParams
			fs := fetchFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()
		},
	}
}

// runFetch 实现 `cf-route-tester fetch`。
//
// 职责边界：本函数只做参数解析、调用 internal/source、以及把结果打印成
// 人类可读的形式。所有下载 / 解析 / 缓存逻辑都在 internal/source 内，
// 因此这一层很薄，也便于后续阶段复用。
func runFetch(env *Env, args []string) error {
	p, err := parseFetchFlags(args)
	if err != nil {
		return err
	}

	// Ctrl+C / SIGTERM 时取消正在进行的下载。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := source.DefaultConfig()
	cfg.URL = p.url
	cfg.FallbackURL = p.fallbackURL
	cfg.CachePath = p.cachePath
	cfg.Timeout = p.timeout
	cfg.Retries = p.retries
	cfg.Proxy = p.proxy

	loader, err := source.NewLoader(cfg)
	if err != nil {
		// 配置错误属于全局错误：终止当前任务。
		return err
	}

	started := time.Now()

	var res *source.Result
	if p.noCache {
		res, err = loader.FetchOnly(ctx)
	} else {
		res, err = loader.Load(ctx, source.LoadOptions{
			Refresh:         p.refresh,
			ForceFetch:      p.refresh,
			CachePath:       p.cachePath,
			CacheFirst:      p.cacheFirst,
			NoStaleFallback: !p.allowStale,
		})
	}
	if err != nil {
		return err
	}

	printFetchResult(env.Stdout, res, time.Since(started), p)

	return nil
}

// fetchFlagSet 构造 fetch 的 FlagSet，供参数解析与帮助输出共用。
//
// 只构造一次参数定义，避免"帮助里显示的参数"与"实际解析的参数"不一致。
func fetchFlagSet(p *fetchParams) *flag.FlagSet {
	fs := newFlagSet("fetch")
	fs.StringVar(&p.url, "url", source.DefaultURL, "数据源地址（默认 all.json）")
	fs.StringVar(&p.fallbackURL, "fallback-url", source.DefaultFallbackURL, "备用数据源地址（留空表示禁用）")
	fs.StringVar(&p.cachePath, "cache", source.DefaultCachePath, "本地缓存路径")
	fs.BoolVar(&p.noCache, "no-cache", false, "只下载并解析，不读取也不写入缓存")
	fs.BoolVar(&p.refresh, "refresh", false, "忽略缓存，强制重新下载")
	fs.BoolVar(&p.cacheFirst, "source-cache-first", false,
		"本地已有新鲜缓存时不联网（默认是网络优先：每次都先下载）")
	fs.BoolVar(&p.allowStale, "allow-stale", true, "网络失败时允许回退使用过期缓存")
	fs.DurationVar(&p.timeout, "timeout", source.DefaultTimeout, "单次 HTTP 请求超时")
	fs.IntVar(&p.retries, "retries", source.DefaultRetries, "每个数据源地址的重试次数")
	fs.StringVar(&p.proxy, "proxy", "", "HTTP(S) 代理地址，例如 http://127.0.0.1:10808")
	fs.BoolVar(&p.verbose, "verbose", false, "显示解析告警与全部跳过原因")
	return fs
}

// parseFetchFlags 解析 fetch 参数。
func parseFetchFlags(args []string) (fetchParams, error) {
	var p fetchParams
	fs := fetchFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return p, err
	}
	return p, nil
}

// printFetchResult 输出 fetch 结果。
//
// 输出结构（stdout）：以 "key: value" 形式的摘要为主，
// 便于脚本 grep，也便于人工快速核对。
func printFetchResult(w io.Writer, res *source.Result, elapsed time.Duration, p fetchParams) {
	origin := "network"
	switch {
	case res.FromCache && res.Stale:
		origin = "cache (stale)"
	case res.FromCache:
		origin = "cache"
	}

	fmt.Fprintf(w, "source:       %s\n", displayOrDash(res.Meta.URL))
	fmt.Fprintf(w, "format:       %s\n", displayOrDash(res.Meta.Format))
	fmt.Fprintf(w, "origin:       %s\n", origin)

	if res.Bytes > 0 {
		fmt.Fprintf(w, "downloaded:   %s\n", source.ContentLengthHint(res.Bytes))
	}
	fmt.Fprintf(w, "elapsed:      %s\n", elapsed.Round(time.Millisecond))

	fmt.Fprintf(w, "targets:      %d\n", len(res.Targets))
	if res.Meta.ReportedCount > 0 && res.Meta.ReportedCount != len(res.Targets) {
		// 源自称的数量与实际解析数量不一致时如实报告，
		// 这类差异通常意味着上游把重复项也算进了总数。
		fmt.Fprintf(w, "              (source reports %d)\n", res.Meta.ReportedCount)
	}

	if res.Meta.HasGeneratorTime {
		fmt.Fprintf(w, "generated_at: %s\n", formatTime(res.Meta.GeneratorGeneratedAt))
	}
	fmt.Fprintf(w, "fetched_at:   %s\n", formatTime(res.FetchedAt))

	if res.CacheWritten {
		fmt.Fprintf(w, "cache:        %s (written)\n", res.CachePath)
	} else if res.FromCache {
		fmt.Fprintf(w, "cache:        %s (hit)\n", res.CachePath)
	} else if p.noCache {
		fmt.Fprintf(w, "cache:        disabled (--no-cache)\n")
	}
	if res.CacheError != nil {
		// 缓存写入失败不是致命错误，但必须让用户看到。
		fmt.Fprintf(w, "cache_warn:   %v\n", res.CacheError)
	}

	printParseStats(w, res, p.verbose)

	// 使用过期缓存时必须明确提示，避免用户误以为数据是新的。
	if res.Stale {
		fmt.Fprintf(w, "\nwarning: using STALE cache (network fetch failed)\n")
		for _, e := range res.FetchErrors {
			fmt.Fprintf(w, "  fetch error: %s\n", e)
		}
	}
}

// printParseStats 打印解析统计与告警。
func printParseStats(w io.Writer, res *source.Result, verbose bool) {
	st := res.Stats

	skipped := st.Duplicates + st.InvalidPorts + st.Unknown + st.SkippedItems
	if skipped > 0 {
		fmt.Fprintf(w, "\nparse:\n")
		fmt.Fprintf(w, "  raw records:      %d\n", st.RawItems)
		fmt.Fprintf(w, "  ip:port combos:   %d\n", st.RawCombos)
		fmt.Fprintf(w, "  duplicate combos: %d\n", st.Duplicates)
		fmt.Fprintf(w, "  invalid ports:    %d\n", st.InvalidPorts)
		fmt.Fprintf(w, "  invalid records:  %d\n", st.SkippedItems)
		if st.Unknown > 0 {
			fmt.Fprintf(w, "  no port field:    %d\n", st.Unknown)
		}
	}

	if len(st.Reasons) > 0 {
		fmt.Fprintf(w, "\nskipped by reason:\n")
		for _, line := range sortedReasons(st.Reasons, verbose) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	if len(res.Warnings) > 0 {
		fmt.Fprintf(w, "\nwarnings (%d shown):\n", len(res.Warnings))
		for _, msg := range res.Warnings {
			fmt.Fprintf(w, "  - %s\n", msg)
		}
	}
}

// sortedReasons 把原因计数格式化成"按数量降序"的列表。
//
// verbose 为假时只显示前 5 条，其余合并成一行，避免刷屏。
func sortedReasons(reasons map[string]int, verbose bool) []string {
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(reasons))
	for k, v := range reasons {
		items = append(items, kv{k, v})
	}
	// 先按数量降序，数量相同时按名称升序，保证输出稳定。
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})

	const maxShown = 5
	shown := items
	var rest int
	if !verbose && len(items) > maxShown {
		shown = items[:maxShown]
		for _, it := range items[maxShown:] {
			rest += it.v
		}
	}

	out := make([]string, 0, len(shown)+1)
	for _, it := range shown {
		out = append(out, fmt.Sprintf("%-24s %d", it.k+":", it.v))
	}
	if rest > 0 {
		out = append(out, fmt.Sprintf("%-24s %d", "... others:", rest))
	}
	return out
}

// displayOrDash 把空值显示成 "-"，避免输出里出现 "source:       "。
func displayOrDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

// formatTime 统一时间显示格式（UTC，秒级）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ---------------------------------------------------------------------------
// flag 解析辅助
// ---------------------------------------------------------------------------

// newFlagSet 创建统一的 FlagSet。
//
// 统一设置：
//   - ContinueOnError：把解析错误交给我们自己转换成用法错误，
//     而不是让 flag 包直接退出进程；
//   - 输出重定向：由调用方通过 parseFlags 注入，
//     避免 flag 包把帮助直接写到 os.Stderr。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(ClientName+" "+name, flag.ContinueOnError)
	// 关闭默认的 -h 处理，统一由 cli.dispatch 拦截 --help，
	// 保证 --help 的输出风格一致。
	fs.Usage = func() {}
	return fs
}

// parseFlags 解析参数，并把 flag 包的错误转换为用法错误。
//
// 要点：flag 包遇到 -h 会返回 flag.ErrHelp；
// 由于 dispatch 已经拦截了第一个参数为 --help 的情况，
// 这里只需把 ErrHelp 也映射成用法错误（退出码 2），
// 提示用户改用顶层 --help 查看帮助。
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return usageError("use '%s --help' for usage", fs.Name())
		}
		return usageError("%v", err)
	}
	if fs.NArg() > 0 {
		return usageError("unexpected argument(s): %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

// parseFlagsAllowInterspersed 允许选项与位置参数任意交错。
//
// 为什么需要它：Go 的 flag 包在遇到第一个非选项参数时就**停止解析**，
// 因此 `query 1.1.1.1:443 --db x.db` 里的 --db 会被当成位置参数，
// 报出 "unexpected argument(s)"——而帮助里给出的示例正是这个写法。
//
// 做法：先把 argv 重排成"选项在前、位置参数在后"，再交给标准的
// Parse。重排需要知道哪些选项会吃掉后面一个值，这从
// fs.VisitAll 的 flag.Value 类型推断（IsBoolFlag 接口）。
func parseFlagsAllowInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	reordered, positionals := reorderInterspersed(fs, args)

	if err := fs.Parse(reordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, usageError("use '%s --help' for usage", fs.Name())
		}
		return nil, usageError("%v", err)
	}

	// 正常情况下位置参数已经被排到末尾，fs.Args() 就是它们；
	// 用收集到的列表作为权威结果（避免重排出错时静默丢参数）。
	remaining := fs.Args()
	if len(remaining) > len(positionals) {
		positionals = remaining
	}
	return positionals, nil
}

// boolFlag 是"不需要值"的选项（flag 包用它区分 -v 与 -db x）。
type boolFlag interface {
	IsBoolFlag() bool
}

// reorderInterspersed 把参数重排成"选项及其值在前、位置参数在后"。
func reorderInterspersed(fs *flag.FlagSet, args []string) (reordered []string, positionals []string) {
	// 先收集所有已知选项名与"是否需要值"。
	needsValue := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		isBool := false
		if boolValue, ok := f.Value.(boolFlag); ok {
			isBool = boolValue.IsBoolFlag()
		}
		needsValue[f.Name] = !isBool
	})

	reordered = make([]string, 0, len(args))
	positionals = make([]string, 0, len(args))

	// sawTerminator 记录是否出现过顶层的 "--"。
	//
	// 这个标记必须在重排后的参数里**保留**：它是 flag 包的"到此为止"
	// 标记。早先的版本把 "--" 吃掉了，于是 `-- --hops a` 里的 --hops
	// 会被当成真正的选项解析（实测踩到过），而用户写 "--" 的意图
	// 正是"后面都是位置参数"。
	//
	// 注意 "值位置"上的 "--" 是普通值（例如 `--db --` 的值就是 "--"），
	// 不算终止符——那种情况在下面的值分支里被直接消费掉。
	sawTerminator := false
	inTerminator := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if inTerminator {
			positionals = append(positionals, arg)
			continue
		}
		if arg == "--" {
			sawTerminator = true
			inTerminator = true
			continue
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			// 位置参数。注意 `-` 常被用作"标准输出/输入"的约定值，
			// 因此上面单独放行。
			positionals = append(positionals, arg)
			continue
		}

		// 选项：可能是 -name=value / --name=value，也可能要吃掉下一个参数。
		name := strings.TrimLeft(arg, "-")
		hasInlineValue := false
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
			hasInlineValue = true
		}

		reordered = append(reordered, arg)

		// 需要值且没有内联值时，把下一个参数一并归入选项。
		// 注意这里不检查下一个是不是 "--"：`--db --` 的值就是 "--"。
		if !hasInlineValue && needsValue[name] && i+1 < len(args) {
			i++
			reordered = append(reordered, args[i])
		}
	}

	// 把位置参数接在后面；若用户写过 "--"，则在它们之前补回该标记，
	// 保证重排后它仍然是"到此为止"的位置。
	if sawTerminator {
		reordered = append(reordered, "--")
	}
	return append(reordered, positionals...), positionals
}

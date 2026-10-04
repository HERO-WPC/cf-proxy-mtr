package service

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// 本文件是"先测 TCP、由使用者挑一批再跟踪"里的第二步。
//
// == 为什么是独立的一步，而不是扫描时一并跟踪 ==
//
// 使用者要先看到延迟才能决定跟踪谁。把它做成"扫描时顺手全跟踪"会有
// 两个问题：一是没得选（一个国家的目标可能上千，跟踪慢到不可用），
// 二是跟踪失败会连累整轮扫描的观感。
//
// 拆开还有个实际好处：**TCP 结果已经落盘**。跟踪这一步崩了、被停了、
// 或者使用者改主意了，延迟数据都不会丢——它是从 CSV 里读出来的。
//
// == 目标从 CSV 里来，而不是从浏览器传过来 ==
//
// CSV 是本项目唯一的数据源，界面显示的也是它。让界面把选中的目标
// 回传，就等于承认"界面手里的那份"才是真相，两边一旦不同步
// （比如使用者开着旧页面、或者中途又跑了一轮）就会跟踪一批
// 已经不在文件里的目标。
//
// 因此这里只接收**筛选条件**（国家、延迟上限、条数），由服务端
// 从文件里现算。界面也是同一套条件，所以"预览 12 个"和"实际跟踪
// 12 个"必然一致。

// TraceSelectionOptions 是"从 CSV 里挑一批目标做跟踪"的参数。
type TraceSelectionOptions struct {
	// CSVPath 是要读取的结果文件（也是跟踪结果的落点）。
	CSVPath string

	// Countries 只挑这些国家的行（空表示不筛选）。
	Countries []string

	// MaxLatencyMS 只挑延迟不超过它的行（<=0 表示不限）。
	//
	// 这是使用者最常用的筛选：只跟踪"够快"的那些，
	// 慢的跟了也没意义。
	MaxLatencyMS float64

	// Limit 最多跟踪多少个（<=0 表示不限）。
	//
	// 按延迟从快到慢取。上限很重要：一次 traceroute 实测十几秒，
	// 不限量就可能变成几小时的等待。
	Limit int

	// TraceConfig 是跟踪配置（模式、数据源、并发、超时）。
	TraceConfig TraceOptions

	// NoASNPrefix 为真时不用本地 ASN 前缀识别线路。
	NoASNPrefix bool

	// ASNPrefixOptions 是前缀解析器的配置。
	ASNPrefixOptions asnprefix.Options

	// Progress 接收进度（nil 表示不关心）。
	Progress func(ProgressEvent)

	// OnTrace 在一个目标跟踪完成时调用（可为 nil，会并发调用）。
	OnTrace func(outcome TraceOutcome)

	// traceEngineOverride 用于测试注入引擎。
	traceEngineOverride *trace.NextTraceEngine
}

// SelectionPreview 是"按当前条件会选到哪些目标"的结果。
//
// 界面在按钮旁显示"将跟踪 N 个"用的就是它，与实际执行共用同一段
// 选取逻辑，因此预览和真正跑的目标不会不一致。
type SelectionPreview struct {
	// Count 是会被跟踪的目标数。
	Count int `json:"count"`

	// Total 是文件里可参与挑选的行数（有延迟的）。
	Total int `json:"total"`

	// Countries 是文件里出现过的国家及数量。
	Countries []csvstore.CountryCount `json:"countries"`

	// FastestMS / SlowestMS 是选中集合的延迟范围（没有选中时为零）。
	FastestMS float64 `json:"fastest_ms"`
	SlowestMS float64 `json:"slowest_ms"`

	// Targets 是选中目标的 ID（截断到前若干个，供界面展示）。
	Targets []string `json:"targets"`
}

// SelectForTrace 按条件从 CSV 行里挑出要跟踪的目标。
//
// 返回的目标按延迟从快到慢排序，每个目标只出现一次——
// 一个目标可能有多行（探测一行、跟踪一行），重复跟踪没有意义。
func SelectForTrace(rows []csvstore.Row, countries []string, maxLatencyMS float64, limit int) []model.Target {
	wanted := normalizeCountries(countries)

	type candidate struct {
		target  model.Target
		latency float64
	}
	// 同一个目标可能有多行，取**最快的那一行**：重新跟踪时
	// 依据应该是它最好的表现，而不是最后一次碰巧很差的那次。
	best := make(map[string]candidate, len(rows))

	for _, row := range rows {
		// 没有延迟的行不参与：它可能只是"跟踪失败"的记录，
		// 也可能是一次超时。两种都不该被当成"要跟踪的候选"。
		if row.LatencyMS <= 0 {
			continue
		}
		if len(wanted) > 0 && !wanted[strings.ToUpper(strings.TrimSpace(row.CCA2))] {
			continue
		}
		if maxLatencyMS > 0 && row.LatencyMS > maxLatencyMS {
			continue
		}
		if strings.TrimSpace(row.IP) == "" {
			continue
		}

		id := row.Target
		if id == "" {
			id = fmt.Sprintf("%s:%d", row.IP, row.Port)
		}

		existing, seen := best[id]
		if seen && existing.latency <= row.LatencyMS {
			continue
		}

		// IPVersion 必须填对：跟踪引擎会用 model.Target.Validate 校验
		// "版本与地址是否一致"，空值一律被判为非法目标，整条跟踪
		// 会以 invalid_target 失败。
		//
		// 实测踩过：只填 IP/Port 时每一步都"看起来"正常——选取有结果、
		// 行也写进了 CSV——但 error_type 是 invalid_target，
		// 而 as_path 永远是空的。只测"选取函数"发现不了，因为它的
		// 输出没问题，是引擎的校验拒了。
		addr, addrErr := netip.ParseAddr(row.IP)
		if addrErr != nil {
			continue
		}

		best[id] = candidate{
			latency: row.LatencyMS,
			target: model.Target{
				ID:        id,
				IP:        row.IP,
				Port:      row.Port,
				IPVersion: model.IPVersionOf(addr),
				// 国家从行里带过来，而不是回头去目标列表里查：
				// 目标列表可能已经变了，而 CSV 里这一行就是当时的事实。
				Location: model.Location{CCA2: strings.ToUpper(strings.TrimSpace(row.CCA2))},
			},
		}
	}

	out := make([]candidate, 0, len(best))
	for _, item := range best {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].latency != out[j].latency {
			return out[i].latency < out[j].latency
		}
		return out[i].target.ID < out[j].target.ID
	})

	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}

	targets := make([]model.Target, 0, len(out))
	for _, item := range out {
		targets = append(targets, item.target)
	}
	return targets
}

// PreviewTraceSelection 读 CSV 并按条件预览会选中哪些目标。
//
// 与实际跟踪共用 SelectForTrace，因此界面上的"N 个"就是真正会跑的个数。
func PreviewTraceSelection(path string, countries []string, maxLatencyMS float64, limit int) (SelectionPreview, error) {
	rows, err := csvstore.ReadAll(path)
	if err != nil {
		return SelectionPreview{}, err
	}

	// 先合并同一目标的探测行与跟踪行，再做任何统计。
	//
	// 不合并会**重复计数**：一个目标跟踪过之后在文件里有两行，
	// 于是一个目标被算成两条。界面上的国家数量必须与结果表格里的
	// 行数对得上（表格用的就是合并后的数据），否则使用者会看到
	// "美国 20 个"而表格里只有 12 行。
	rows = csvstore.CollapseByTarget(rows)

	selected := SelectForTrace(rows, countries, maxLatencyMS, limit)

	preview := SelectionPreview{
		Count: len(selected),
		// 国家清单只统计**有延迟的行**。
		//
		// 这是跟踪区的可选项：能跟踪的前提是测出过延迟
		// （SelectForTrace 只挑 LatencyMS > 0）。把只有失败行的国家
		// 也列出来，使用者勾了它却一个都跟踪不了——那是列表在骗人。
		//
		// 与 /api/results 的 countries 分工不同：那边给表格做筛选，
		// 需要包含失败行（否则筛不出"这个国家的失败情况"）。
		Countries: csvstore.Countries(measuredOnly(rows)),
	}

	// Total 是"可参与挑选的目标数"（有延迟的那些），让界面能说清
	// "结果里 200 个，符合条件的有 12 个"。
	for _, row := range rows {
		if row.LatencyMS > 0 {
			preview.Total++
		}
	}

	if len(selected) > 0 {
		latencies := make([]float64, 0, len(selected))
		byID := make(map[string]float64, len(rows))
		for _, row := range rows {
			if row.LatencyMS > 0 {
				if existing, ok := byID[row.Target]; !ok || row.LatencyMS < existing {
					byID[row.Target] = row.LatencyMS
				}
			}
		}
		for _, target := range selected {
			if ms, ok := byID[target.ID]; ok {
				latencies = append(latencies, ms)
			}
		}
		if len(latencies) > 0 {
			sort.Float64s(latencies)
			preview.FastestMS = latencies[0]
			preview.SlowestMS = latencies[len(latencies)-1]
		}
	}

	// 只带前 20 个目标给界面：它要展示的是"大概会跟哪些"，
	// 而不是把整份列表塞进 JSON。
	const previewLimit = 20
	for i, target := range selected {
		if i == previewLimit {
			break
		}
		preview.Targets = append(preview.Targets, target.ID)
	}

	return preview, nil
}

// measuredOnly 只保留测出过延迟的行。
//
// 跟踪区的可选项来自它：没有延迟就无从判断"要跟踪谁"，
// 因此那些行不该出现在可跟踪的国家清单里。
func measuredOnly(rows []csvstore.Row) []csvstore.Row {
	out := make([]csvstore.Row, 0, len(rows))
	for _, row := range rows {
		if row.LatencyMS > 0 {
			out = append(out, row)
		}
	}
	return out
}

// RunTraceSelection 从 CSV 里挑一批目标做线路跟踪，结果追加回同一个文件。
//
// 只跟踪，不重新探测：延迟数据已经在文件里了，重测会多花时间，
// 还会让"同一个目标在文件里出现两行不同延迟"这种难以解释的情况变多。
func (s *Service) RunTraceSelection(ctx context.Context, opts TraceSelectionOptions) (*CSVScanResult, error) {
	path := strings.TrimSpace(opts.CSVPath)
	if path == "" {
		return nil, fmt.Errorf("%w: no csv path given", ErrUsage)
	}
	if opts.Limit < 0 {
		return nil, fmt.Errorf("%w: limit must not be negative", ErrUsage)
	}

	result := &CSVScanResult{
		StartedAt:  time.Now().UTC(),
		OutputPath: path,
	}

	rows, err := csvstore.ReadAll(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	targets := SelectForTrace(rows, opts.Countries, opts.MaxLatencyMS, opts.Limit)
	result.Targets = len(targets)
	if len(targets) == 0 {
		// 不是错误：条件太紧而已。调用方据此提示"没有符合条件的目标"。
		result.FinishedAt = time.Now().UTC()
		s.log("trace: 没有符合条件的目标（国家=%v 延迟上限=%.0fms 条数=%d）",
			opts.Countries, opts.MaxLatencyMS, opts.Limit)
		return result, nil
	}
	s.log("trace: 从 %s 选出 %d 个目标（国家=%v 延迟上限=%.0fms）",
		path, len(targets), opts.Countries, opts.MaxLatencyMS)

	// 组装一份 CSVScanOptions 只为复用引擎与前缀的接线：
	// resolveTraceEngine 与 awaitPrefixWarmup 都吃它，重复写一套
	// 校验与默认值必然与扫描那条路漂移。
	scanOpts := CSVScanOptions{
		OutputPath:          path,
		Append:              true,
		Trace:               true,
		TraceConfig:         opts.TraceConfig,
		NoASNPrefix:         opts.NoASNPrefix,
		ASNPrefixOptions:    opts.ASNPrefixOptions,
		Progress:            opts.Progress,
		OnTrace:             opts.OnTrace,
		Countries:           opts.Countries,
		traceEngineOverride: opts.traceEngineOverride,
	}

	engine, engineErr := s.resolveTraceEngine(ctx, scanOpts)
	if engineErr != nil {
		s.log("trace: trace engine unavailable: %v", engineErr)
		result.TraceUnavailable = engineErr.Error()
		result.FinishedAt = time.Now().UTC()
		return result, nil
	}

	store, err := csvstore.Open(csvstore.Options{Path: path, Append: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			s.log("csv: close failed: %v", cerr)
		}
	}()

	warmer := startPrefixWarmup(ctx, scanOpts, s.log)
	prefixes := awaitPrefixWarmup(warmer, scanOpts, s.log)

	successful := make([]string, 0, len(targets))
	for _, target := range targets {
		successful = append(successful, target.ID)
	}

	s.runTracePhase(ctx, store, engine, prefixes, targets, successful, scanOpts, result)

	if ctx.Err() != nil {
		result.Interrupted = true
	}
	result.FinishedAt = time.Now().UTC()
	s.log("trace: finished traced=%d ok=%d rows=%d errors=%d",
		result.Traced, result.TracedOK, result.RowsWritten, result.Errors)

	if result.Errors > 0 && result.RowsWritten == 0 {
		return result, fmt.Errorf("could not write any result to %s (%d failures); check disk space and permissions",
			store.Path(), result.Errors)
	}
	return result, nil
}

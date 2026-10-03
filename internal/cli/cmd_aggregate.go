package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/aggregate"
)

// aggregateParams 是 `cf-route-tester aggregate` 的参数。
type aggregateParams struct {
	inputs stringList

	out    string
	format string

	topGroups  int
	topTargets int

	minSamples int
	regionMin  int
	maxGroups  int

	jsonOut bool
	quiet   bool
}

// 聚合输出格式。
const (
	aggregateFormatJSON  = "json"
	aggregateFormatText  = "text"
	aggregateFormatJSONL = "jsonl"
)

// newAggregateCommand 构造 aggregate 子命令。
func newAggregateCommand() Command {
	return Command{
		Name:    "aggregate",
		Summary: "把公开 JSONL 聚合为按地区/运营商分组的统计结果",
		Usage:   ClientName + " aggregate --input <file-or-dir> [--input ...] [flags]",
		Run:     runAggregate,
		Flags: func(w io.Writer) {
			var p aggregateParams
			fs := aggregateFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nInput:\n")
			fmt.Fprintf(w, "  --input 可以是文件或目录（目录会被递归扫描）。\n")
			fmt.Fprintf(w, "  自动识别 %s 与 .gz 压缩的 JSONL。\n", ".jsonl")
			fmt.Fprintf(w, "  单个坏文件会被跳过并计数，不会导致整批失败。\n")

			fmt.Fprintf(w, "\nGrouping:\n")
			fmt.Fprintf(w, "  分组键是 (目标 IP:Port) × (国家/省/市) × (运营商/ASN)。\n")
			fmt.Fprintf(w, "  同一个 IP:Port 从不同地区看过去是不同的线路，\n")
			fmt.Fprintf(w, "  因此「1.1.1.1:443 的延迟是多少」没有唯一答案，\n")
			fmt.Fprintf(w, "  只有「从某地某运营商看是多少」。\n")

			fmt.Fprintf(w, "\nWhat it answers:\n")
			fmt.Fprintf(w, "  - 哪些目标在哪类网络下表现差（regions / groups）\n")
			fmt.Fprintf(w, "  - 同一个目标跨地区的表现差异有多大（latency_spread_ms）\n")
			fmt.Fprintf(w, "  - 不同运营商是否走了不同的 AS 路径（distinct_as_paths）\n")
			fmt.Fprintf(w, "\n它不做主观评分：只给计数、成功率和分位数。\n")
		},
	}
}

// aggregateFlagSet 构造 aggregate 的 FlagSet。
func aggregateFlagSet(p *aggregateParams) *flag.FlagSet {
	fs := newFlagSet("aggregate")

	fs.Var(&p.inputs, "input", "输入 JSONL 文件或目录（可重复）")
	fs.StringVar(&p.out, "out", "", "输出文件路径（默认写到 stdout）")
	fs.StringVar(&p.format, "format", aggregateFormatText, "输出格式：text / json / jsonl")

	fs.IntVar(&p.topGroups, "top-groups", aggregate.DefaultTopGroups,
		"明细分组输出条数（0 表示全部）")
	fs.IntVar(&p.topTargets, "top-targets", 0, "目标明细输出条数（0 表示全部）")

	fs.IntVar(&p.minSamples, "min-samples", 1,
		"分组进入结果所需的最少样本数（1 表示不过滤）")
	fs.IntVar(&p.regionMin, "region-min-samples", 1,
		"地区汇总所需的最少样本数")
	fs.IntVar(&p.maxGroups, "max-groups", aggregate.DefaultMaxGroups,
		"分组数上限（防止内存被畸形输入撑爆）")

	fs.BoolVar(&p.jsonOut, "json", false, "同 --format json（便捷写法）")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出结果，不输出进度")

	return fs
}

// runAggregate 实现 `cf-route-tester aggregate`。
func runAggregate(env *Env, args []string) error {
	var p aggregateParams
	fs := aggregateFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	format := strings.ToLower(strings.TrimSpace(p.format))
	if p.jsonOut {
		format = aggregateFormatJSON
	}
	switch format {
	case aggregateFormatText, aggregateFormatJSON, aggregateFormatJSONL:
	default:
		return usageError("unknown --format %q (want %s, %s or %s)",
			p.format, aggregateFormatText, aggregateFormatJSON, aggregateFormatJSONL)
	}

	if len(p.inputs) == 0 {
		return usageError("--input is required: give a JSONL file or a directory of batches")
	}
	if p.topGroups < 0 || p.topTargets < 0 {
		return usageError("--top-groups and --top-targets must not be negative")
	}
	if p.maxGroups <= 0 {
		return usageError("--max-groups must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// 1) 展开输入。
	sources, err := aggregate.DiscoverSources(p.inputs)
	if err != nil {
		return err
	}

	if !p.quiet {
		fmt.Fprintf(env.Stderr, "input:       %d file(s)\n", len(sources))
		for _, source := range sources {
			fmt.Fprintf(env.Stderr, "  %s (%s)\n", source.Path, source.Kind)
		}
	}

	// 2) 聚合。
	collector := aggregate.NewCollector(aggregate.Options{
		MinSamples: p.minSamples,
		MaxGroups:  p.maxGroups,
	})

	failures := collector.LoadSources(sources)

	if ctx.Err() != nil {
		return fmt.Errorf("aggregation interrupted after reading %d file(s)", collector.Load.Sources)
	}

	// 坏文件必须如实报出：静默跳过会让人以为整批数据都被算进去了。
	for _, failure := range failures {
		fmt.Fprintf(env.Stderr, "warning:     %v\n", failure)
	}

	report := collector.Report(aggregate.ReportOptions{
		TopGroups:        p.topGroups,
		TopTargets:       p.topTargets,
		RegionMinSamples: p.regionMin,
	})

	// 3) 输出。
	if err := writeAggregateOutput(env, report, format, p); err != nil {
		return err
	}

	// 4) 用法提示（只在 text 模式下，避免污染机器可读输出）。
	if format == aggregateFormatText && !p.quiet {
		fmt.Fprintf(env.Stderr, "\nrun with --format json for the full result, "+
			"or --top-groups 0 to list every group\n")
	}
	return nil
}

// writeAggregateOutput 按格式写出结果。
func writeAggregateOutput(env *Env, report *aggregate.Report, format string, p aggregateParams) error {
	writer := env.Stdout
	var closeFn func() error

	if path := strings.TrimSpace(p.out); path != "" && path != "-" {
		// 与 export 一致：不静默覆盖。
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("output file %q already exists; remove it or choose another path with --out", path)
		}
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create output file %q: %w", path, err)
		}
		writer = file
		closeFn = file.Close
	}
	if closeFn != nil {
		defer func() { _ = closeFn() }()
	}

	buffered := bufio.NewWriter(writer)

	var err error
	switch format {
	case aggregateFormatJSON:
		err = writeAggregateJSON(buffered, report)
	case aggregateFormatJSONL:
		err = writeAggregateJSONL(buffered, report)
	default:
		err = writeAggregateText(buffered, report)
	}
	if err != nil {
		return err
	}
	return buffered.Flush()
}

// writeAggregateJSON 输出完整 JSON 报告。
func writeAggregateJSON(w io.Writer, report *aggregate.Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return nil
}

// writeAggregateJSONL 输出 JSONL：每个分组一行，便于用 jq 之类处理。
//
// 与 JSON 的区别：JSON 是"一份报告"，JSONL 是"一堆记录"。
// 后者在分组数很大时更适合流式处理。
func writeAggregateJSONL(w io.Writer, report *aggregate.Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	// 先给一行元信息，然后是明细分组。
	meta := map[string]any{
		"schema_version":     report.SchemaVersion,
		"kind":               "aggregate_summary",
		"generated_at":       report.GeneratedAt,
		"input":              report.Input,
		"totals":             report.Totals,
		"collector_count":    report.CollectorCount,
		"multi_path_targets": report.MultiPathTargets,
		"notes":              report.Notes,
	}
	if err := encoder.Encode(meta); err != nil {
		return fmt.Errorf("encode summary line: %w", err)
	}

	for i := range report.Groups {
		group := report.Groups[i]
		line := struct {
			SchemaVersion int    `json:"schema_version"`
			Kind          string `json:"kind"`
			aggregate.GroupSummary
		}{
			SchemaVersion: report.SchemaVersion,
			Kind:          "aggregate_group",
			GroupSummary:  group,
		}
		if err := encoder.Encode(line); err != nil {
			return fmt.Errorf("encode group line: %w", err)
		}
	}
	return nil
}

// writeAggregateText 输出人类可读的报告。
func writeAggregateText(w io.Writer, report *aggregate.Report) error {
	fmt.Fprintf(w, "input:\n")
	fmt.Fprintf(w, "  files:        %d\n", report.Input.Sources)
	fmt.Fprintf(w, "  rows:         %d (%d measurements, %d traces)\n",
		report.Input.Rows, report.Input.Measurements, report.Input.Traces)
	if report.Input.BadLines > 0 {
		fmt.Fprintf(w, "  bad lines:    %d (skipped)\n", report.Input.BadLines)
	}
	if report.Input.FirstTimestamp != "" {
		fmt.Fprintf(w, "  time range:   %s .. %s (%.1fh)\n",
			report.Input.FirstTimestamp, report.Input.LastTimestamp, report.Input.SpanHours)
	}
	fmt.Fprintf(w, "  collectors:   %d\n", report.CollectorCount)

	fmt.Fprintf(w, "\ntotals:\n")
	fmt.Fprintf(w, "  targets:      %d\n", report.Totals.Targets)
	fmt.Fprintf(w, "  groups:       %d shown of %d\n", report.Totals.GroupsShown, report.Totals.Groups)
	fmt.Fprintf(w, "  probes:       %d total, %d ok (%.1f%%)\n",
		report.Totals.ProbeTotal, report.Totals.ProbeSuccess, report.Totals.ProbeSuccessRate*100)
	if report.Totals.TraceTotal > 0 {
		fmt.Fprintf(w, "  traces:       %d total, %d ok (%.1f%%)\n",
			report.Totals.TraceTotal, report.Totals.TraceSuccess, report.Totals.TraceSuccessRate*100)
	}
	fmt.Fprintf(w, "  multi-path:   %d target(s) with more than one AS path\n", report.MultiPathTargets)

	if report.Totals.Latency.Count > 0 {
		fmt.Fprintf(w, "\nlatency (ms, %d success samples, percentiles are approximate):\n",
			report.Totals.Latency.Count)
		fmt.Fprintf(w, "  min %.1f   p50 %.1f   p90 %.1f   p95 %.1f   p99 %.1f   max %.1f   avg %.1f\n",
			report.Totals.Latency.MinMS, report.Totals.Latency.P50MS, report.Totals.Latency.P90MS,
			report.Totals.Latency.P95MS, report.Totals.Latency.P99MS,
			report.Totals.Latency.MaxMS, report.Totals.Latency.AvgMS)
	}

	if len(report.Totals.Errors) > 0 {
		fmt.Fprintf(w, "\nfailures by type:\n")
		for _, item := range report.Totals.Errors {
			fmt.Fprintf(w, "  %-24s %d\n", item.Type+":", item.Count)
		}
	}

	if len(report.Regions) > 0 {
		fmt.Fprintf(w, "\nby region / ISP:\n")
		fmt.Fprintf(w, "  %-38s %7s %10s %8s %8s\n", "REGION", "TARGETS", "PROBES", "OK%", "P50 ms")
		for _, region := range report.Regions {
			p50 := 0.0
			if region.Latency.Count > 0 {
				p50 = region.Latency.P50MS
			}
			fmt.Fprintf(w, "  %-38s %7d %10d %7.1f%% %8.1f\n",
				truncateForDisplay(region.Label, 38), region.Targets, region.ProbeTotal,
				region.SuccessRate*100, p50)
		}
	}

	if len(report.Targets) > 0 {
		fmt.Fprintf(w, "\ntop targets by cross-region coverage:\n")
		fmt.Fprintf(w, "  %-30s %7s %8s %8s %11s\n", "TARGET", "REGIONS", "PROBES", "OK%", "SPREAD ms")
		for _, target := range report.Targets {
			fmt.Fprintf(w, "  %-30s %7d %8d %7.1f%% %11.1f\n",
				truncateForDisplay(target.TargetID, 30), target.Regions, target.ProbeTotal,
				target.SuccessRate*100, target.LatencySpreadMS)
		}
	}

	if len(report.Groups) > 0 {
		fmt.Fprintf(w, "\ngroups (target x region x ISP):\n")
		fmt.Fprintf(w, "  %-26s %-22s %7s %8s %8s\n", "TARGET", "REGION/ISP", "PROBES", "OK%", "P50 ms")
		for _, group := range report.Groups {
			label := regionLabelForDisplay(group.Country, group.Province, group.City, group.ISP, group.ASN)
			p50 := 0.0
			if group.Latency.Count > 0 {
				p50 = group.Latency.P50MS
			}
			fmt.Fprintf(w, "  %-26s %-22s %7d %7.1f%% %8.1f\n",
				truncateForDisplay(group.TargetID, 26), truncateForDisplay(label, 22),
				group.ProbeTotal, group.SuccessRate*100, p50)
		}
	}

	if len(report.Notes) > 0 {
		fmt.Fprintf(w, "\nnotes:\n")
		for _, note := range report.Notes {
			fmt.Fprintf(w, "  - %s\n", note)
		}
	}
	return nil
}

// regionLabelForDisplay 拼接地区展示标签。
func regionLabelForDisplay(country, province, city, isp, asn string) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{country, province, city, isp, asn} {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "(unknown)"
	}
	return strings.Join(parts, "/")
}

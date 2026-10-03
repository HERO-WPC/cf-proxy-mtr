package aggregate

import (
	"fmt"
	"sort"
	"time"
)

// AggregateSchemaVersion 是聚合输出的版本。
//
// 与公开数据的 schema_version 分开编号：聚合结果的结构由本项目
// 的分析需求决定，与原始数据的结构各自演进。
const AggregateSchemaVersion = 1

// ReportOptions 控制报告的生成与大小。
type ReportOptions struct {
	// TopGroups 是明细分组输出条数（<=0 时使用默认值）。
	TopGroups int

	// TopTargets 是目标明细输出条数（<=0 时表示全部）。
	TopTargets int

	// RegionMinSamples 是地区汇总的最少样本数。
	RegionMinSamples int

	// Now 允许注入当前时间（测试用）。
	Now func() time.Time
}

// DefaultTopGroups 是默认输出的明细分组数。
//
// 明细可能上百万条，全部输出既没有可读性也占满终端。
// 默认只给最有价值的部分。
const DefaultTopGroups = 50

// Report 生成聚合结果。
func (c *Collector) Report(opts ReportOptions) *Report {
	if opts.TopGroups <= 0 {
		opts.TopGroups = DefaultTopGroups
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	report := &Report{
		SchemaVersion: AggregateSchemaVersion,
		GeneratedAt:   opts.Now().UTC().Format(time.RFC3339),
	}

	// ---------------- 输入概况 ----------------
	report.Input = InputSummary{
		Sources:      c.Load.Sources,
		Lines:        c.Load.Lines,
		Rows:         c.Load.Rows,
		Measurements: c.Load.Measurements,
		Traces:       c.Load.Traces,
		BadLines:     c.Load.BadLines,
		UnknownKinds: c.Load.UnknownKinds,
	}
	if !c.Load.FirstTimestamp.IsZero() {
		report.Input.FirstTimestamp = c.Load.FirstTimestamp.Format(time.RFC3339)
	}
	if !c.Load.LastTimestamp.IsZero() {
		report.Input.LastTimestamp = c.Load.LastTimestamp.Format(time.RFC3339)
		span := c.Load.LastTimestamp.Sub(c.Load.FirstTimestamp)
		report.Input.SpanHours = round(span.Hours(), 2)
	}

	// ---------------- 全局合计 ----------------
	globalLatency := NewLatencyHistogram()
	globalErrors := ErrorCounts{}
	var probeTotal, probeSuccess, traceTotal, traceSuccess int64

	// 地区汇总：按 (国家/省/市/ISP/ASN) 合并多个目标的分组。
	regionStates := make(map[string]*regionState)

	// 目标汇总。
	targetSummaries := make([]TargetSummary, 0, len(c.targets))

	// 明细分组。
	groupSummaries := make([]GroupSummary, 0, len(c.groups))

	for _, state := range c.groups {
		probeTotal += state.probeTotal
		probeSuccess += state.probeSuccess
		traceTotal += state.traceTotal
		traceSuccess += state.traceSuccess
		globalLatency.Merge(state.latency)
		for kind, count := range state.errors {
			globalErrors[kind] += count
		}

		// 地区汇总。
		regionKey := regionLabel(state.key.Country, state.key.Province, state.key.City,
			state.key.ISP, state.key.ASN)
		region, ok := regionStates[regionKey]
		if !ok {
			region = &regionState{
				country:  state.key.Country,
				province: state.key.Province,
				city:     state.key.City,
				isp:      state.key.ISP,
				asn:      state.key.ASN,
				label:    regionKey,
				targets:  make(map[string]struct{}),
				latency:  NewLatencyHistogram(),
				errors:   ErrorCounts{},
			}
			regionStates[regionKey] = region
		}
		region.targets[state.key.TargetID] = struct{}{}
		region.probeTotal += state.probeTotal
		region.probeSuccess += state.probeSuccess
		region.latency.Merge(state.latency)
		for kind, count := range state.errors {
			region.errors[kind] += count
		}

		groupSummaries = append(groupSummaries, groupSummaryFrom(state))
	}

	// ---------------- 目标汇总 ----------------
	c.Skipped = 0
	for _, state := range c.targets {
		summary := c.targetSummaryFrom(state)

		// MinSamples 在**目标**层面过滤，而不是分组层面。
		//
		// 这个区别很关键：跨地区对比的常态是"每个地区只有一两个样本"
		// （一个节点一次扫描对同一目标只测一次）。若按分组过滤，
		// --min-samples 2 会把所有分组都藏掉，跨地区对比直接失效。
		//
		// 目标层面的语义是"这个目标被足够多次地测过没有"，
		// 更接近 min-samples 这个名字本来的意思。
		if summary.ProbeTotal+summary.TraceTotal < int64(c.opts.MinSamples) {
			c.Skipped++
			continue
		}
		targetSummaries = append(targetSummaries, summary)
	}
	// 目标按"地区覆盖数"降序：被越多节点测过越值得关注。
	sort.Slice(targetSummaries, func(i, j int) bool {
		if targetSummaries[i].Regions != targetSummaries[j].Regions {
			return targetSummaries[i].Regions > targetSummaries[j].Regions
		}
		return targetSummaries[i].TargetID < targetSummaries[j].TargetID
	})

	// ---------------- 明细排序与截断 ----------------
	// 明细先按目标分组、组内按成功率排序，保证输出稳定可比对。
	sortGroupsByTargetAndRate(groupSummaries)

	// totalGroups 是**分析过的**分组总数，不受展示层过滤影响。
	// 它与 GroupsShown 的差额就是被隐藏的部分，必须能被看出来。
	totalGroups := len(groupSummaries)

	if opts.TopGroups > 0 && len(groupSummaries) > opts.TopGroups {
		groupSummaries = groupSummaries[:opts.TopGroups]
	}

	if opts.TopTargets > 0 && len(targetSummaries) > opts.TopTargets {
		targetSummaries = targetSummaries[:opts.TopTargets]
	}

	// ---------------- 地区汇总排序 ----------------
	regions := make([]RegionSummary, 0, len(regionStates))
	for _, state := range regionStates {
		if int(state.probeTotal) < opts.RegionMinSamples {
			continue
		}
		regions = append(regions, state.summary())
	}
	// 按样本数降序：样本多的地区更可信，也更值得先看。
	sort.Slice(regions, func(i, j int) bool {
		if regions[i].ProbeTotal != regions[j].ProbeTotal {
			return regions[i].ProbeTotal > regions[j].ProbeTotal
		}
		return regions[i].Label < regions[j].Label
	})

	// ---------------- 多路径目标数 ----------------
	multiPath := 0
	for _, state := range c.targets {
		if len(state.asPaths) > 1 {
			multiPath++
		}
	}

	report.Totals = Totals{
		Targets:          len(c.targets),
		Groups:           totalGroups,
		GroupsShown:      len(groupSummaries),
		ProbeTotal:       probeTotal,
		ProbeSuccess:     probeSuccess,
		TraceTotal:       traceTotal,
		TraceSuccess:     traceSuccess,
		ProbeSuccessRate: round(successRate(probeSuccess, probeTotal), 4),
		TraceSuccessRate: round(successRate(traceSuccess, traceTotal), 4),
		Latency:          globalLatency.Snapshot(),
		Errors:           globalErrors.Sorted(),
	}
	report.Regions = regions
	report.Targets = targetSummaries
	report.Groups = groupSummaries
	report.CollectorCount = len(c.collectors)
	report.MultiPathTargets = multiPath

	report.Notes = c.notes(report, totalGroups)
	return report
}

// regionState 是地区层面的累积状态。
type regionState struct {
	country  string
	province string
	city     string
	isp      string
	asn      string
	label    string

	targets map[string]struct{}

	probeTotal   int64
	probeSuccess int64
	latency      *LatencyHistogram
	errors       ErrorCounts
}

// summary 生成地区汇总。
func (r *regionState) summary() RegionSummary {
	return RegionSummary{
		Country:      r.country,
		Province:     r.province,
		City:         r.city,
		ISP:          r.isp,
		ASN:          r.asn,
		Label:        r.label,
		Targets:      len(r.targets),
		ProbeTotal:   r.probeTotal,
		ProbeSuccess: r.probeSuccess,
		SuccessRate:  round(successRate(r.probeSuccess, r.probeTotal), 4),
		Latency:      r.latency.Snapshot(),
		Errors:       r.errors.Sorted(),
	}
}

// groupSummaryFrom 生成明细分组汇总。
func groupSummaryFrom(state *groupState) GroupSummary {
	versions := make([]string, 0, len(state.clientVersions))
	for version := range state.clientVersions {
		versions = append(versions, version)
	}
	sort.Strings(versions)

	out := GroupSummary{
		TargetID:         state.key.TargetID,
		Country:          state.key.Country,
		Province:         state.key.Province,
		City:             state.key.City,
		ISP:              state.key.ISP,
		ASN:              state.key.ASN,
		ProbeTotal:       state.probeTotal,
		ProbeSuccess:     state.probeSuccess,
		SuccessRate:      round(successRate(state.probeSuccess, state.probeTotal), 4),
		Latency:          state.latency.Snapshot(),
		TraceTotal:       state.traceTotal,
		TraceSuccess:     state.traceSuccess,
		AvgHopCount:      round(average(state.hopCounts), 1),
		AvgRespondedHops: round(average(state.respondedHops), 1),
		Errors:           state.errors.Sorted(),
		TraceErrors:      state.traceErrors.Sorted(),
		Sessions:         len(state.sessions),
		ClientVersions:   versions,
	}
	if !state.first.IsZero() {
		out.FirstSeen = state.first.Format(time.RFC3339)
	}
	if !state.last.IsZero() {
		out.LastSeen = state.last.Format(time.RFC3339)
	}
	return out
}

// targetSummaryFrom 生成目标汇总。
//
// 关键计算是 LatencySpreadMS：各分组延迟中位数的极差。
// 它是"同一个目标在不同地区表现差异有多大"的直接度量。
func (c *Collector) targetSummaryFrom(state *targetState) TargetSummary {
	out := TargetSummary{
		TargetID:     state.targetID,
		IP:           state.ip,
		Port:         state.port,
		Regions:      len(state.regions),
		ProbeTotal:   state.probeTotal,
		ProbeSuccess: state.probeSuccess,
		SuccessRate:  round(successRate(state.probeSuccess, state.probeTotal), 4),
		Latency:      state.latency.Snapshot(),
		TraceTotal:   state.traceTotal,
		TraceSuccess: state.traceSuccess,
	}

	if len(state.asPaths) > 1 {
		out.DistinctASPaths = len(state.asPaths)
	} else if len(state.asPaths) == 1 {
		out.DistinctASPaths = 1
	}

	// 收集该目标各分组的延迟中位数，算极差。
	medians := make([]float64, 0, len(state.regions))
	for key := range state.regions {
		if group, ok := c.groups[key]; ok {
			if median, ok := group.latency.Quantile(0.50); ok {
				medians = append(medians, median)
			}
		}
	}
	if len(medians) > 1 {
		low, high := medians[0], medians[0]
		for _, value := range medians[1:] {
			if value < low {
				low = value
			}
			if value > high {
				high = value
			}
		}
		out.LatencySpreadMS = round(high-low, 3)
	}

	return out
}

// notes 生成结果里需要提醒的事项。
//
// 存在的意义是**不让人误读数字**：一个成功率 100% 的结论，
// 如果只基于 1 个样本，就必须说清楚。
func (c *Collector) notes(report *Report, totalGroups int) []string {
	notes := make([]string, 0, 6)

	if c.Load.BadLines > 0 {
		notes = append(notes, fmtNote(
			"%d line(s) could not be parsed and were skipped; the numbers below are based on the remaining %d row(s)",
			c.Load.BadLines, c.Load.Rows))
	}
	if c.Load.UnknownKinds > 0 {
		notes = append(notes, fmtNote(
			"%d row(s) had an unrecognised kind and contributed nothing to the aggregates",
			c.Load.UnknownKinds))
	}
	if c.Dropped > 0 {
		notes = append(notes, fmtNote(
			"%d row(s) were dropped because the group limit (%d) was reached; increase --max-groups or narrow the input",
			c.Dropped, c.opts.MaxGroups))
	}
	if c.Skipped > 0 {
		notes = append(notes, fmtNote(
			"%d target(s) were hidden because they had fewer than %d sample(s) in total",
			c.Skipped, c.opts.MinSamples))
	}
	if report.CollectorCount <= 1 && report.Totals.ProbeTotal > 0 {
		notes = append(notes, "only one collector contributed data; "+
			"cross-region comparisons are not meaningful yet")
	}
	if report.Totals.TraceTotal == 0 && report.Totals.ProbeTotal > 0 {
		notes = append(notes, "no route traces in the input; "+
			"run 'scan --trace' to collect path data")
	}
	if totalGroups > len(report.Groups) {
		notes = append(notes, fmtNote(
			"%d of %d group(s) are shown; use --min-samples 1 and --top-groups 0 to see them all",
			len(report.Groups), totalGroups))
	}

	// 时间跨度：短到不可分辨时更值得提醒，因为那意味着时间维度没有信息。
	switch {
	case c.Load.Rows > 0 && report.Input.SpanHours > 0 && report.Input.SpanHours < 1:
		notes = append(notes, "all data was collected within one hour; "+
			"this is a snapshot, not a trend")
	case c.Load.Rows > 1 && report.Input.SpanHours == 0:
		notes = append(notes, "all rows carry the same timestamp (or none at all); "+
			"the aggregation has no time dimension")
	}

	return notes
}

// fmtNote 构造一条提醒。
func fmtNote(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

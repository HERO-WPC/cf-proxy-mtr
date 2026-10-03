package aggregate

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 构造测试行
// ---------------------------------------------------------------------------

// rowOptions 描述要构造的行。
type rowOptions struct {
	kind      string
	targetID  string
	ip        string
	port      int
	country   string
	province  string
	city      string
	isp       string
	asn       string
	collector string
	session   string
	version   string
	timestamp string

	success      bool
	latency      float64
	errorType    string
	traceSuccess bool
	hopCount     int
	responded    int
	hops         []HopView
}

// makeRow 构造一行测试数据。
func makeRow(o rowOptions) Row {
	row := Row{
		SchemaVersion: 1,
		Kind:          o.kind,
		ClientVersion: o.version,
		TargetID:      o.targetID,
		IP:            o.ip,
		Port:          o.port,
		CollectorID:   o.collector,
		SessionID:     o.session,
		TimestampUTC:  o.timestamp,
	}
	if row.TimestampUTC == "" {
		row.TimestampUTC = "2026-10-03T10:00:00Z"
	}

	row.Collector = &CollectorRef{
		Country: o.country, Province: o.province, City: o.city,
		ISP: o.isp, ASN: o.asn,
	}

	switch o.kind {
	case "measurement":
		row.Measurement = &MeasurementRef{
			Success: o.success, LatencyMS: o.latency, ErrorType: o.errorType,
		}
	case "trace":
		row.Trace = &TraceRef{
			Success: o.traceSuccess, Engine: "nexttrace", EngineVersion: "1.7.3",
			Mode: "tcp", DurationMS: 1200, HopCount: o.hopCount, RespondedHops: o.responded,
			LocalFiltered: true, ErrorType: o.errorType, Hops: o.hops,
		}
	}
	return row
}

// ---------------------------------------------------------------------------
// 分组语义
// ---------------------------------------------------------------------------

// TestGroupKeyIncludesRegionAndISP 是需求第 3 条的核心测试。
//
// 同一个 IP:Port 从不同地区/运营商看过去是不同的线路，
// 因此分组键必须包含地区与运营商。
func TestGroupKeyIncludesRegionAndISP(t *testing.T) {
	c := NewCollector(Options{})

	// 同一个目标，三个不同运营商。
	for _, isp := range []string{"China Mobile", "China Telecom", "China Unicom"} {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			country: "CN", province: "Zhejiang", city: "Hangzhou", isp: isp, asn: "AS1",
			collector: "c1", success: true, latency: 100,
		}))
	}

	report := c.Report(ReportOptions{TopGroups: 0})
	if report.Totals.Groups != 3 {
		t.Fatalf("groups = %d, want 3 (one per ISP)", report.Totals.Groups)
	}
	if report.Totals.Targets != 1 {
		t.Errorf("targets = %d, want 1", report.Totals.Targets)
	}

	// 目标的 Regions 应当反映"被 3 个不同分组测过"。
	if len(report.Targets) != 1 {
		t.Fatalf("target summaries = %d, want 1", len(report.Targets))
	}
	if report.Targets[0].Regions != 3 {
		t.Errorf("Regions = %d, want 3", report.Targets[0].Regions)
	}

	// 地区汇总也应当是 3 条。
	if len(report.Regions) != 3 {
		t.Errorf("regions = %d, want 3", len(report.Regions))
	}
}

// TestSameRegionMergesAcrossTargets 验证地区汇总把多个目标合并。
func TestSameRegionMergesAcrossTargets(t *testing.T) {
	c := NewCollector(Options{})

	for i, ip := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"} {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: ip + ":443", ip: ip, port: 443,
			country: "CN", province: "Zhejiang", city: "Hangzhou", isp: "China Mobile", asn: "AS9808",
			collector: "c1", success: true, latency: float64(100 + i*10),
		}))
	}

	report := c.Report(ReportOptions{})
	if len(report.Regions) != 1 {
		t.Fatalf("regions = %d, want 1 (same region/ISP)", len(report.Regions))
	}
	region := report.Regions[0]
	if region.Targets != 3 {
		t.Errorf("Targets = %d, want 3", region.Targets)
	}
	if region.ProbeTotal != 3 || region.ProbeSuccess != 3 {
		t.Errorf("probe counts = %d/%d, want 3/3", region.ProbeTotal, region.ProbeSuccess)
	}
	if region.Label != "CN/Zhejiang/Hangzhou/China Mobile/AS9808" {
		t.Errorf("Label = %q", region.Label)
	}
}

// ---------------------------------------------------------------------------
// 成功率
// ---------------------------------------------------------------------------

func TestSuccessRateAndErrorCounts(t *testing.T) {
	c := NewCollector(Options{})

	// 4 成功，3 失败（2 超时 + 1 拒绝）。
	for i := 0; i < 4; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			collector: "c1", success: true, latency: 50,
		}))
	}
	for _, kind := range []string{"timeout", "timeout", "connection_refused"} {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			collector: "c1", success: false, errorType: kind,
		}))
	}

	report := c.Report(ReportOptions{})
	if report.Totals.ProbeTotal != 7 || report.Totals.ProbeSuccess != 4 {
		t.Fatalf("totals = %d/%d, want 4/7", report.Totals.ProbeSuccess, report.Totals.ProbeTotal)
	}
	if report.Totals.ProbeSuccessRate != 0.5714 {
		t.Errorf("ProbeSuccessRate = %v, want 0.5714", report.Totals.ProbeSuccessRate)
	}

	// 失败分类必须保留（超时与拒绝是不同的线路现象）。
	if len(report.Totals.Errors) != 2 {
		t.Fatalf("error kinds = %d, want 2", len(report.Totals.Errors))
	}
	// 按数量降序：timeout(2) 在前。
	if report.Totals.Errors[0].Type != "timeout" || report.Totals.Errors[0].Count != 2 {
		t.Errorf("first error = %+v, want timeout x2", report.Totals.Errors[0])
	}
	if report.Totals.Errors[1].Type != "connection_refused" {
		t.Errorf("second error = %+v, want connection_refused", report.Totals.Errors[1])
	}
}

// TestFailedMeasurementsDoNotEnterLatency 验证失败样本不污染延迟统计。
//
// 失败样本的 latency_ms 是"失败前等了多久"，把它算进延迟分布
// 会让延迟看起来比实际差得多（超时样本动辄几千毫秒）。
func TestFailedMeasurementsDoNotEnterLatency(t *testing.T) {
	c := NewCollector(Options{})

	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", success: true, latency: 20,
	}))
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", success: false, errorType: "timeout", latency: 3000,
	}))

	report := c.Report(ReportOptions{})
	if report.Totals.Latency.Count != 1 {
		t.Fatalf("latency samples = %d, want 1 (only the successful one)", report.Totals.Latency.Count)
	}
	if report.Totals.Latency.MaxMS > 100 {
		t.Errorf("MaxMS = %v, want the 3000ms failure excluded", report.Totals.Latency.MaxMS)
	}
}

// ---------------------------------------------------------------------------
// 直方图
// ---------------------------------------------------------------------------

func TestLatencyHistogram(t *testing.T) {
	h := NewLatencyHistogram()

	// 空直方图不能 panic，也不能给出假数字。
	if h.Count != 0 {
		t.Errorf("Count = %d, want 0", h.Count)
	}
	if _, ok := h.Quantile(0.5); ok {
		t.Error("Quantile on an empty histogram reported a value")
	}
	if h.Mean() != 0 {
		t.Errorf("Mean() = %v, want 0", h.Mean())
	}
	snapshot := h.Snapshot()
	if snapshot.Count != 0 || snapshot.P50MS != 0 {
		t.Errorf("snapshot = %+v, want empty", snapshot)
	}

	// 精确值：极值与平均值不受分桶影响。
	for i := 1; i <= 100; i++ {
		h.Observe(float64(i))
	}
	if h.Count != 100 {
		t.Fatalf("Count = %d, want 100", h.Count)
	}
	if h.Min != 1 || h.Max != 100 {
		t.Errorf("min/max = %v/%v, want 1/100 (exact)", h.Min, h.Max)
	}
	if mean := h.Mean(); mean != 50.5 {
		t.Errorf("Mean() = %v, want exactly 50.5", mean)
	}

	// 分位数是近似值，误差应在声明范围内（约 3.5%）。
	p50, ok := h.Quantile(0.50)
	if !ok {
		t.Fatal("Quantile(0.5) failed")
	}
	if relativeError(p50, 50) > 0.05 {
		t.Errorf("p50 = %v, want within 5%% of 50", p50)
	}
	p90, _ := h.Quantile(0.90)
	if relativeError(p90, 90) > 0.05 {
		t.Errorf("p90 = %v, want within 5%% of 90", p90)
	}

	// Snapshot 必须标记为近似。
	if !h.Snapshot().PercentilesApprox {
		t.Error("PercentilesApprox = false; callers must know these are approximations")
	}
}

// relativeError 计算相对误差。
func relativeError(got, want float64) float64 {
	if want == 0 {
		return 0
	}
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff / want
}

func TestLatencyHistogramRejectsInvalidSamples(t *testing.T) {
	h := NewLatencyHistogram()

	// 负值、NaN、Inf 都不是有效延迟：收进来会把统计带偏。
	h.Observe(-1)
	h.Observe(math.NaN())
	h.Observe(math.Inf(1))

	if h.Count != 0 {
		t.Errorf("Count = %d, want 0 for invalid samples", h.Count)
	}

	// **0 也必须被丢掉**。
	//
	// 0 在真实数据里表示"没有测到"（失败的探测、超时跳），
	// 而不是"0 毫秒"。收进来会让分位数被一堆 0 拉垮——
	// 实测表现是"只有一个 1.31ms 样本的跳，p50 却报成 0.00"。
	h.Observe(0)
	if h.Count != 0 {
		t.Errorf("Count = %d, want 0 (zero means 'not measured', not '0 ms')", h.Count)
	}

	// 一个真实样本必须完整反映出来。
	h.Observe(1.31)
	if h.Count != 1 {
		t.Fatalf("Count = %d, want 1", h.Count)
	}
	median, ok := h.Quantile(0.50)
	if !ok {
		t.Fatal("Quantile failed")
	}
	if median <= 0 || median > 1.31 {
		t.Errorf("p50 = %v, want a positive value <= the only sample (1.31)", median)
	}
}

// TestQuantileStaysWithinExactExtremes 是一条反自相矛盾的回归测试。
//
// 分桶的代表值是几何中点，可能略微超出桶内真实样本的范围，
// 于是出现 "p90 = 305.6 而 max = 300.1" 这种输出（实测踩到过）。
// 分位数必须落在 [min, max] 之内。
func TestQuantileStaysWithinExactExtremes(t *testing.T) {
	cases := []struct {
		name    string
		samples []float64
	}{
		{"single sample", []float64{300.1}},
		{"two close samples", []float64{300.1, 305.6}},
		{"wide range", []float64{1.2, 3.4, 50, 300.1, 1300}},
		{"many identical", []float64{42, 42, 42, 42, 42}},
		{"tiny values", []float64{0.11, 0.12, 0.13}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewLatencyHistogram()
			for _, sample := range tc.samples {
				h.Observe(sample)
			}

			for _, p := range []float64{0.01, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99} {
				value, ok := h.Quantile(p)
				if !ok {
					t.Fatalf("Quantile(%v) failed", p)
				}
				if value < h.Min {
					t.Errorf("p%v = %v, below the exact min %v", p*100, value, h.Min)
				}
				if value > h.Max {
					t.Errorf("p%v = %v, above the exact max %v (contradictory output)",
						p*100, value, h.Max)
				}
			}
		})
	}
}

// TestSnapshotIsSelfConsistent 验证快照里的数字彼此不矛盾。
func TestSnapshotIsSelfConsistent(t *testing.T) {
	h := NewLatencyHistogram()
	for _, sample := range []float64{300.1, 305.6, 310.2} {
		h.Observe(sample)
	}

	snapshot := h.Snapshot()
	if snapshot.MinMS > snapshot.P50MS {
		t.Errorf("min %v > p50 %v", snapshot.MinMS, snapshot.P50MS)
	}
	if snapshot.P50MS > snapshot.P90MS {
		t.Errorf("p50 %v > p90 %v", snapshot.P50MS, snapshot.P90MS)
	}
	if snapshot.P90MS > snapshot.P95MS {
		t.Errorf("p90 %v > p95 %v", snapshot.P90MS, snapshot.P95MS)
	}
	if snapshot.P95MS > snapshot.P99MS {
		t.Errorf("p95 %v > p99 %v", snapshot.P95MS, snapshot.P99MS)
	}
	if snapshot.P99MS > snapshot.MaxMS {
		t.Errorf("p99 %v > max %v", snapshot.P99MS, snapshot.MaxMS)
	}
}

func TestLatencyHistogramMerge(t *testing.T) {
	a := NewLatencyHistogram()
	b := NewLatencyHistogram()

	for i := 1; i <= 10; i++ {
		a.Observe(float64(i))
	}
	for i := 100; i <= 110; i++ {
		b.Observe(float64(i))
	}

	a.Merge(b)

	if a.Count != 21 {
		t.Errorf("Count = %d, want 21", a.Count)
	}
	// 极值必须精确合并。
	if a.Min != 1 {
		t.Errorf("Min = %v, want 1", a.Min)
	}
	if a.Max != 110 {
		t.Errorf("Max = %v, want 110", a.Max)
	}

	// 合并到空直方图也要正确。
	empty := NewLatencyHistogram()
	empty.Merge(b)
	if empty.Count != 11 || empty.Min != 100 || empty.Max != 110 {
		t.Errorf("merge into empty = %+v", empty.Snapshot())
	}

	// 合并 nil / 空 都不能 panic。
	empty.Merge(nil)
	empty.Merge(NewLatencyHistogram())
	if empty.Count != 11 {
		t.Errorf("Count changed after merging nothing: %d", empty.Count)
	}
}

// ---------------------------------------------------------------------------
// 路径签名
// ---------------------------------------------------------------------------

// TestDistinctASPaths 验证"不同运营商是否走不同出口"这个问题能被回答。
func TestDistinctASPaths(t *testing.T) {
	c := NewCollector(Options{})

	// 同一个目标，两个运营商，两条不同的 AS 路径。
	c.Add(makeRow(rowOptions{
		kind: "trace", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		isp: "China Mobile", collector: "c1", traceSuccess: true, hopCount: 3,
		hops: []HopView{
			{TTL: 1, ASN: "AS9808"}, {TTL: 2, ASN: "AS58453"}, {TTL: 3, ASN: "AS13335"},
		},
	}))
	c.Add(makeRow(rowOptions{
		kind: "trace", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		isp: "China Telecom", collector: "c1", traceSuccess: true, hopCount: 3,
		hops: []HopView{
			{TTL: 1, ASN: "AS4134"}, {TTL: 2, ASN: "AS13335"},
		},
	}))

	report := c.Report(ReportOptions{})
	if report.MultiPathTargets != 1 {
		t.Errorf("MultiPathTargets = %d, want 1", report.MultiPathTargets)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(report.Targets))
	}
	if report.Targets[0].DistinctASPaths != 2 {
		t.Errorf("DistinctASPaths = %d, want 2", report.Targets[0].DistinctASPaths)
	}
}

// TestSameASPathIsNotMultiPath 验证相同路径不会被算成多条。
func TestSameASPathIsNotMultiPath(t *testing.T) {
	c := NewCollector(Options{})

	for _, isp := range []string{"China Mobile", "China Unicom"} {
		c.Add(makeRow(rowOptions{
			kind: "trace", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: isp, collector: "c1", traceSuccess: true, hopCount: 2,
			hops: []HopView{{TTL: 1, ASN: "AS9808"}, {TTL: 2, ASN: "AS13335"}},
		}))
	}

	report := c.Report(ReportOptions{})
	if report.MultiPathTargets != 0 {
		t.Errorf("MultiPathTargets = %d, want 0 (the AS path is identical)", report.MultiPathTargets)
	}
	if report.Targets[0].DistinctASPaths != 1 {
		t.Errorf("DistinctASPaths = %d, want 1", report.Targets[0].DistinctASPaths)
	}
}

// TestAsPathSignatureIgnoresHopsWithoutASN 验证签名只用有 ASN 的跳。
func TestAsPathSignatureIgnoresHopsWithoutASN(t *testing.T) {
	// 内网跳被导出层替换成占位符、且没有 ASN，因此不该出现在签名里。
	hops := []HopView{
		{TTL: 1, IP: "private-v4"},
		{TTL: 2, ASN: "AS9808"},
		{TTL: 3, IP: "1.1.1.1"},
		{TTL: 4, ASN: "AS13335"},
	}
	if got := asPathSignature(hops); got != "AS9808-AS13335" {
		t.Errorf("signature = %q, want AS9808-AS13335", got)
	}

	// 完全没有 ASN 时返回空（用于判断"这条路径没有可用信息"）。
	if got := asPathSignature([]HopView{{TTL: 1, IP: "1.1.1.1"}}); got != "" {
		t.Errorf("signature = %q, want empty", got)
	}
	if got := asPathSignature(nil); got != "" {
		t.Errorf("signature = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// 输入读取
// ---------------------------------------------------------------------------

// TestLoadSkipsBadLinesAndCounts 验证坏行被计数而不是静默跳过。
func TestLoadSkipsBadLinesAndCounts(t *testing.T) {
	body := `{"kind":"measurement","target_id":"1.1.1.1:443"}
not json at all
{"kind":"measurement","target_id":"8.8.8.8:443"}

{"kind":"something_else","target_id":"9.9.9.9:443"}
`

	var stats LoadStats
	var rows int
	err := Load(strings.NewReader(body), &stats, func(Row) error {
		rows++
		return nil
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if stats.Lines != 4 {
		t.Errorf("Lines = %d, want 4 (blank lines are not counted)", stats.Lines)
	}
	if stats.Rows != 3 {
		t.Errorf("Rows = %d, want 3", stats.Rows)
	}
	if stats.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1", stats.BadLines)
	}
	if stats.UnknownKinds != 1 {
		t.Errorf("UnknownKinds = %d, want 1", stats.UnknownKinds)
	}
	if stats.Measurements != 2 {
		t.Errorf("Measurements = %d, want 2", stats.Measurements)
	}
	if rows != 3 {
		t.Errorf("callback ran %d times, want 3", rows)
	}
}

func TestLoadTracksTimeRange(t *testing.T) {
	body := `{"kind":"measurement","timestamp_utc":"2026-10-03T10:00:00Z"}
{"kind":"measurement","timestamp_utc":"2026-10-03T08:00:00Z"}
{"kind":"measurement","timestamp_utc":"2026-10-03T12:00:00Z"}
`
	var stats LoadStats
	if err := Load(strings.NewReader(body), &stats, func(Row) error { return nil }); err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	if !stats.FirstTimestamp.Equal(want) {
		t.Errorf("FirstTimestamp = %s, want %s", stats.FirstTimestamp, want)
	}
	wantLast := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if !stats.LastTimestamp.Equal(wantLast) {
		t.Errorf("LastTimestamp = %s, want %s", stats.LastTimestamp, wantLast)
	}
}

func TestLoadHandlesVeryLongLines(t *testing.T) {
	// 一条含 30 跳的跟踪行可能很长；默认 64 KiB 的扫描上限会误报
	// "token too long"，因此实现里把上限提到了 8 MiB。
	var builder strings.Builder
	builder.WriteString(`{"kind":"trace","trace":{"hops":[`)
	for i := 0; i < 800; i++ {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"ttl":`)
		builder.WriteString(itoa(i + 1))
		builder.WriteString(`,"asn":"AS9808","as_organization":"China Mobile Communications Corporation Zhejiang Branch"}`)
	}
	builder.WriteString(`]}}`)

	if builder.Len() < 64*1024 {
		t.Fatalf("test bug: line is only %d bytes, not long enough", builder.Len())
	}

	var stats LoadStats
	if err := Load(strings.NewReader(builder.String()), &stats, func(Row) error { return nil }); err != nil {
		t.Fatalf("Load on a long line: %v", err)
	}
	if stats.BadLines != 0 {
		t.Errorf("BadLines = %d, want 0", stats.BadLines)
	}
}

// itoa 是本文件用的整数格式化。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// ---------------------------------------------------------------------------
// 文件来源
// ---------------------------------------------------------------------------

func TestDiscoverSourcesFromDirectory(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "a.jsonl"), "{}\n")
	writeFile(t, filepath.Join(dir, "b.jsonl.gz"), "")
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignore me")
	writeFile(t, filepath.Join(dir, "sub", "c.jsonl"), "{}\n")

	sources, err := DiscoverSources([]string{dir})
	if err != nil {
		t.Fatalf("DiscoverSources: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("sources = %d, want 3 (txt files are ignored)", len(sources))
	}

	// 顺序必须稳定，便于比对与复现。
	paths := []string{sources[0].Path, sources[1].Path, sources[2].Path}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] > paths[i] {
			t.Errorf("sources are not sorted: %v", paths)
			break
		}
	}

	// gz 文件必须被识别成 gzip。
	kinds := map[string]SourceKind{}
	for _, source := range sources {
		kinds[filepath.Base(source.Path)] = source.Kind
	}
	if kinds["b.jsonl.gz"] != SourceGzip {
		t.Errorf("b.jsonl.gz kind = %v, want gzip", kinds["b.jsonl.gz"])
	}
	if kinds["a.jsonl"] != SourcePlain {
		t.Errorf("a.jsonl kind = %v, want plain", kinds["a.jsonl"])
	}
}

func TestDiscoverSourcesErrors(t *testing.T) {
	if _, err := DiscoverSources(nil); err == nil {
		t.Error("DiscoverSources accepted no inputs")
	}
	if _, err := DiscoverSources([]string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("DiscoverSources accepted a missing path")
	}

	empty := t.TempDir()
	if _, err := DiscoverSources([]string{empty}); err == nil {
		t.Error("DiscoverSources accepted a directory with no JSONL files")
	}
}

// TestLoadSourcesReadsGzip 验证 gzip 输入可被正确读取。
func TestLoadSourcesReadsGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "batch.jsonl.gz")

	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	rows := []Row{
		makeRow(rowOptions{kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1",
			port: 443, collector: "c1", success: true, latency: 42}),
		makeRow(rowOptions{kind: "measurement", targetID: "8.8.8.8:443", ip: "8.8.8.8",
			port: 443, collector: "c1", success: true, latency: 43}),
	}
	for _, row := range rows {
		blob, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := compressor.Write(append(blob, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := DiscoverSources([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Kind != SourceGzip {
		t.Fatalf("sources = %+v", sources)
	}

	c := NewCollector(Options{})
	failures := c.LoadSources(sources)
	if len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	if c.Load.Rows != 2 {
		t.Errorf("Rows = %d, want 2", c.Load.Rows)
	}

	report := c.Report(ReportOptions{})
	if report.Totals.ProbeTotal != 2 {
		t.Errorf("ProbeTotal = %d, want 2", report.Totals.ProbeTotal)
	}
}

// TestLoadSourcesContinuesAfterBadFile 验证单个坏文件不终止整批。
//
// 众测数据里混着坏文件是常态，一个坏文件不该让整批数据作废。
func TestLoadSourcesContinuesAfterBadFile(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.jsonl")
	writeFile(t, good, `{"kind":"measurement","target_id":"1.1.1.1:443","ip":"1.1.1.1","port":443,"collector_id":"c1","timestamp_utc":"2026-10-03T10:00:00Z","measurement":{"success":true,"latency_ms":10}}`+"\n")

	// 一个"gz"文件，内容其实不是 gzip：打开就会失败。
	broken := filepath.Join(dir, "broken.jsonl.gz")
	writeFile(t, broken, "this is not gzip")

	sources := []Source{
		{Path: good, Kind: SourcePlain},
		{Path: broken, Kind: SourceGzip},
	}

	c := NewCollector(Options{})
	failures := c.LoadSources(sources)

	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1 (the broken file must be reported)", len(failures))
	}
	if !strings.Contains(failures[0].Path, "broken.jsonl.gz") {
		t.Errorf("failure path = %q", failures[0].Path)
	}
	// 好的文件必须被读进去。
	if c.Load.Rows != 1 {
		t.Errorf("Rows = %d, want 1 (the good file must still be aggregated)", c.Load.Rows)
	}
	if c.Report(ReportOptions{}).Totals.ProbeTotal != 1 {
		t.Error("the good file's data did not reach the aggregates")
	}
}

// ---------------------------------------------------------------------------
// 报告与边界
// ---------------------------------------------------------------------------

func TestReportNotesWarnAboutWeakData(t *testing.T) {
	c := NewCollector(Options{})

	// 一行有内容的数据。
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", success: true, latency: 10, timestamp: "2026-10-03T10:00:00Z",
	}))

	// 输入侧统计由 Load 填写（Add 刻意不碰它）。
	c.Load.Rows = 3
	c.Load.BadLines = 2
	c.Load.FirstTimestamp = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	c.Load.LastTimestamp = time.Date(2026, 10, 3, 10, 30, 0, 0, time.UTC)

	report := c.Report(ReportOptions{})
	joined := strings.Join(report.Notes, " | ")

	if !strings.Contains(joined, "could not be parsed") {
		t.Errorf("notes do not mention bad lines: %v", report.Notes)
	}
	if !strings.Contains(joined, "only one collector") {
		t.Errorf("notes do not warn about single-collector data: %v", report.Notes)
	}
	if !strings.Contains(joined, "within one hour") {
		t.Errorf("notes do not warn about the short time span: %v", report.Notes)
	}
}

// TestReportNotesWhenAllRowsShareATimestamp 覆盖"完全没有时间维度"。
//
// 与"都在一小时内"不同：同一时间戳（或都没有时间戳）意味着
// 时间维度根本不存在，这更值得提醒。
func TestReportNotesWhenAllRowsShareATimestamp(t *testing.T) {
	c := NewCollector(Options{})
	for i := 0; i < 2; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			collector: "c1", success: true, latency: 10,
		}))
	}
	c.Load.Rows = 2
	// FirstTimestamp / LastTimestamp 保持零值 -> SpanHours 为 0。

	report := c.Report(ReportOptions{})
	joined := strings.Join(report.Notes, " | ")
	if !strings.Contains(joined, "no time dimension") {
		t.Errorf("notes do not mention the missing time dimension: %v", report.Notes)
	}
}

func TestReportNotesMentionMissingTraces(t *testing.T) {
	c := NewCollector(Options{})
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", success: true, latency: 10,
	}))

	report := c.Report(ReportOptions{})
	if !strings.Contains(strings.Join(report.Notes, " "), "no route traces") {
		t.Errorf("notes do not mention missing traces: %v", report.Notes)
	}
}

func TestRegionMinSamplesFiltersNoisyRegions(t *testing.T) {
	c := NewCollector(Options{})

	// 一个有 5 个样本的地区，一个只有 1 个。
	for i := 0; i < 5; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: "Big", collector: "c1", success: true, latency: 10,
		}))
	}
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		isp: "Small", collector: "c1", success: true, latency: 10,
	}))

	all := c.Report(ReportOptions{RegionMinSamples: 1})
	if len(all.Regions) != 2 {
		t.Fatalf("regions = %d, want 2", len(all.Regions))
	}
	// 样本多的排在前面。
	if all.Regions[0].ISP != "Big" {
		t.Errorf("first region = %q, want Big (more samples)", all.Regions[0].ISP)
	}

	filtered := c.Report(ReportOptions{RegionMinSamples: 3})
	if len(filtered.Regions) != 1 {
		t.Fatalf("regions = %d, want 1 after filtering", len(filtered.Regions))
	}
	if filtered.Regions[0].ISP != "Big" {
		t.Errorf("kept region = %q, want Big", filtered.Regions[0].ISP)
	}
}

func TestMaxGroupsIsEnforcedAndCounted(t *testing.T) {
	c := NewCollector(Options{MaxGroups: 2})

	for i := 0; i < 5; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: "ISP" + itoa(i), collector: "c1", success: true, latency: 10,
		}))
	}

	report := c.Report(ReportOptions{})
	if report.Totals.Groups > 2 {
		t.Errorf("groups = %d, want at most 2", report.Totals.Groups)
	}
	// 被丢掉的行必须被计数并报出，而不是静默消失。
	if c.Dropped == 0 {
		t.Error("Dropped = 0, but rows were discarded")
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "group limit") {
		t.Errorf("notes do not mention the group limit: %v", report.Notes)
	}
}

// TestMinSamplesFiltersThinGroups 验证样本太少的分组真的被过滤。
//
// 1 条样本的成功率要么 0% 要么 100%，没有统计意义。
func TestMinSamplesFiltersThinGroups(t *testing.T) {
	c := NewCollector(Options{MinSamples: 3})

	// 一个 4 样本的分组，一个 1 样本的分组。
	for i := 0; i < 4; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: "Fat", collector: "c1", success: true, latency: 10,
		}))
	}
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "8.8.8.8:443", ip: "8.8.8.8", port: 443,
		isp: "Thin", collector: "c1", success: true, latency: 10,
	}))

	report := c.Report(ReportOptions{TopGroups: 0})

	// 目标是层面过滤：4 样本的目标保留，1 样本的目标被隐藏。
	if len(report.Targets) != 1 {
		t.Fatalf("targets = %d, want 1 (the thin target must be filtered)", len(report.Targets))
	}
	if report.Targets[0].TargetID != "1.1.1.1:443" {
		t.Errorf("kept target = %q, want the 4-sample one", report.Targets[0].TargetID)
	}
	if c.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", c.Skipped)
	}
	// 明细分组不受影响：它是原始观察记录，不是"结论"。
	if len(report.Groups) != 2 {
		t.Errorf("groups = %d, want 2 (detail rows are not filtered by MinSamples)", len(report.Groups))
	}

	// 被过滤的事实必须出现在提醒里，不能静默隐藏。
	joined := strings.Join(report.Notes, " | ")
	if !strings.Contains(joined, "fewer than 3 sample") {
		t.Errorf("notes do not mention the filter: %v", report.Notes)
	}

	// MinSamples=1（默认）时两条都保留。
	loose := NewCollector(Options{MinSamples: 1})
	loose.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "8.8.8.8:443", ip: "8.8.8.8", port: 443,
		isp: "Thin", collector: "c1", success: true, latency: 10,
	}))
	if got := len(loose.Report(ReportOptions{TopGroups: 0}).Targets); got != 1 {
		t.Errorf("targets = %d with MinSamples=1, want 1", got)
	}
}

// TestMinSamplesCountsTracesToo 验证"样本"包含跟踪结果。
func TestMinSamplesCountsTracesToo(t *testing.T) {
	c := NewCollector(Options{MinSamples: 2})

	// 1 条测量 + 1 条跟踪 = 2 个样本，应当保留。
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", success: true, latency: 10,
	}))
	c.Add(makeRow(rowOptions{
		kind: "trace", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c1", traceSuccess: true, hopCount: 2,
		hops: []HopView{{TTL: 1, ASN: "AS1"}},
	}))

	report := c.Report(ReportOptions{TopGroups: 0})
	if len(report.Targets) != 1 {
		t.Fatalf("targets = %d, want 1 (probe + trace = 2 samples)", len(report.Targets))
	}
}

func TestReportIsIdempotent(t *testing.T) {
	c := NewCollector(Options{})
	for i := 0; i < 3; i++ {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: "ISP" + itoa(i), collector: "c1", success: true, latency: 10,
		}))
	}

	// TopGroups 的截断不能写回状态。
	first := c.Report(ReportOptions{TopGroups: 1})
	second := c.Report(ReportOptions{TopGroups: 1})
	full := c.Report(ReportOptions{TopGroups: 0})

	firstJSON, err := json.Marshal(first.Totals)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second.Totals)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Error("two identical Report calls produced different totals")
	}
	if full.Totals.GroupsShown != 3 {
		t.Errorf("GroupsShown = %d, want 3 (truncation must not persist)", full.Totals.GroupsShown)
	}

	// MinSamples 是构造选项（不是 ReportOptions），因此它对同一个
	// Collector 的每次 Report 都是一致的；这里验证过滤不会破坏状态。
	strict := NewCollector(Options{MinSamples: 10})
	for i := 0; i < 3; i++ {
		strict.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			isp: "ISP" + itoa(i), collector: "c1", success: true, latency: 10,
		}))
	}
	empty := strict.Report(ReportOptions{TopGroups: 0})
	// 每个目标只有 1 个样本，MinSamples=10 应把 3 个目标全部隐藏。
	if len(empty.Targets) != 0 {
		t.Errorf("targets = %d with MinSamples=10, want 0", len(empty.Targets))
	}
	// 但分组明细仍然完整：被过滤的是"结论"，不是"原始观察"。
	if empty.Totals.Groups != 3 {
		t.Errorf("Groups = %d, want 3 (detail rows are unaffected)", empty.Totals.Groups)
	}
	// 再调一次仍然稳定。
	again := strict.Report(ReportOptions{TopGroups: 0})
	if again.Totals.GroupsShown != empty.Totals.GroupsShown || again.Totals.Groups != empty.Totals.Groups {
		t.Error("repeated Report calls with MinSamples produced different results")
	}
}

// TestCollectorRecordsClientVersions 验证版本被记录。
//
// 测量逻辑演进后，不同版本的样本不该被当成同一口径的数据。
func TestCollectorRecordsClientVersions(t *testing.T) {
	c := NewCollector(Options{})

	for _, version := range []string{"0.1.0", "0.2.0", "0.1.0"} {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			collector: "c1", success: true, latency: 10, version: version,
		}))
	}

	report := c.Report(ReportOptions{TopGroups: 0})
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(report.Groups))
	}
	versions := report.Groups[0].ClientVersions
	if len(versions) != 2 {
		t.Fatalf("versions = %v, want 2 distinct", versions)
	}
	// 排序保证输出稳定。
	if versions[0] != "0.1.0" || versions[1] != "0.2.0" {
		t.Errorf("versions = %v, want sorted", versions)
	}
}

func TestCollectorRecordsSessions(t *testing.T) {
	c := NewCollector(Options{})

	for _, session := range []string{"s1", "s2", "s1"} {
		c.Add(makeRow(rowOptions{
			kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
			collector: "c1", session: session, success: true, latency: 10,
		}))
	}

	report := c.Report(ReportOptions{TopGroups: 0})
	if report.Groups[0].Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", report.Groups[0].Sessions)
	}
}

// TestRowsWithoutPayloadAreIgnored 验证缺 payload 的行不会 panic。
func TestRowsWithoutPayloadAreIgnored(t *testing.T) {
	c := NewCollector(Options{})

	// kind 是 measurement 但没有 measurement 对象。
	c.Add(Row{Kind: "measurement", TargetID: "1.1.1.1:443", CollectorID: "c1"})
	// kind 未知。
	c.Add(Row{Kind: "banana", TargetID: "1.1.1.1:443", CollectorID: "c1"})
	// 空行。
	c.Add(Row{})

	report := c.Report(ReportOptions{})
	if report.Totals.ProbeTotal != 0 {
		t.Errorf("ProbeTotal = %d, want 0", report.Totals.ProbeTotal)
	}
	// 这些行什么都没贡献，因此**不**算作"参与的采集者"。
	// 把它们算进去会让 CollectorCount 虚高，进而让
	// "数据来自多少个节点"这个结论失准。
	if report.CollectorCount != 0 {
		t.Errorf("CollectorCount = %d, want 0 (rows without payload contribute nothing)", report.CollectorCount)
	}

	// 但一行有内容的数据仍然会被记为参与者。
	c.Add(makeRow(rowOptions{
		kind: "measurement", targetID: "1.1.1.1:443", ip: "1.1.1.1", port: 443,
		collector: "c-real", success: true, latency: 10,
	}))
	report = c.Report(ReportOptions{})
	if report.CollectorCount != 1 {
		t.Errorf("CollectorCount = %d, want 1 after adding a real row", report.CollectorCount)
	}
}

func TestErrorCountsSorted(t *testing.T) {
	counts := ErrorCounts{}
	counts.Add("timeout")
	counts.Add("timeout")
	counts.Add("refused")
	counts.Add("")
	counts.Add("aaa")

	sorted := counts.Sorted()
	if len(sorted) != 3 {
		t.Fatalf("sorted = %v, want 3 entries (empty type ignored)", sorted)
	}
	if sorted[0].Type != "timeout" || sorted[0].Count != 2 {
		t.Errorf("first = %+v, want timeout x2", sorted[0])
	}
	// 数量相同则按类型名排序，保证输出稳定。
	if sorted[1].Type != "aaa" || sorted[2].Type != "refused" {
		t.Errorf("ties are not ordered by name: %v", sorted)
	}
}

func TestGroupKeyString(t *testing.T) {
	key := GroupKey{TargetID: "1.1.1.1:443", Country: "CN", Province: "Zhejiang",
		City: "Hangzhou", ISP: "China Mobile", ASN: "AS9808"}

	want := "1.1.1.1:443|CN|Zhejiang|Hangzhou|China Mobile|AS9808"
	if got := key.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// 相同分组必须生成完全一致的键（跨时间、跨节点可比）。
	other := key
	if key.String() != other.String() {
		t.Error("identical keys produced different strings")
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// writeFile 写入测试文件（必要时创建父目录）。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

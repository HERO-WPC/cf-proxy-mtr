package query

import (
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/aggregate"
)

// makeMeasurement 构造一条测量行。
func makeMeasurement(targetID, ip string, port int, region []string, success bool, latency float64, opts ...func(*Row)) Row {
	row := Row{
		Kind:         "measurement",
		TargetID:     targetID,
		IP:           ip,
		Port:         port,
		CollectorID:  "c1",
		TimestampUTC: "2026-10-03T10:00:00Z",
	}
	row.Collector = collectorRef(region)
	row.Measurement = &aggregate.MeasurementRef{Success: success, LatencyMS: latency}
	for _, opt := range opts {
		opt(&row)
	}
	return row
}

// makeTrace 构造一条跟踪行。
func makeTrace(targetID, ip string, port int, region []string, hops []aggregate.HopView, opts ...func(*Row)) Row {
	row := Row{
		Kind:         "trace",
		TargetID:     targetID,
		IP:           ip,
		Port:         port,
		CollectorID:  "c1",
		TimestampUTC: "2026-10-03T10:00:00Z",
	}
	row.Collector = collectorRef(region)
	row.Trace = &aggregate.TraceRef{
		Success: true, Engine: "nexttrace", Mode: "tcp",
		DurationMS: 1200, HopCount: len(hops), Hops: hops,
	}
	for _, opt := range opts {
		opt(&row)
	}
	return row
}

// collectorRef 由 [country, province, city, isp, asn] 构造采集者信息。
func collectorRef(region []string) *aggregate.CollectorRef {
	ref := &aggregate.CollectorRef{}
	if len(region) > 0 {
		ref.Country = region[0]
	}
	if len(region) > 1 {
		ref.Province = region[1]
	}
	if len(region) > 2 {
		ref.City = region[2]
	}
	if len(region) > 3 {
		ref.ISP = region[3]
	}
	if len(region) > 4 {
		ref.ASN = region[4]
	}
	return ref
}

// withTime 覆盖时间戳。
func withTime(at string) func(*Row) {
	return func(row *Row) { row.TimestampUTC = at }
}

// withCollector 覆盖采集者 ID。
func withCollector(id string) func(*Row) {
	return func(row *Row) { row.CollectorID = id }
}

// withSession 覆盖会话 ID。
func withSession(id string) func(*Row) {
	return func(row *Row) { row.SessionID = id }
}

// withVersion 覆盖客户端版本。
func withVersion(v string) func(*Row) {
	return func(row *Row) { row.ClientVersion = v }
}

// regionCN / regionUS 是测试用的两个地区。
var (
	regionCN = []string{"CN", "Zhejiang", "Hangzhou", "China Mobile", "AS9808"}
	regionUS = []string{"US", "California", "Los Angeles", "Vultr", "AS20473"}
)

// ---------------------------------------------------------------------------
// 索引与查询
// ---------------------------------------------------------------------------

func TestIndexGroupsByRegion(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 300),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionUS, true, 250),
	}, IndexOptions{})

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	if profile.RegionCount() != 2 {
		t.Fatalf("Regions = %d, want 2 (one per collector region)", profile.RegionCount())
	}
	if profile.ProbeTotal != 2 || profile.ProbeSuccess != 2 {
		t.Errorf("probes = %d/%d, want 2/2", profile.ProbeSuccess, profile.ProbeTotal)
	}

	// 跨地区差异必须被算出来。
	spread := profile.LatencySpreadMS()
	if spread < 40 || spread > 60 {
		t.Errorf("LatencySpreadMS = %v, want about 50 (300 - 250)", spread)
	}
}

func TestLookupAcceptskBareIPWithSinglePort(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10),
	}, IndexOptions{})

	// 裸 IP 且只有一个端口：可以直接命中。
	profile, err := d.Lookup("1.1.1.1")
	if err != nil {
		t.Fatalf("Lookup with a bare IP: %v", err)
	}
	if profile.TargetID != "1.1.1.1:443" {
		t.Errorf("TargetID = %q", profile.TargetID)
	}
}

// TestLookupAmbiguousBareIPIsAnError 验证有歧义时不瞎猜。
//
// 同一个 IP 有多个端口时，静默返回其中一个会给出**错误的目标**——
// 而用户会以为那就是他要的。
func TestLookupAmbiguousBareIPIsAnError(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10),
		makeMeasurement("1.1.1.1:2053", "1.1.1.1", 2053, regionCN, true, 11),
	}, IndexOptions{})

	_, err := d.Lookup("1.1.1.1")
	if err == nil {
		t.Fatal("Lookup returned a profile for an ambiguous bare IP")
	}
	// 错误信息必须告诉用户该怎么办。
	if !strings.Contains(err.Error(), "specify the port") {
		t.Errorf("error = %v, want it to ask for a port", err)
	}
	if !strings.Contains(err.Error(), "2053") || !strings.Contains(err.Error(), "443") {
		t.Errorf("error = %v, want the available ports listed", err)
	}
}

func TestLookupMissingTarget(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10),
	}, IndexOptions{})

	_, err := d.Lookup("8.8.8.8:443")
	if err == nil {
		t.Fatal("Lookup found a target that was never indexed")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want a 'not found' message", err)
	}
}

func TestLookupNormalizesIPv6(t *testing.T) {
	d := NewDataset()
	// 索引里用规范化形式。
	d.IndexRows([]Row{
		makeMeasurement("[2001:db8::1]:443", "2001:db8::1", 443, regionCN, true, 10),
	}, IndexOptions{})

	// 用"展开"的写法查询也应当命中（规范化后一致）。
	profile, err := d.Lookup("[2001:0db8:0000:0000:0000:0000:0000:0001]:443")
	if err != nil {
		t.Fatalf("Lookup with an expanded IPv6 form: %v", err)
	}
	if profile.Port != 443 {
		t.Errorf("Port = %d", profile.Port)
	}
}

func TestLookupRejectsEmptyAndBadInput(t *testing.T) {
	d := NewDataset()
	for _, bad := range []string{"", "   ", "not-an-ip", "1.2.3.4:99999"} {
		if _, err := d.Lookup(bad); err == nil {
			t.Errorf("Lookup(%q) succeeded, want an error", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// 统计正确性
// ---------------------------------------------------------------------------

func TestSuccessAndFailureAccounting(t *testing.T) {
	d := NewDataset()

	rows := []Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 100),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 110),
	}
	// 两条失败：一条超时一条被拒。
	failed := makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, false, 3000)
	failed.Measurement.ErrorType = "timeout"
	refused := makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, false, 5)
	refused.Measurement.ErrorType = "connection_refused"
	rows = append(rows, failed, refused)

	d.IndexRows(rows, IndexOptions{})

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ProbeTotal != 4 || profile.ProbeSuccess != 2 {
		t.Fatalf("probes = %d/%d, want 2/4", profile.ProbeSuccess, profile.ProbeTotal)
	}
	if rate := profile.SuccessRate(); rate != 0.5 {
		t.Errorf("SuccessRate = %v, want 0.5", rate)
	}

	// 失败分类必须保留。
	errors := profile.Errors.Sorted()
	if len(errors) != 2 {
		t.Fatalf("error kinds = %d, want 2", len(errors))
	}

	// 失败的 3000ms 不能进延迟统计。
	if profile.Latency.Max > 200 {
		t.Errorf("Max latency = %v, the 3000ms failure must be excluded", profile.Latency.Max)
	}
	if profile.Latency.Count != 2 {
		t.Errorf("latency samples = %d, want 2", profile.Latency.Count)
	}
}

// ---------------------------------------------------------------------------
// AS 路径
// ---------------------------------------------------------------------------

func TestASPathsAreTrackedPerRegion(t *testing.T) {
	cnHops := []aggregate.HopView{
		{TTL: 1, IP: "private-v4"},
		{TTL: 2, ASN: "AS9808"},
		{TTL: 3, ASN: "AS58453"},
		{TTL: 4, ASN: "AS13335"},
	}
	usHops := []aggregate.HopView{
		{TTL: 1, ASN: "AS20473"},
		{TTL: 2, ASN: "AS13335"},
	}

	d := NewDataset()
	d.IndexRows([]Row{
		makeTrace("1.1.1.1:443", "1.1.1.1", 443, regionCN, cnHops),
		makeTrace("1.1.1.1:443", "1.1.1.1", 443, regionUS, usHops),
	}, IndexOptions{})

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}

	regions := profile.RegionsSorted()
	if len(regions) != 2 {
		t.Fatalf("regions = %d, want 2", len(regions))
	}

	// 每个分组各有一条 AS 路径，且签名里不含没有 ASN 的跳。
	seen := map[string]string{}
	for _, region := range regions {
		paths := region.ASPathsSorted()
		if len(paths) != 1 {
			t.Fatalf("region %s has %d AS paths, want 1", region.Label(), len(paths))
		}
		seen[region.Country] = paths[0].Signature
	}
	if seen["CN"] != "AS9808-AS58453-AS13335" {
		t.Errorf("CN path = %q", seen["CN"])
	}
	if seen["US"] != "AS20473-AS13335" {
		t.Errorf("US path = %q", seen["US"])
	}
}

// TestSameASPathCountsUp 验证同一路径多次出现会累加。
func TestSameASPathCountsUp(t *testing.T) {
	hops := []aggregate.HopView{{TTL: 1, ASN: "AS9808"}, {TTL: 2, ASN: "AS13335"}}

	d := NewDataset()
	for i := 0; i < 3; i++ {
		d.IndexRows([]Row{
			makeTrace("1.1.1.1:443", "1.1.1.1", 443, regionCN, hops,
				withTime("2026-10-03T10:0"+string(rune('0'+i))+":00Z")),
		}, IndexOptions{})
	}

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}
	paths := profile.RegionsSorted()[0].ASPathsSorted()
	if len(paths) != 1 {
		t.Fatalf("paths = %d, want 1 distinct", len(paths))
	}
	if paths[0].Count != 3 {
		t.Errorf("count = %d, want 3", paths[0].Count)
	}
}

// ---------------------------------------------------------------------------
// 逐跳统计
// ---------------------------------------------------------------------------

func TestHopStatsAggregateByTTL(t *testing.T) {
	d := NewDataset()

	// 两次跟踪：同一 TTL 由不同等价路由器应答（负载分担是常态）。
	first := []aggregate.HopView{
		{TTL: 1, IP: "10.0.0.1", RTTMS: []float64{1.0, 2.0}},
		{TTL: 2, Timeout: true},
	}
	second := []aggregate.HopView{
		{TTL: 1, IP: "10.0.0.2", RTTMS: []float64{3.0}},
		{TTL: 2, IP: "1.1.1.1", ASN: "AS13335", RTTMS: []float64{10.0}},
	}

	d.IndexRows([]Row{
		makeTrace("1.1.1.1:443", "1.1.1.1", 443, regionCN, first, withTime("2026-10-03T10:00:00Z")),
		makeTrace("1.1.1.1:443", "1.1.1.1", 443, regionCN, second, withTime("2026-10-03T11:00:00Z")),
	}, IndexOptions{})

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}
	region := profile.RegionsSorted()[0]

	stats := region.HopStatsSorted()
	if len(stats) != 2 {
		t.Fatalf("hop stats = %d, want 2 (grouped by TTL, not by IP)", len(stats))
	}

	// TTL 1：两次都成功，共 3 个 RTT 样本。
	ttl1 := stats[0]
	if ttl1.RTTs.Count != 3 {
		t.Errorf("TTL 1 samples = %d, want 3", ttl1.RTTs.Count)
	}
	if ttl1.Timeouts != 0 {
		t.Errorf("TTL 1 timeouts = %d, want 0", ttl1.Timeouts)
	}

	// TTL 2：一次超时一次成功。
	ttl2 := stats[1]
	if ttl2.Timeouts != 1 {
		t.Errorf("TTL 2 timeouts = %d, want 1", ttl2.Timeouts)
	}
	if ttl2.RTTs.Count != 1 {
		t.Errorf("TTL 2 samples = %d, want 1", ttl2.RTTs.Count)
	}
	// 超时那次的 RTT 0 不能进直方图（否则 p50 会被拉成 0）。
	if median, ok := ttl2.RTTs.Quantile(0.50); !ok || median != 10.0 {
		t.Errorf("TTL 2 p50 = %v, want exactly 10.0", median)
	}
	if ttl2.ASN != "AS13335" {
		t.Errorf("TTL 2 ASN = %q", ttl2.ASN)
	}
}

// ---------------------------------------------------------------------------
// 时间序列
// ---------------------------------------------------------------------------

func TestSeriesKeepsMostRecentPoints(t *testing.T) {
	d := NewDataset()

	// IndexRows 的 MaxSeries 是"每个目标保留的点数"。
	const maxSeries = 3
	for i := 0; i < 6; i++ {
		d.IndexRows([]Row{
			makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, float64(10+i),
				withTime(timeAt("2026-10-03T10:0"+itoa(i)+":00Z"))),
		}, IndexOptions{MaxSeries: maxSeries})
	}

	profile, err := d.Lookup("1.1.1.1:443")
	if err != nil {
		t.Fatal(err)
	}

	if len(profile.Series) != maxSeries {
		t.Fatalf("series points = %d, want %d", len(profile.Series), maxSeries)
	}
	// 保留的应当是**最近**的点（延迟 13/14/15）。
	first := profile.Series[0]
	if first.LatencyMS != 13 {
		t.Errorf("oldest kept point = %v ms, want 13 (the most recent window)", first.LatencyMS)
	}
	last := profile.Series[len(profile.Series)-1]
	if last.LatencyMS != 15 {
		t.Errorf("newest point = %v ms, want 15", last.LatencyMS)
	}

	// 但精确统计不受这个上限影响。
	if profile.ProbeTotal != 6 {
		t.Errorf("ProbeTotal = %d, want 6 (the cap only affects the series)", profile.ProbeTotal)
	}
}

// timeAt 解析测试用时间字符串；失败时 panic（测试夹具问题）。
func timeAt(s string) string {
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		panic(err)
	}
	return s
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
// 边界
// ---------------------------------------------------------------------------

func TestIndexSkipsRowsWithoutTargetOrPayload(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		{Kind: "measurement"},                          // 没有 target_id
		{Kind: "measurement", TargetID: "1.1.1.1:443"}, // 没有 payload
		{Kind: "banana", TargetID: "1.1.1.1:443"},      // 未知 kind
		{Kind: "trace", TargetID: "1.1.1.1:443"},       // 没有 trace payload
	}, IndexOptions{})

	if d.TargetCount() != 1 {
		t.Errorf("targets = %d, want 1", d.TargetCount())
	}
	if d.BadRows == 0 {
		t.Error("BadRows = 0, want the row without a target id counted")
	}
	profile, _ := d.Lookup("1.1.1.1:443")
	if profile.ProbeTotal != 0 || profile.TraceTotal != 0 {
		t.Errorf("profile = %+v, want no data", profile)
	}
}

func TestMaxTargetsIsEnforced(t *testing.T) {
	d := NewDataset()
	rows := make([]Row, 0, 5)
	for i := 0; i < 5; i++ {
		ip := "1.1.1." + itoa(1+i)
		rows = append(rows, makeMeasurement(ip+":443", ip, 443, regionCN, true, 10))
	}
	d.IndexRows(rows, IndexOptions{MaxTargets: 2})

	if d.TargetCount() > 2 {
		t.Errorf("targets = %d, want at most 2", d.TargetCount())
	}
	if d.BadRows == 0 {
		t.Error("BadRows = 0, but rows were dropped by the target cap")
	}
}

func TestDatasetTracksCollectorsAndTimeRange(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10,
			withCollector("c-a"), withTime("2026-10-03T10:00:00Z")),
		makeMeasurement("8.8.8.8:443", "8.8.8.8", 443, regionUS, true, 20,
			withCollector("c-b"), withTime("2026-10-03T12:00:00Z")),
	}, IndexOptions{})

	if len(d.Collectors) != 2 {
		t.Errorf("collectors = %d, want 2", len(d.Collectors))
	}
	want := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	if !d.FirstTimestamp.Equal(want) {
		t.Errorf("FirstTimestamp = %s, want %s", d.FirstTimestamp, want)
	}
	wantLast := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if !d.LastTimestamp.Equal(wantLast) {
		t.Errorf("LastTimestamp = %s, want %s", d.LastTimestamp, wantLast)
	}
}

func TestTargetsSortedAndRegionsSortedStable(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("8.8.8.8:443", "8.8.8.8", 443, regionUS, true, 10),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionUS, true, 10),
	}, IndexOptions{})

	targets := d.Targets()
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	// 按 TargetID 排序，保证输出稳定（可比对、可 diff）。
	if targets[0].TargetID != "1.1.1.1:443" || targets[1].TargetID != "8.8.8.8:443" {
		t.Errorf("targets not sorted: %s, %s", targets[0].TargetID, targets[1].TargetID)
	}

	// 1.1.1.1 有两个分组，各 1 个样本；排序必须稳定（按标签）。
	regions := targets[0].RegionsSorted()
	if len(regions) != 2 {
		t.Fatalf("regions = %d, want 2", len(regions))
	}
	first := regions[0].Label()
	second := regions[1].Label()
	if first > second {
		t.Errorf("regions not sorted: %q then %q", first, second)
	}
}

func TestSessionsAndVersionsTracked(t *testing.T) {
	d := NewDataset()
	d.IndexRows([]Row{
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 10,
			withSession("s1"), withVersion("0.1.0")),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 11,
			withSession("s2"), withVersion("0.2.0")),
		makeMeasurement("1.1.1.1:443", "1.1.1.1", 443, regionCN, true, 12,
			withSession("s1"), withVersion("0.1.0")),
	}, IndexOptions{})

	profile, _ := d.Lookup("1.1.1.1:443")
	if len(profile.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(profile.Sessions))
	}
	if len(profile.ClientVersions) != 2 {
		t.Errorf("versions = %d, want 2", len(profile.ClientVersions))
	}
}

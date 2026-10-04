package service

import (
	"path/filepath"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 本文件覆盖"先测 TCP、再挑一批跟踪"里的两个关键判断：
// 按国家筛选目标，以及从 CSV 里挑出要跟踪的目标。
//
// 这两处都属于"错了不会报错、只是结果不对"的类型，因此测试的价值
// 特别高：国家字段选错会让筛选结果与界面上的数字不符；重建目标时
// 少填一个字段会让每一步都"看起来正常"而跟踪全部失败。

// ---------------------------------------------------------------------------
// 国家筛选
// ---------------------------------------------------------------------------

func targetWith(id, cca2 string) model.Target {
	return model.Target{ID: id, IP: "1.2.3.4", Port: 443, Location: model.Location{CCA2: cca2}}
}

// TestFilterByCountriesUsesCCA2Only 验证只认 CCA2。
//
// 上游的 Location 有两个国家字段且实测约 18% 互相矛盾
// （country=FI 而 cca2=SE）。若这里退回读 Country，
// 同一个国家的目标会时而被选中、时而不被选中，而且没有任何提示。
func TestFilterByCountriesUsesCCA2Only(t *testing.T) {
	targets := []model.Target{
		{ID: "a:443", Location: model.Location{CCA2: "US", Country: "DE"}},
		{ID: "b:443", Location: model.Location{CCA2: "DE", Country: "US"}},
		{ID: "c:443", Location: model.Location{CCA2: "US", Country: "US"}},
	}

	got := filterByCountries(targets, []string{"US"})
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), ids(got))
	}
	for _, target := range got {
		if target.ID == "b:443" {
			t.Error("selected b:443, whose CCA2 is DE — Country must not be consulted")
		}
	}
}

// TestFilterByCountriesIsCaseInsensitive 验证大小写与空白都容忍。
func TestFilterByCountriesIsCaseInsensitive(t *testing.T) {
	targets := []model.Target{targetWith("a:443", "US"), targetWith("b:443", "DE")}

	for _, input := range [][]string{{"us"}, {" US "}, {"Us", "de"}} {
		if got := filterByCountries(targets, input); len(got) == 0 {
			t.Errorf("filterByCountries(%v) selected nothing", input)
		}
	}
}

// TestFilterByCountriesEmptyListMeansNoFilter 验证空条件不筛掉任何东西。
//
// 界面上的多选框没勾任何一项时传空列表。若把它理解成"筛掉所有"，
// 使用者会看到一个空结果而不知道为什么。
func TestFilterByCountriesEmptyListMeansNoFilter(t *testing.T) {
	targets := []model.Target{targetWith("a:443", "US"), targetWith("b:443", "DE")}

	for _, input := range [][]string{nil, {}, {""}, {"  "}} {
		if got := filterByCountries(targets, input); len(got) != len(targets) {
			t.Errorf("filterByCountries(%v) = %d targets, want all %d", input, len(got), len(targets))
		}
	}
}

// TestFilterByCountriesSkipsTargetsWithoutCountry 验证没有国家的目标
// 不会被算进任何国家。
//
// 把它们算进某个国家是猜；国家的正确做法是不选——想连它们一起测，
// 就不要用国家筛选。
func TestFilterByCountriesSkipsTargetsWithoutCountry(t *testing.T) {
	targets := []model.Target{targetWith("a:443", "US"), targetWith("b:443", "")}

	got := filterByCountries(targets, []string{"US"})
	if len(got) != 1 || got[0].ID != "a:443" {
		t.Errorf("got %v, want only a:443", ids(got))
	}
}

// TestTargetsByCountryMatchesFilter 验证统计与筛选口径一致。
//
// 这是"下拉框的数字必须等于筛出来的行数"的护栏：两者用同一个字段、
// 同一套归一化，否则会出现"显示 1388 个、筛出 1580 个"这种
// 没有报错的错误。
func TestTargetsByCountryMatchesFilter(t *testing.T) {
	targets := []model.Target{
		targetWith("a:443", "US"),
		targetWith("b:443", "us"),
		targetWith("c:443", "DE"),
		targetWith("d:443", ""),
	}

	counts := targetsByCountry(targets)
	total := 0
	for _, count := range counts {
		if got := len(filterByCountries(targets, []string{count.CCA2})); got != count.Count {
			t.Errorf("dropdown says %s has %d, filter returns %d", count.CCA2, count.Count, got)
		}
		if count.CCA2 == "" {
			t.Error("empty country code must not appear in the dropdown")
		}
		total += count.Count
	}
	if total != 3 {
		t.Errorf("counted %d targets across countries, want 3 (the empty one excluded)", total)
	}
}

// ---------------------------------------------------------------------------
// 从 CSV 挑要跟踪的目标
// ---------------------------------------------------------------------------

func rowFor(target, cca2 string, latency float64) csvstore.Row {
	return csvstore.Row{Target: target, IP: "1.2.3.4", Port: 443, Success: latency > 0, LatencyMS: latency, CCA2: cca2}
}

// TestSelectForTraceSetsIPVersion 是一次真实缺陷的回归护栏。
//
// 实测踩过：从 CSV 重建 Target 时只填了 IP/Port，没填 IPVersion，
// 于是跟踪引擎的 Validate 把每一个目标都判成 invalid_target，
// 而表现是"每一步都正常"——选取有结果、行也写进了 CSV——
// 只是 as_path 永远为空。只测选取函数的输出发现不了，因为它的
// 输出没问题，是引擎的校验拒了。
func TestSelectForTraceSetsIPVersion(t *testing.T) {
	rows := []csvstore.Row{
		{Target: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, LatencyMS: 10, CCA2: "US"},
		{Target: "[2001:db8::1]:443", IP: "2001:db8::1", Port: 443, LatencyMS: 20, CCA2: "US"},
	}

	selected := SelectForTrace(rows, nil, 0, 0)
	if len(selected) != 2 {
		t.Fatalf("selected %d targets, want 2", len(selected))
	}

	for _, target := range selected {
		if target.IPVersion == "" {
			t.Errorf("%s has an empty IPVersion; the trace engine will reject it", target.ID)
		}
		// 直接过一遍引擎用的同一套校验：这才是真正的判据。
		if err := target.Validate(); err != nil {
			t.Errorf("%s failed validation: %v", target.ID, err)
		}
	}
}

// TestSelectForTraceFiltersByLatency 验证延迟上限。
func TestSelectForTraceFiltersByLatency(t *testing.T) {
	rows := []csvstore.Row{
		rowFor("fast:443", "US", 20),
		rowFor("mid:443", "US", 150),
		rowFor("slow:443", "US", 400),
	}

	got := SelectForTrace(rows, nil, 200, 0)
	if len(got) != 2 {
		t.Fatalf("selected %v, want the two under 200ms", idsOf(got))
	}
	if got[0].ID != "fast:443" || got[1].ID != "mid:443" {
		t.Errorf("selected %v, want fast then mid (sorted by latency)", idsOf(got))
	}
}

// TestSelectForTraceSkipsUnmeasured 验证没有延迟的行不参与。
//
// 它们可能是"跟踪失败"的记录，也可能是一次超时；两种都不该被
// 当成"要跟踪的候选"，否则跟踪一批本来就没连上的目标毫无意义。
func TestSelectForTraceSkipsUnmeasured(t *testing.T) {
	rows := []csvstore.Row{
		rowFor("ok:443", "US", 30),
		{Target: "timeout:443", IP: "5.6.7.8", Port: 443, Success: false, ErrorType: "timeout", CCA2: "US"},
	}

	got := SelectForTrace(rows, nil, 0, 0)
	if len(got) != 1 || got[0].ID != "ok:443" {
		t.Errorf("selected %v, want only ok:443", idsOf(got))
	}
}

// TestSelectForTraceDeduplicatesAndKeepsBest 验证同一目标只出现一次，
// 且用最快的那一行。
//
// 一个目标可能有多行（探测一行、跟踪一行）。按最后一行算会让
// "要不要跟踪它"取决于文件里碰巧最后写的是哪次。
func TestSelectForTraceDeduplicatesAndKeepsBest(t *testing.T) {
	rows := []csvstore.Row{
		{Target: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, LatencyMS: 300, CCA2: "US"},
		{Target: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, LatencyMS: 25, CCA2: "US"},
		{Target: "5.6.7.8:443", IP: "5.6.7.8", Port: 443, LatencyMS: 40, CCA2: "US"},
	}

	got := SelectForTrace(rows, nil, 0, 0)
	if len(got) != 2 {
		t.Fatalf("selected %v, want 2 distinct targets", idsOf(got))
	}
	// 用最快的那行（25ms）→ 它应当排在 40ms 之前。
	if got[0].ID != "1.2.3.4:443" {
		t.Errorf("selected order %v, want the deduped target first (it has the best latency)", idsOf(got))
	}
}

// TestSelectForTraceRespectsLimit 验证条数上限按延迟从快到慢取。
//
// 上限很重要：一次 traceroute 实测十几秒，不限量就可能变成几小时。
func TestSelectForTraceRespectsLimit(t *testing.T) {
	rows := []csvstore.Row{
		rowFor("c:443", "US", 300),
		rowFor("a:443", "US", 10),
		rowFor("b:443", "US", 100),
	}

	got := SelectForTrace(rows, nil, 0, 2)
	if len(got) != 2 {
		t.Fatalf("selected %d, want 2", len(got))
	}
	if got[0].ID != "a:443" || got[1].ID != "b:443" {
		t.Errorf("selected %v, want the two fastest (a, b)", idsOf(got))
	}
}

// TestSelectForTraceFiltersByCountry 验证按国家挑。
func TestSelectForTraceFiltersByCountry(t *testing.T) {
	rows := []csvstore.Row{
		rowFor("us:443", "US", 50),
		rowFor("de:443", "DE", 20),
	}

	got := SelectForTrace(rows, []string{"DE"}, 0, 0)
	if len(got) != 1 || got[0].ID != "de:443" {
		t.Errorf("selected %v, want only de:443", idsOf(got))
	}
}

// TestSelectForTraceCarriesCountryFromRow 验证国家从行里带过来。
//
// 而不是回头去目标列表里查：目标列表可能已经变了，而 CSV 里
// 这一行就是当时的事实。跟踪结果写回 CSV 时用的是这个国家，
// 因此它必须来自行本身。
func TestSelectForTraceCarriesCountryFromRow(t *testing.T) {
	rows := []csvstore.Row{rowFor("a:443", "de", 10)}

	got := SelectForTrace(rows, nil, 0, 0)
	if len(got) != 1 {
		t.Fatalf("selected %d, want 1", len(got))
	}
	if got[0].Location.CCA2 != "DE" {
		t.Errorf("CCA2 = %q, want DE (normalized from the row)", got[0].Location.CCA2)
	}
}

// TestSelectForTraceRejectsBadIP 验证 IP 解析不了的行被跳过。
func TestSelectForTraceRejectsBadIP(t *testing.T) {
	rows := []csvstore.Row{
		{Target: "garbage:443", IP: "not-an-ip", Port: 443, LatencyMS: 10},
		rowFor("ok:443", "US", 20),
	}

	got := SelectForTrace(rows, nil, 0, 0)
	if len(got) != 1 || got[0].ID != "ok:443" {
		t.Errorf("selected %v, want only ok:443", idsOf(got))
	}
}

// TestPreviewTraceSelectionMatchesActualSelection 验证预览与实际
// 选取是同一套结果。
//
// 界面上的"N 个"就是使用者对"要等多久"的唯一预期，它与实际跟踪的
// 目标数不一致会直接变成"说好 12 个，跑了 40 个"。
func TestPreviewTraceSelectionMatchesActualSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	store, err := csvstore.Open(csvstore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	rows := []csvstore.Row{
		rowFor("a:443", "US", 10),
		rowFor("b:443", "US", 120),
		rowFor("c:443", "DE", 30),
		rowFor("d:443", "DE", 900),
	}
	for _, row := range rows {
		if err := store.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	const limit = 2
	preview, err := PreviewTraceSelection(path, []string{"US"}, 200, limit)
	if err != nil {
		t.Fatalf("PreviewTraceSelection: %v", err)
	}

	read, err := csvstore.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := SelectForTrace(read, []string{"US"}, 200, limit)

	if preview.Count != len(actual) {
		t.Errorf("preview says %d targets, actual selection is %d", preview.Count, len(actual))
	}
	if preview.Count != 2 {
		t.Errorf("preview count = %d, want 2 (US rows under 200ms)", preview.Count)
	}
	if preview.FastestMS != 10 || preview.SlowestMS != 120 {
		t.Errorf("latency range = %.1f~%.1f, want 10~120", preview.FastestMS, preview.SlowestMS)
	}
	// 预览里的国家统计来自同一份文件，供界面画下拉框。
	if len(preview.Countries) != 2 {
		t.Errorf("preview reported %d countries, want 2", len(preview.Countries))
	}
}

func ids(targets []model.Target) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.ID)
	}
	return out
}

func idsOf(targets []model.Target) []string { return ids(targets) }

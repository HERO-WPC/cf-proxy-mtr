package csvstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 CSV 的读取侧与排序。
//
// 读取侧存在的理由是"CSV 是本项目唯一的数据源"：界面要展示、要按
// 国家筛选、要挑一批去跟踪，全都得把它读回来。因此这里也覆盖
// **老文件**（没有 cca2 列）与**坏行**（写到一半被杀）——这两种
// 情况在生产里都会遇到。

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "results.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadAllRoundTrip 验证写出去再读回来是同一份数据。
func TestReadAllRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	written := []Row{
		{
			Timestamp: time.Date(2026, 10, 4, 9, 10, 7, 0, time.UTC),
			Target:    "1.2.3.4:443",
			IP:        "1.2.3.4",
			Port:      443,
			Success:   true,
			LatencyMS: 42.5,
			CCA2:      "US",
		},
		{
			Timestamp:    time.Date(2026, 10, 4, 9, 10, 8, 0, time.UTC),
			Target:       "2.3.4.5:8443",
			IP:           "2.3.4.5",
			Port:         8443,
			Success:      false,
			ErrorType:    "timeout",
			ErrorMessage: "i/o timeout",
			CCA2:         "DE",
		},
		{
			Timestamp: time.Date(2026, 10, 4, 9, 10, 9, 0, time.UTC),
			Target:    "1.2.3.4:443",
			IP:        "1.2.3.4",
			Port:      443,
			Success:   true,
			HopCount:  25,
			ASPath:    "CMNET > CMI > Cogent",
			CCA2:      "US",
		},
	}
	for _, row := range written {
		if err := store.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != len(written) {
		t.Fatalf("read %d rows, want %d", len(rows), len(written))
	}

	first := rows[0]
	if first.Target != "1.2.3.4:443" || !first.Success || first.LatencyMS != 42.5 || first.CCA2 != "US" {
		t.Errorf("first row round-tripped wrong: %+v", first)
	}
	if !first.Timestamp.Equal(written[0].Timestamp) {
		t.Errorf("timestamp = %v, want %v", first.Timestamp, written[0].Timestamp)
	}

	// "没测到"必须还是"没测到"，不能变成 0。
	if rows[1].LatencyMS != 0 {
		t.Errorf("failed row latency = %v, want 0 (unmeasured)", rows[1].LatencyMS)
	}
	if rows[1].ErrorType != "timeout" || rows[1].ErrorMessage != "i/o timeout" {
		t.Errorf("failed row lost its reason: %+v", rows[1])
	}

	// 跟踪信息也要读回来。
	if rows[2].ASPath != "CMNET > CMI > Cogent" || rows[2].HopCount != 25 {
		t.Errorf("trace row lost its route: %+v", rows[2])
	}
}

// TestReadAllAcceptsFileWithoutCCA2 验证**老文件**（没有 cca2 列）照样能读。
//
// 这一条很实际：cca2 是后加的列，而使用者手里已经有之前测出来的 CSV。
// 按下标读会把它缺失错位成别的字段——国家变成空、跳数变成线路。
func TestReadAllAcceptsFileWithoutCCA2(t *testing.T) {
	path := writeFile(t, strings.Join([]string{
		"timestamp_utc,target,ip,port,success,latency_ms,error_type,error_message,hop_count,as_path,hops,client_version",
		"2026-10-04T09:10:07Z,1.2.3.4:443,1.2.3.4,443,true,42.5,,,25,CMNET > CMI,,0.1.0",
	}, "\n")+"\n")

	rows, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %d rows, want 1", len(rows))
	}

	row := rows[0]
	if row.CCA2 != "" {
		t.Errorf("CCA2 = %q, want empty for an old file", row.CCA2)
	}
	// 关键：后面的列不能因为少一列而错位。
	if row.ASPath != "CMNET > CMI" {
		t.Errorf("ASPath = %q, want %q — columns shifted", row.ASPath, "CMNET > CMI")
	}
	if row.HopCount != 25 {
		t.Errorf("HopCount = %d, want 25", row.HopCount)
	}
	if row.LatencyMS != 42.5 {
		t.Errorf("LatencyMS = %v, want 42.5", row.LatencyMS)
	}
}

// TestReadAllSurvivesTruncatedLastLine 验证**最后一行写了一半**时，
// 前面的行照样读得出来。
//
// 这是真实会发生的：CSV 是流式追加写的，进程被杀（或断电）时最后
// 一行可能只写了一半。为了那一行而让 1.5 万行结果完全打不开，
// 是不可接受的。
//
// 注意断言的是"前面的行完好"，而不是"半行被丢掉"：行尾被截断时
// 它仍然是一个合法的 CSV 记录（只是末尾几列为空），因此会作为一行
// 出现，字段缺失。把它当坏行丢掉反而会丢真实数据。
func TestReadAllSurvivesTruncatedLastLine(t *testing.T) {
	path := writeFile(t, strings.Join([]string{
		"timestamp_utc,target,ip,port,success,latency_ms",
		"2026-10-04T09:10:07Z,1.2.3.4:443,1.2.3.4,443,true,42.5",
		"2026-10-04T09:10:08Z,5.6.7.8:443,5.6.7.8,443,true,55.5",
		// 没有换行、字段不全——写到一半就被杀了。
		"2026-10-04T09:10:09Z,3.4.5.6:443,3.4",
	}, "\n"))

	rows, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3: %+v", len(rows), rows)
	}

	// 前两行必须完好。
	if rows[0].Target != "1.2.3.4:443" || rows[0].LatencyMS != 42.5 {
		t.Errorf("first row damaged: %+v", rows[0])
	}
	if rows[1].Target != "5.6.7.8:443" || rows[1].LatencyMS != 55.5 {
		t.Errorf("second row damaged: %+v", rows[1])
	}

	// 半行：目标认得出，缺失的字段是零值，而不是让整个文件读不出来。
	if rows[2].Target != "3.4.5.6:443" {
		t.Errorf("truncated row target = %q, want 3.4.5.6:443", rows[2].Target)
	}
	if rows[2].LatencyMS != 0 {
		t.Errorf("truncated row latency = %v, want 0 (missing)", rows[2].LatencyMS)
	}
}

// TestReadAllStopsAtUnterminatedQuote 记录一个**CSV 本身的**限制。
//
// 未闭合的引号会让解析器把余下的内容全部当成同一个字段，因此后面的
// 行读不出来。这不是我们能在读取侧修好的：在 CSV 语义里那些字节
// 确实属于同一个字段。测试在这里固定住行为，免得日后有人以为
// "坏行只影响它自己"而据此做假设。
//
// 实际影响有限：写出侧会把字段正确转义（见 TestSpecialCharactersAreQuoted），
// 所以只有文件被外部工具改坏才可能出现这种情况。
func TestReadAllStopsAtUnterminatedQuote(t *testing.T) {
	path := writeFile(t, strings.Join([]string{
		"timestamp_utc,target,ip,port,success,latency_ms",
		"2026-10-04T09:10:07Z,1.2.3.4:443,1.2.3.4,443,true,42.5",
		`2026-10-04T09:10:08Z,"unterminated quote,2.3.4.5,8443,true,10`,
		"2026-10-04T09:10:09Z,3.4.5.6:443,3.4.5.6,443,true,55.5",
	}, "\n")+"\n")

	rows, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll should not fail outright: %v", err)
	}

	// 引号之前的那一行必须仍在。
	if len(rows) == 0 || rows[0].Target != "1.2.3.4:443" {
		t.Fatalf("rows before the unterminated quote were lost: %+v", rows)
	}
	// 不做"后面那些行还在"的断言：按 CSV 语义它们已经被吞掉了。
}

// TestReadAllRejectsNonResultFile 验证不是结果文件时明确报错。
func TestReadAllRejectsNonResultFile(t *testing.T) {
	path := writeFile(t, "hello,world\n1,2\n")
	if _, err := ReadAll(path); err == nil {
		t.Fatal("ReadAll accepted a file that is not a result CSV")
	}
}

// TestReadAllMissingFileIsNotExist 验证文件不存在时报"不存在"。
//
// 界面靠这个区分"还没测过"（正常）与"读坏了"（要报错）。
func TestReadAllMissingFileIsNotExist(t *testing.T) {
	_, err := ReadAll(filepath.Join(t.TempDir(), "nope.csv"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

// ---------------------------------------------------------------------------
// 排序
// ---------------------------------------------------------------------------

func rowsForSort() []Row {
	return []Row{
		{Target: "slow:443", LatencyMS: 300},
		{Target: "failed:443", Success: false},
		{Target: "fast:443", LatencyMS: 20},
		{Target: "mid:443", LatencyMS: 150},
	}
}

// TestSortByLatencyPutsUnmeasuredLast 验证按延迟排序时"没测到"排最后。
//
// 这是本文件里最重要的一条：把失败的行排在 20ms 那个前面，会让
// 使用者以为"最快的几个都通"，而实际上它们根本没测到。
func TestSortByLatencyPutsUnmeasuredLast(t *testing.T) {
	rows := rowsForSort()
	Sort(rows, SortLatency)

	want := []string{"fast:443", "mid:443", "slow:443", "failed:443"}
	for i, target := range want {
		if rows[i].Target != target {
			t.Fatalf("order = %v, want %v", targets(rows), want)
		}
	}
}

// TestSortByRouteGroupsSameRoute 验证按线路排序时同线路相邻，
// 且组间按组内最快延迟排。
func TestSortByRouteGroupsSameRoute(t *testing.T) {
	rows := []Row{
		{Target: "a:443", LatencyMS: 300, ASPath: "slow-route"},
		{Target: "b:443", LatencyMS: 250, ASPath: "slow-route"},
		{Target: "c:443", LatencyMS: 90, ASPath: "fast-route"},
		{Target: "d:443", LatencyMS: 95, ASPath: "fast-route"},
		{Target: "e:443", LatencyMS: 10, ASPath: ""}, // 没跟到线路
	}
	Sort(rows, SortRoute)

	// 快的线路组在前，组内按延迟；没线路的排最后。
	want := []string{"c:443", "d:443", "b:443", "a:443", "e:443"}
	if got := targets(rows); !equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// 优先线路置顶
// ---------------------------------------------------------------------------

// TestSortByRoutePinsPremiumRoutes 验证 CMIN2 / CN2 / 9929 排在最上面，
// 且次序固定。
//
// 这三条是大陆方向的优质线路（移动 CMIN2、电信 CN2、联通 9929/CUII）。
// 使用者测一堆目标，真正想先看的就是"哪些走了这三条"，
// 而按延迟排的话它们可能被埋在一堆更快的普通线路后面。
func TestSortByRoutePinsPremiumRoutes(t *testing.T) {
	rows := []Row{
		// 普通线路，延迟还更低——按纯延迟排它会排第一。
		{Target: "plain:443", LatencyMS: 10, ASPath: "CMNET > Cogent"},
		{Target: "cmin2:443", LatencyMS: 300, ASPath: "CMNET > CMIN2"},
		{Target: "cn2:443", LatencyMS: 200, ASPath: "163 > CN2"},
		{Target: "cuii:443", LatencyMS: 100, ASPath: "169 > 9929/CUII"},
		{Target: "none:443", LatencyMS: 5}, // 没跟到线路
	}

	Sort(rows, SortRoute)

	want := []string{"cmin2:443", "cn2:443", "cuii:443", "plain:443", "none:443"}
	if got := targets(rows); !equal(got, want) {
		t.Fatalf("order = %v, want %v\n"+
			"（优先线路按 CMIN2 → CN2 → 9929 置顶，其余仍按原有规则，"+
			"没有线路信息的排最后）", got, want)
	}

	// 优先线路内部也要按延迟排：同一等级里快的在前。
	inner := []Row{
		{Target: "slow:443", LatencyMS: 400, ASPath: "CMIN2"},
		{Target: "fast:443", LatencyMS: 50, ASPath: "CMIN2"},
	}
	Sort(inner, SortRoute)
	if inner[0].Target != "fast:443" {
		t.Errorf("within a premium group the fastest should come first, got %v", targets(inner))
	}
}

// TestSortByRouteMatchesWholeSegments 验证匹配的是**整段线路名**，
// 不是子串。
//
// 子串匹配会让一个恰好包含 "CN2" 的名字（例如将来的某个 "CN2X"）
// 被误判成优先线路。那种错误只会表现为"排序看着不太对"，极难排查。
func TestSortByRouteMatchesWholeSegments(t *testing.T) {
	rows := []Row{
		{Target: "lookalike:443", LatencyMS: 10, ASPath: "CMNET > CMI2"},
		{Target: "real:443", LatencyMS: 500, ASPath: "CMNET > CN2"},
	}

	Sort(rows, SortRoute)
	if rows[0].Target != "real:443" {
		t.Errorf("order = %v; only an exact route-name segment should be pinned", targets(rows))
	}
}

// TestPremiumRankTakesHighestPriorityInPath 验证一条路径经过多条优先线路时
// 取等级最高的那条——它确实走了更好的线路。
func TestPremiumRankTakesHighestPriorityInPath(t *testing.T) {
	cases := map[string]int{
		"CMIN2":                  0,
		"CN2":                    1,
		"9929/CUII":              2,
		"9929":                   2, // 别名
		"CMNET > CMIN2 > Cogent": 0,
		"163 > CN2 > Cloudflare": 1,
		"CN2 > CMIN2":            0, // 两条都有 → 取更高的
		"CMNET > CMI":            -1,
		"":                       -1,
		"CMNET > CMI2":           -1,
	}

	for path, want := range cases {
		row := Row{Target: "x:443", ASPath: path}
		if got := premiumRank(row); got != want {
			t.Errorf("premiumRank(%q) = %d, want %d", path, got, want)
		}
	}
}

// TestSortByLatencyIgnoresPremiumRoutes 验证**只有按线路排序**时才置顶。
//
// "按延迟排序"是一个明确的承诺：最快的就是第一个。在其中偷偷把
// 优先线路提上来，会让那个标签变成假话，而且使用者无法察觉——
// 他只会觉得"这个延迟排序怎么不太准"。
func TestSortByLatencyIgnoresPremiumRoutes(t *testing.T) {
	rows := []Row{
		{Target: "plain:443", LatencyMS: 10, ASPath: "CMNET > Cogent"},
		{Target: "cmin2:443", LatencyMS: 300, ASPath: "CMNET > CMIN2"},
	}

	Sort(rows, SortLatency)
	if rows[0].Target != "plain:443" {
		t.Errorf("order = %v; sorting by latency must stay strictly by latency", targets(rows))
	}
}

// TestSortIsStableForEqualLatency 验证延迟相同时顺序稳定。//
// 不稳定的话，界面每次刷新行序都可能不同，看起来像数据在跳。
func TestSortIsStableForEqualLatency(t *testing.T) {
	rows := []Row{
		{Target: "b:443", LatencyMS: 50},
		{Target: "a:443", LatencyMS: 50},
		{Target: "c:443", LatencyMS: 50},
	}
	first := targets(rows)
	Sort(rows, SortLatency)
	second := targets(rows)

	Sort(rows, SortLatency)
	if got := targets(rows); !equal(got, second) {
		t.Errorf("order changed between sorts: %v then %v (first was %v)", second, got, first)
	}
}

// TestCountriesCountsFromRows 验证国家统计直接来自结果行。
//
// 这一条保证"下拉框的数字"与"筛出来的行数"必然一致：两者都从
// 同一份 rows 算出来。
func TestCountriesCountsFromRows(t *testing.T) {
	rows := []Row{
		{Target: "a", CCA2: "US"},
		{Target: "b", CCA2: "us"}, // 大小写混用要合并
		{Target: "c", CCA2: "DE"},
		{Target: "d", CCA2: ""}, // 没有国家的行不参与统计
		{Target: "e", CCA2: "DE"},
	}

	counts := Countries(rows)
	if len(counts) != 2 {
		t.Fatalf("got %d countries, want 2: %+v", len(counts), counts)
	}
	if counts[0].CCA2 != "DE" || counts[0].Count != 2 {
		t.Errorf("first = %+v, want DE with 2", counts[0])
	}
	if counts[1].CCA2 != "US" || counts[1].Count != 2 {
		t.Errorf("second = %+v, want US with 2 (case-folded)", counts[1])
	}
}

func targets(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Target)
	}
	return out
}

// ---------------------------------------------------------------------------
// 同一目标的多行合并（展示用）
// ---------------------------------------------------------------------------

// TestCollapseByTargetMergesProbeAndTrace 验证探测行与跟踪行合并成一行。
//
// 这是展示层必须做的事：一个目标在 CSV 里有两行（探测一行有延迟、
// 跟踪一行有线路），直接展示会让人看到同一条目标出现两次、
// 其中一次"线路是空的"，看起来像跟踪失败了。
func TestCollapseByTargetMergesProbeAndTrace(t *testing.T) {
	rows := []Row{
		{Target: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, LatencyMS: 42.5, CCA2: "US"},
		{Target: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, HopCount: 25, ASPath: "CMNET > CMI", CCA2: "US"},
	}

	merged := CollapseByTarget(rows)
	if len(merged) != 1 {
		t.Fatalf("collapsed to %d rows, want 1", len(merged))
	}

	row := merged[0]
	// 延迟来自探测行，线路来自跟踪行——两者都要在。
	if row.LatencyMS != 42.5 {
		t.Errorf("LatencyMS = %v, want 42.5 (from the probe row)", row.LatencyMS)
	}
	if row.ASPath != "CMNET > CMI" || row.HopCount != 25 {
		t.Errorf("route lost: ASPath=%q HopCount=%d", row.ASPath, row.HopCount)
	}
}

// TestCollapseByTargetKeepsFastestLatency 验证多次测量取最快的那次。
//
// 使用者问"这个目标多快"时想知道的是它最快能到多少，
// 而不是最后一次碰巧多慢。
func TestCollapseByTargetKeepsFastestLatency(t *testing.T) {
	rows := []Row{
		{Target: "a:443", Success: true, LatencyMS: 300},
		{Target: "a:443", Success: true, LatencyMS: 25},
		{Target: "a:443", Success: true, LatencyMS: 120},
	}

	merged := CollapseByTarget(rows)
	if len(merged) != 1 {
		t.Fatalf("collapsed to %d rows, want 1", len(merged))
	}
	if merged[0].LatencyMS != 25 {
		t.Errorf("LatencyMS = %v, want 25 (the fastest)", merged[0].LatencyMS)
	}
}

// TestCollapseByTargetDropsErrorWhenAnyRowSucceeded 验证成功过就不留失败原因。
//
// 一行成功的结果旁边挂一句失败原因会自相矛盾：使用者会以为
// "这条通了但报了错"。失败原因只在**一次都没成功**时才有意义。
func TestCollapseByTargetDropsErrorWhenAnyRowSucceeded(t *testing.T) {
	rows := []Row{
		{Target: "a:443", Success: true, LatencyMS: 42},
		{Target: "a:443", Success: false, ErrorType: "timeout", ErrorMessage: "i/o timeout"},
	}

	merged := CollapseByTarget(rows)
	if len(merged) != 1 {
		t.Fatalf("collapsed to %d rows, want 1", len(merged))
	}
	if merged[0].ErrorType != "" || merged[0].ErrorMessage != "" {
		t.Errorf("kept a failure reason next to a successful measurement: %+v", merged[0])
	}
}

// TestCollapseByTargetKeepsErrorWhenAllRowsFailed 验证全失败时保留原因。
func TestCollapseByTargetKeepsErrorWhenAllRowsFailed(t *testing.T) {
	rows := []Row{
		{Target: "a:443", Success: false, ErrorType: "timeout", ErrorMessage: "i/o timeout"},
		{Target: "a:443", Success: false, ErrorType: "timeout", ErrorMessage: "i/o timeout"},
	}

	merged := CollapseByTarget(rows)
	if len(merged) != 1 {
		t.Fatalf("collapsed to %d rows, want 1", len(merged))
	}
	if merged[0].Success {
		t.Error("row marked successful although every measurement failed")
	}
	if merged[0].ErrorType != "timeout" {
		t.Errorf("ErrorType = %q, want timeout", merged[0].ErrorType)
	}
}

// TestCollapseByTargetKeepsDistinctTargets 验证不同目标不会被并到一起。
func TestCollapseByTargetKeepsDistinctTargets(t *testing.T) {
	rows := []Row{
		{Target: "a:443", Success: true, LatencyMS: 10},
		{Target: "b:443", Success: true, LatencyMS: 20},
		{Target: "a:8443", Success: true, LatencyMS: 30},
	}

	if merged := CollapseByTarget(rows); len(merged) != 3 {
		t.Errorf("collapsed %d rows into %d, want 3 distinct targets", len(rows), len(merged))
	}
}

func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

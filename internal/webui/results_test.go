package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
)

// 本文件覆盖"结果表格"与"跟踪区可选项"这两个视图接口的口径。
//
// 它们最容易出的错不是崩溃，而是**数字对不上**：使用者看到
// "美国 20 个"、表格里却只有 12 行，或者勾了一个国家却一个都跟不了。
// 这类问题没有任何报错，只能靠断言钉住。
//
// 请求走真实 HTTP（server.URL()）而不是直接调 handler：这样顺带覆盖了
// Host 必须是本机、token 必须正确这两道访问控制——直接调 handler 会
// 绕过它们，测试就少了一层真实约束。

// writeResultsCSV 写一份结果文件，行内容由调用方给出。
func writeResultsCSV(t *testing.T, rows []csvstore.Row) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "results.csv")
	store, err := csvstore.Open(csvstore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := store.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// fetchResults 调一次 /api/results 并解析响应。
//
// 用 path 查询参数指向测试自己写的那份 CSV，而不是依赖服务默认路径。
func fetchResults(t *testing.T, server *Server, csvPath, query string) resultsResponse {
	t.Helper()

	endpoint := server.URL() + "/api/results?path=" + url.QueryEscape(csvPath) + "&" + query
	status, body := request(t, http.MethodGet, endpoint, testToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}

	var response resultsResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	return response
}

func resultRowFor(target, cca2 string, latency float64, success bool) csvstore.Row {
	return csvstore.Row{
		Timestamp: time.Now().UTC(),
		Target:    target,
		IP:        "1.2.3.4",
		Port:      443,
		Success:   success,
		LatencyMS: latency,
		CCA2:      cca2,
	}
}

// TestResultsCountryFacetIgnoresFilter 验证国家清单**不随筛选收缩**。
//
// 这份清单是"可选项"：界面用它画下拉框让人挑国家。若先按 countries
// 过滤再统计，勾了美国之后清单里就只剩美国——想换一个国家也无从选起，
// 而那个国家明明还在数据里。
//
// 实测后果：前端为了拿到完整清单，不得不额外再发一次不带筛选的请求，
// 把后端的问题绕过去。接口给出的"可选项"本来就该是完整的。
func TestResultsCountryFacetIgnoresFilter(t *testing.T) {
	csvPath := writeResultsCSV(t, []csvstore.Row{
		resultRowFor("a:443", "US", 20, true),
		resultRowFor("b:443", "US", 30, true),
		resultRowFor("c:443", "DE", 40, true),
		resultRowFor("d:443", "JP", 0, false), // 失败行
	})

	server := newTestServer(t, csvPath)

	// 不带筛选：三个国家都在。
	all := fetchResults(t, server, csvPath, "sort=latency")
	if len(all.Countries) != 3 {
		t.Fatalf("unfiltered facet has %d countries, want 3: %+v", len(all.Countries), all.Countries)
	}
	if all.Total != 4 {
		t.Errorf("unfiltered total = %d, want 4", all.Total)
	}

	// 只筛美国：行只剩两行，但**清单仍然是三个国家**。
	us := fetchResults(t, server, csvPath, "sort=latency&countries=US")
	if us.Total != 2 {
		t.Errorf("filtered total = %d, want 2", us.Total)
	}
	if len(us.Countries) != 3 {
		t.Errorf("facet shrank with the filter: %d countries, want 3 "+
			"(the facet is a picker, not a subset)", len(us.Countries))
	}

	// 而且筛选本身要有用（别把筛选一起改坏）。
	if len(us.Rows) != 2 {
		t.Errorf("filtered rows = %d, want 2", len(us.Rows))
	}
	for _, row := range us.Rows {
		if row.CCA2 != "US" {
			t.Errorf("filter leaked a %s row", row.CCA2)
		}
	}
}

// TestResultsCollapsesProbeAndTraceRows 验证同一目标的探测行与跟踪行
// 合并成一行后展示。
//
// 不合并的话，被跟踪过的目标会与"线路为空"的那一行并列出现，
// 看起来像跟踪失败了。
func TestResultsCollapsesProbeAndTraceRows(t *testing.T) {
	csvPath := writeResultsCSV(t, []csvstore.Row{
		resultRowFor("a:443", "US", 20, true),
		{
			Timestamp: time.Now().UTC(),
			Target:    "a:443",
			IP:        "1.2.3.4",
			Port:      443,
			Success:   true,
			ASPath:    "CMNET > CMI",
			HopCount:  20,
			CCA2:      "US",
		},
	})

	server := newTestServer(t, csvPath)
	response := fetchResults(t, server, csvPath, "sort=latency")

	if response.Total != 1 {
		t.Fatalf("total = %d, want 1 (probe and trace rows merged)", response.Total)
	}
	row := response.Rows[0]
	if row.LatencyMS != 20 {
		t.Errorf("latency = %v, want 20 (from the probe row)", row.LatencyMS)
	}
	if row.ASPath != "CMNET > CMI" {
		t.Errorf("route = %q, want the trace row's route", row.ASPath)
	}
	if len(response.Countries) != 1 || response.Countries[0].Count != 1 {
		t.Errorf("countries = %+v, want US with 1", response.Countries)
	}
}

// TestResultsMissingFileIsNotAnError 验证"还没测过"不是错误。
//
// 界面据此显示"还没有结果"，而不是弹一个红色报错。
func TestResultsMissingFileIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.csv")

	server := newTestServer(t, missing)
	response := fetchResults(t, server, missing, "sort=latency")

	if response.Exists {
		t.Error("exists = true for a missing file")
	}
	if len(response.Rows) != 0 {
		t.Errorf("rows = %d, want 0", len(response.Rows))
	}
}

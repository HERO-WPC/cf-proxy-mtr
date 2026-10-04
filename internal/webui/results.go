package webui

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/csvstore"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

// 本文件是"选国家 -> 测 TCP -> 挑一批 -> 跟踪"这条流程的接口。
//
// == 为什么跟踪是单独一次请求 ==
//
// 使用者要先看到延迟才能决定跟踪谁。做成"扫描时顺手全跟踪"就没得选了：
// 一个国家的目标可能上千，而一次 traceroute 实测十几秒。
//
// 拆开还有个实际好处：TCP 结果已经落盘，跟踪这一步崩了、被停了、
// 或者使用者改主意了，延迟数据都不丢——它是从 CSV 里读出来的。
//
// == 为什么只传"筛选条件"而不是目标列表 ==
//
// CSV 是本项目唯一的数据源，界面显示的也是它。让界面把选中的目标回传，
// 等于承认"界面手里那份"才是真相；两边一旦不同步（开着旧页面、
// 中途又跑了一轮），就会去跟踪一批已经不在文件里的目标。
//
// 因此接口只收条件，由服务端从文件里现算；界面预览用的是**同一段
// 选取逻辑**，所以"预览 12 个"与"实际跟踪 12 个"必然一致。

// maxResultRows 是接口一次最多返回多少行。
//
// 上限存在的理由很实际：一次全量扫描可以有 1.5 万行，全塞进 JSON
// 会让浏览器卡住好几秒。界面按这个上限做"显示前 N 行 + 共 M 行"，
// 而**排序与统计仍在服务端对全部行做**，因此数字是准的。
const maxResultRows = 2000

// handleCountries 返回目标列表里的国家分布。
func (s *Server) handleCountries(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	counts, total, err := s.cfg.Service.TargetCountries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":     total,
		"countries": counts,
	})
}

// handleResults 读结果 CSV 并返回排序后的行。
func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = s.defaultCSVPath()
	}

	rows, err := csvstore.ReadAll(path)
	if err != nil {
		// 文件不存在不是错误：还没跑过扫描而已。
		// 界面据此显示"还没有结果"，而不是弹一个红色报错。
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusOK, resultsResponse{
				Path:      path,
				Exists:    false,
				Rows:      []resultRow{},
				Countries: []csvstore.CountryCount{},
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	countries := splitList(r.URL.Query().Get("countries"))
	successOnly := r.URL.Query().Get("success_only") == "true"

	// 先把同一个目标的探测行与跟踪行合并。
	//
	// 一个目标在文件里有两行（探测一行有延迟、跟踪一行有线路），
	// 直接展示会让人看到同一条目标出现两次、其中一次"线路是空的"，
	// 看起来像跟踪失败了。合并只影响这个接口的视图，
	// CSV 文件里一行不少。
	rows = csvstore.CollapseByTarget(rows)

	// 国家分布必须**在应用筛选之前**统计。
	//
	// 它是一份"可选项清单"：界面用它画下拉框，让人从里面挑国家。
	// 若先按 countries 过滤再统计，勾了美国之后清单里就只剩美国——
	// 想换一个国家也无从选起（那个国家还在数据里，只是被筛掉了）。
	//
	// 实测后果：前端为了拿到完整清单，不得不额外再发一次不带筛选的
	// 请求，把后端的问题绕过去。接口给出的"可选项"就该是完整的。
	facet := csvstore.Countries(rows)

	filtered := make([]csvstore.Row, 0, len(rows))
	for _, row := range rows {
		if len(countries) > 0 && !containsFold(countries, row.CCA2) {
			continue
		}
		if successOnly && !row.Success {
			continue
		}
		filtered = append(filtered, row)
	}

	// 排序在**截断之前**做：只对前 2000 行排序会让"最快的那个"
	// 取决于文件顺序，那是错的。
	csvstore.Sort(filtered, sortByFrom(r.URL.Query().Get("sort")))

	response := resultsResponse{
		Path:      path,
		Exists:    true,
		Total:     len(filtered),
		Countries: facet,
		Rows:      make([]resultRow, 0, min(len(filtered), maxResultRows)),
	}
	for i, row := range filtered {
		if i == maxResultRows {
			break
		}
		response.Rows = append(response.Rows, toResultRow(row))
	}

	writeJSON(w, http.StatusOK, response)
}

// handleTracePreview 返回"按当前条件会跟踪哪些目标"。
func (s *Server) handleTracePreview(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = s.defaultCSVPath()
	}

	preview, err := service.PreviewTraceSelection(
		path,
		splitList(r.URL.Query().Get("countries")),
		floatQuery(r.URL.Query().Get("max_latency_ms")),
		intQuery(r.URL.Query().Get("limit")),
	)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 还没有 CSV：预览为 0，界面会禁用按钮并说明原因。
			writeJSON(w, http.StatusOK, service.SelectionPreview{
				Countries: []csvstore.CountryCount{},
				Targets:   []string{},
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, preview)
}

// traceRequest 是开始跟踪的请求体。
type traceRequest struct {
	OutputPath   string   `json:"output_path"`
	Countries    []string `json:"countries"`
	MaxLatencyMS float64  `json:"max_latency_ms"`
	Limit        int      `json:"limit"`

	TraceMode      string `json:"trace_mode"`
	TraceWorkers   int    `json:"trace_workers"`
	TraceTimeoutS  int    `json:"trace_timeout_s"`
	DataProvider   string `json:"data_provider"`
	PowProvider    string `json:"pow_provider"`
	NoASNPrefix    bool   `json:"no_asn_prefix"`
	NoAutoDownload bool   `json:"no_auto_download"`
}

// handleTrace 从已有结果里挑一批目标做跟踪。
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req traceRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 与扫描共用同一个"忙"标志：跟踪也在写同一个 CSV，
	// 两者同时跑会把文件搞成两批交错的行。
	s.scanMu.Lock()
	if s.scanBusy {
		s.scanMu.Unlock()
		writeError(w, http.StatusConflict, "another scan or trace is already running")
		return
	}
	s.scanBusy = true
	s.lastResult = nil
	s.hub.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	s.scanCancel = cancel
	s.scanMu.Unlock()

	s.cfg.Logger.Info("webui: trace requested",
		"countries", req.Countries, "max_latency_ms", req.MaxLatencyMS,
		"limit", req.Limit, "output", req.OutputPath)

	go s.runTrace(ctx, req, cancel)

	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// runTrace 在后台执行跟踪。
func (s *Server) runTrace(ctx context.Context, req traceRequest, cancel context.CancelFunc) {
	defer cancel()

	// 跟踪要**追加到当前结果文件**，而不是新建一份。
	//
	// 这里刻意不用 next()：跟踪是在给刚测出的那份结果补线路信息，
	// 写成另一个文件会让结果表格突然找不到自己的线路——而且使用者
	// 看着"跟踪成功了"却看不到任何变化。
	path := strings.TrimSpace(req.OutputPath)
	if path == "" {
		path = s.defaultCSVPath()
	} else {
		s.resultsFile.set(path)
	}

	started := time.Now().UTC()

	opts := service.TraceSelectionOptions{
		CSVPath:      path,
		Countries:    req.Countries,
		MaxLatencyMS: req.MaxLatencyMS,
		Limit:        req.Limit,
		TraceConfig: service.TraceOptions{
			Mode:         strings.TrimSpace(req.TraceMode),
			DataProvider: strings.TrimSpace(req.DataProvider),
			PowProvider:  strings.TrimSpace(req.PowProvider),
			// 图形界面默认自动下载；勾选框可以关掉。
			AutoDownload: !req.NoAutoDownload,
			Workers:      req.TraceWorkers,
			Timeout:      durationSeconds(req.TraceTimeoutS),
		},
		NoASNPrefix: req.NoASNPrefix,
		Progress:    s.emitCSVProgress,
		OnTrace:     s.emitTrace,
	}

	result, err := s.cfg.Service.RunTraceSelection(ctx, opts)

	// 与 runScan 用同一套结局结构：界面只需要认一种。
	outcome := &scanOutcome{StartedAt: started, FinishedAt: time.Now().UTC()}
	if err != nil {
		outcome.Error = err.Error()
		outcome.Kind = classify(err)
		s.cfg.Logger.Error("webui: trace failed", "kind", outcome.Kind, "error", err)
	} else {
		summary := summarizeCSVScan(result)
		outcome.Summary = &summary
		s.cfg.Logger.Info("webui: trace finished",
			"csv", summary.OutputPath, "traced", summary.Traced, "rows", summary.RowsWritten)
	}

	// 结局先发事件再落进 lastResult：反过来的话，界面收到事件后
	// 立刻查询时可能还没看到新结局。
	s.emitScanEnd(outcome)

	s.scanMu.Lock()
	s.lastResult = outcome
	s.scanBusy = false
	s.scanCancel = nil
	s.scanMu.Unlock()

	// 清掉"当前目标"：跟踪结束后它已经没有意义。
	s.currentMu.Lock()
	s.currentTarget = ""
	s.currentMu.Unlock()
}

// defaultCSVPath 返回**当前**结果文件的路径。
//
// 调用方没给 path 时用它。一轮都没跑过时返回一个尚不存在的名字
// （读取接口会如实报告 exists=false），而不是回落到一个固定名字——
// 固定名字会让"读哪一份"取决于上一次是谁写的。
func (s *Server) defaultCSVPath() string {
	return s.resultsFile.current(ResultsDir(s.cfg.DefaultCSVPath))
}

// resultsResponse 是 /api/results 的响应。
type resultsResponse struct {
	Path      string                  `json:"path"`
	Exists    bool                    `json:"exists"`
	Rows      []resultRow             `json:"rows"`
	Countries []csvstore.CountryCount `json:"countries"`
	Total     int                     `json:"total"`
}

// resultRow 是给界面的一行。
//
// 与 csvstore.Row 分开：CSV 的字段名是给表格软件看的（也是契约），
// 而这里的字段名是给前端看的，两者独立演进才不会互相牵制。
type resultRow struct {
	Timestamp    string  `json:"timestamp"`
	Target       string  `json:"target"`
	IP           string  `json:"ip"`
	Port         int     `json:"port"`
	Success      bool    `json:"success"`
	LatencyMS    float64 `json:"latency_ms"`
	ErrorType    string  `json:"error_type"`
	ErrorMessage string  `json:"error_message"`
	HopCount     int     `json:"hop_count"`
	ASPath       string  `json:"as_path"`
	Hops         string  `json:"hops"`
	CCA2         string  `json:"cca2"`
}

func toResultRow(row csvstore.Row) resultRow {
	timestamp := ""
	if !row.Timestamp.IsZero() {
		timestamp = row.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
	}
	return resultRow{
		Timestamp:    timestamp,
		Target:       row.Target,
		IP:           row.IP,
		Port:         row.Port,
		Success:      row.Success,
		LatencyMS:    row.LatencyMS,
		ErrorType:    row.ErrorType,
		ErrorMessage: row.ErrorMessage,
		HopCount:     row.HopCount,
		ASPath:       row.ASPath,
		Hops:         row.Hops,
		CCA2:         row.CCA2,
	}
}

func sortByFrom(raw string) csvstore.SortBy {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "route":
		return csvstore.SortRoute
	case "target":
		return csvstore.SortTarget
	default:
		return csvstore.SortLatency
	}
}

// splitList 解析逗号分隔的查询参数。
func splitList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// containsFold 是不区分大小写的成员判断。
func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func floatQuery(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return value
}

func intQuery(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return value
}

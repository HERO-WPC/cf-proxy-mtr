package webui

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

// handleEventsLog 用 SSE 推送界面日志事件。
//
// 与 /api/events（进度）分开：
//
//	/api/events      进度条用：完成数 / 总数，覆盖式更新
//	/api/events/log  日志面板用：一条条的事件，只追加
//
// 分开的理由是重连语义不同：进度是"最新状态"，断线后拿最新的就够；
// 日志是"发生过什么"，重连后需要补齐，因此这里会先补历史。
func (s *Server) handleEventsLog(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, cancel := s.events.Subscribe()
	defer cancel()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	// 先送一条自己的连接事件，让用户看到面板是活的。
	if !writeSSE(w, "log", uiEvent{
		Time:  time.Now().UTC().Format(time.RFC3339),
		Level: levelInfo,
		Text:  "已连接到日志流",
	}) {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return

		case event, open := <-events:
			if !open {
				return
			}
			if !writeSSE(w, "log", event) {
				return
			}
			flusher.Flush()

		case <-ticker.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleLog 返回最近的日志事件（供页面初始加载或 SSE 不可用时兜底）。
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": s.events.Recent(200)})
}

// ---------------------------------------------------------------------------
// 扫描过程中的界面事件
// ---------------------------------------------------------------------------

// emitScanStart 记录"开始扫描"以及这次扫描的关键参数。
func (s *Server) emitScanStart(req scanRequest) {
	limit := "全部目标"
	if req.Limit > 0 {
		limit = fmt.Sprintf("前 %d 个目标", req.Limit)
	}
	workers := req.Workers
	if workers <= 0 {
		workers = 100
	}
	timeout := req.TimeoutMS
	if timeout <= 0 {
		timeout = 3000
	}

	out := req.OutputPath
	if out == "" {
		out = "(默认路径)"
	}
	s.events.Info("开始测量：%s，并发 %d，超时 %dms，结果写入 %s",
		limit, workers, timeout, out)
	if req.Trace {
		s.events.Info("线路跟踪：已启用（模式 %s）", traceModeOrDefault(req.TraceMode))
	} else {
		s.events.Info("线路跟踪：未启用")
	}
}

// emitLoadResult 记录目标列表的加载结果。
func (s *Server) emitLoadResult(result *service.CSVScanResult) {
	s.events.Info("目标列表：%d 个待测（来源 %s，%s）",
		result.Targets, result.SourceURL, originLabel(result.SourceFromCache))
}

// emitTarget 记录"正在测哪个目标"。
//
// 它会被多个 worker **并发**调用，因此这里只做一件事：
// 缓存当前目标（供 /api/scan/status 返回，页面刷新时靠它恢复显示）。
//
// **刻意不写日志行**：每个目标的结果由 emitProbe 输出一行带延迟与
// 连通性的记录。这里再写一条"正在测量 X"会让日志每个目标两行，
// 其中一行还没有任何结果信息。参数是字符串而不是 model.Target：
// 界面层不需要拿到整个模型类型。
func (s *Server) emitTarget(target string) {
	s.currentMu.Lock()
	s.currentTarget = target
	s.currentMu.Unlock()
}

// emitProbe 输出一条"这个 IP 通不通、多少毫秒、在哪儿"的日志。
//
// 这是使用者最常看的一行：TCP 延迟测试的产出就是这些。
// 落地地区（目标 IP 自身的地理位置）必须带上——同一个 IP 段在不同
// 地区表现差别很大，没有地区就无法判断"延迟高"是不是因为目标远。
func (s *Server) emitProbe(outcome service.ProbeOutcome) {
	location := landingSuffix(outcome.Landing)

	if outcome.Success {
		s.events.Emit(levelGood, fmt.Sprintf("连通 %s  %.1f ms%s",
			outcome.Target, outcome.LatencyMS, location))
		return
	}

	// 失败时给**分类**（timeout / refused / ...）而不是整条错误信息：
	// 分类足够说明问题，而完整信息里带着重复的目标地址，逐行看很吵。
	// 完整原因在 CSV 的 error_message 列里。
	reason := outcome.ErrorType
	if reason == "" {
		reason = "failed"
	}
	s.events.Warn("不通 %s  (%s)%s", outcome.Target, reason, location)
}

// emitTrace 输出一条线路跟踪结果。
//
// 线路串**同时带 ASN 编号与线路名称**（如 "163(AS4134) > CN2(AS4809)"）：
// 只有名称无法核对编号，只有编号又认不出这条线路意味着什么。
// 同样带上落地地区，这样"走优质出口到美国"这类判断才成立。
//
// 会被多个 worker 并发调用；events 广播器自身是线程安全的。
func (s *Server) emitTrace(outcome service.TraceOutcome) {
	location := landingSuffix(outcome.Landing)

	if !outcome.Success {
		reason := outcome.ErrorMessage
		if reason == "" {
			reason = outcome.ErrorType
		}
		s.events.Warn("线路跟踪 %s 失败：%s%s", outcome.Target, reason, location)
		return
	}

	if outcome.Route == "" {
		// 所有跳都没有已知 ASN：如实说"没识别出来"，
		// 而不是显示一个空白的"线路："。
		s.events.Info("线路 %s：%d 跳（未识别出已知骨干线路）%s",
			outcome.Target, outcome.HopCount, location)
		return
	}

	s.events.Emit(levelGood, fmt.Sprintf("线路 %s：%d 跳，%s%s",
		outcome.Target, outcome.HopCount, outcome.Route, location))
}

// landingSuffix 把落地地区格式化成日志行尾的补充说明。
//
// 没有数据时返回空串而不是 "落地 -"：日志里出现一个空值标注
// 只增加噪声，不提供信息。
func landingSuffix(location string) string {
	trimmed := strings.TrimSpace(location)
	if trimmed == "" || trimmed == "-" {
		return ""
	}
	return "  落地 " + trimmed
}

// emitCSVProgress 把扫描进度翻译成日志行。
//
// 刻意**不**每个进度回调都写一行：那会让面板被"完成 37/100"刷满，
// 反而看不到别的东西。只在阶段完成时写一行，中间的进度由进度条表达。
func (s *Server) emitCSVProgress(event service.ProgressEvent) {
	// 推给进度条：hub 自己保存最近一次，页面刷新后能恢复。
	s.hub.Publish(event)

	if event.Total > 0 && event.Completed == event.Total {
		name := phaseLabel(event.Phase)
		if event.Phase == "probe" {
			s.events.Good("%s完成：%d 个目标，成功 %d，失败 %d",
				name, event.Completed, event.Success, event.Failed)
		} else {
			s.events.Good("%s完成：%d 条线路", name, event.Completed)
		}
	}
}

// emitScanEnd 记录扫描结束。
func (s *Server) emitScanEnd(outcome *scanOutcome) {
	if outcome == nil {
		return
	}
	if outcome.Error != "" {
		s.events.Error("测量失败：%s", outcome.Error)
		return
	}

	summary := outcome.Summary
	if summary == nil {
		s.events.Info("测量结束")
		return
	}

	s.events.Good("测量完成：用时 %.1fs，成功 %d，失败 %d，写入 %d 行",
		summary.ElapsedSecs, summary.Succeeded, summary.Failed, summary.RowsWritten)

	if summary.Traced > 0 {
		s.events.Good("线路跟踪完成：%d 条（成功 %d）", summary.Traced, summary.TracedOK)
	}
	if summary.TraceUnavailable != "" {
		s.events.Warn("线路跟踪被跳过：%s", summary.TraceUnavailable)
	}
	if summary.Interrupted {
		s.events.Warn("测量被中断：已经测完的部分都已写入 %s，不会丢", summary.OutputPath)
	}
	if summary.WriteErrors > 0 {
		s.events.Error("有 %d 次写入失败，请检查磁盘空间与文件权限", summary.WriteErrors)
	}

	s.events.Info("结果文件：%s", summary.OutputPath)
}

// phaseLabel 返回阶段的显示名。
func phaseLabel(phase string) string {
	switch phase {
	case "probe":
		return "TCP 探测"
	case "trace":
		return "线路跟踪"
	default:
		return phase
	}
}

// traceModeOrDefault 返回跟踪模式的显示值。
func traceModeOrDefault(mode string) string {
	if mode == "" {
		return "tcp"
	}
	return mode
}

// originLabel 描述目标列表来自缓存还是网络。
func originLabel(fromCache bool) string {
	if fromCache {
		return "本地缓存"
	}
	return "网络"
}

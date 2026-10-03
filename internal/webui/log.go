package webui

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/scheduler"
	"github.com/cf-route-tester/cf-route-tester/internal/service"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
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
	mode := "新会话"
	if req.Resume {
		mode = "继续未完成的会话"
	}
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

	s.events.Info("开始测量：%s，%s，并发 %d，超时 %dms", mode, limit, workers, timeout)
	if req.Trace {
		s.events.Info("线路跟踪：已启用（模式 %s）", traceModeOrDefault(req.TraceMode))
	} else {
		s.events.Info("线路跟踪：未启用")
	}
}

// emitLoadResult 记录目标列表的加载结果。
func (s *Server) emitLoadResult(result *service.ScanResult) {
	s.events.Info("目标列表：%d 个待测（来源 %s，%s）",
		result.TargetsConsidered, result.SourceURL, originLabel(result.SourceFromCache))
}

// emitTarget 记录"正在测哪个目标"。
//
// 它会被多个 worker **并发**调用，因此这里只做两件事：
// 把消息转给线程安全的 broker，并缓存当前目标
// （供 /api/scan/status 返回，页面刷新时靠它恢复显示）。
func (s *Server) emitTarget(target model.Target) {
	s.currentMu.Lock()
	s.currentTarget = target.ID
	s.currentMu.Unlock()

	s.events.Emit(levelInfo, "正在测量 "+target.ID)
}

// emitTrace 记录一条已完成线路的**线路信息**。
//
// 这是"AS4134 → 163"这类信息出现的地方：光有 ASN 编号
// 对使用者没有意义，配上线路名称才知道走的是普通出口还是优质出口。
func (s *Server) emitTrace(target model.Target, result *trace.TraceResult) {
	if result == nil {
		return
	}

	if !result.Success {
		reason := result.ErrorMessage
		if reason == "" {
			reason = string(result.ErrorType)
		}
		s.events.Warn("线路跟踪 %s 失败：%s", target.ID, reason)
		return
	}

	route := asnmap.ShortPath(hopASNs(result.Hops))
	if route == "" {
		// 所有跳都没有已知 ASN：如实说"没识别出来"，
		// 而不是显示一个空白的"线路："。
		s.events.Info("线路 %s：%d 跳（未识别出已知骨干线路）", target.ID, result.HopCount())
		return
	}

	s.events.Emit(levelGood, fmt.Sprintf("线路 %s：%d 跳，%s",
		target.ID, result.HopCount(), route))
}

// emitProgressEvent 把调度器进度翻译成日志行。
//
// 刻意**不**每个进度回调都写一行：进度回调按间隔触发，
// 每次都写会让面板被"完成 37/100"刷满，反而看不到别的东西。
// 只在阶段完成时写一行，中间的进度由进度条本身表达。
func (s *Server) emitProgressEvent(event scheduler.ProgressEvent) {
	s.hub.Publish(event)

	if event.Total > 0 && event.Completed == event.Total {
		name := phaseLabel(event.Phase)
		s.events.Good("%s完成：%d 个目标，成功 %d，失败 %d",
			name, event.Completed, event.Success, event.Failed)
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

	s.events.Good("测量完成：用时 %.1fs，成功 %d，失败 %d，落库 %d 条",
		summary.ElapsedSeconds, summary.ProbeSuccess, summary.ProbeFailed, summary.Stored)

	if summary.TraceStored > 0 {
		s.events.Good("线路跟踪完成：落库 %d 条", summary.TraceStored)
	}
	if summary.TraceSkipped {
		s.events.Warn("线路跟踪被跳过：%s", summary.TraceUnavailable)
	}
	if summary.Interrupted {
		s.events.Warn("测量被中断：已测量的部分已保存，可以继续这个会话")
	}
}

// phaseLabel 返回阶段的显示名。
func phaseLabel(phase scheduler.Phase) string {
	switch phase {
	case scheduler.PhaseProbe:
		return "TCP 探测"
	case scheduler.PhaseTrace:
		return "线路跟踪"
	default:
		return string(phase)
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

// hopASNs 把逐跳的 ASN 抽成一个字符串切片。
//
// asnmap 不依赖 trace 包（否则任何用到它的地方都会被拖上
// 那条依赖链），因此这里做一次转换。
func hopASNs(hops []trace.Hop) []string {
	out := make([]string, 0, len(hops))
	for _, hop := range hops {
		out = append(out, hop.ASN)
	}
	return out
}

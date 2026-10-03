package webui

import (
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

// 本文件锁定**日志行的内容契约**。
//
// 三条要求来自使用者，都很具体，而且都不是"类型正确"能保证的：
//
//  1. TCP 延迟测试的日志要有**每个 IP 的延迟与连通性**；
//  2. 跟踪的日志要有 **ASN 编号 + 线路名称**（只有名称无法核对编号，
//     只有编号又认不出线路）；
//  3. 都要带上**落地地区**（目标 IP 自身的位置），
//     否则"延迟高"到底是因为目标远还是线路差就说不清。
//
// 这些是纯格式契约，因此直接调格式化入口，不需要真扫描。

// TestEmitProbeShowsLatencyConnectivityAndLanding 验证成功行的内容。
func TestEmitProbeShowsLatencyConnectivityAndLanding(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	server.emitProbe(service.ProbeOutcome{
		Target:    "1.2.3.4:443",
		IP:        "1.2.3.4",
		Port:      443,
		Landing:   "US/Illinois/Chicago",
		Success:   true,
		LatencyMS: 285.44,
	})

	text := lastEventText(t, server)

	for _, want := range []string{"1.2.3.4:443", "连通", "285.4", "ms", "落地", "US/Illinois/Chicago"} {
		if !strings.Contains(text, want) {
			t.Errorf("probe log line is missing %q: %q", want, text)
		}
	}
}

// TestEmitProbeFailureShowsReasonAndLanding 验证失败行给出分类。
//
// 用**分类**（timeout）而不是完整错误信息：完整信息里带着重复的
// 目标地址，逐行看很吵；分类足以说明问题，细节在 CSV 里。
func TestEmitProbeFailureShowsReasonAndLanding(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	server.emitProbe(service.ProbeOutcome{
		Target:       "1.2.3.4:443",
		Landing:      "US/Texas/Dallas",
		Success:      false,
		ErrorType:    "timeout",
		ErrorMessage: "dial tcp4 1.2.3.4:443: i/o timeout",
	})

	text := lastEventText(t, server)

	for _, want := range []string{"1.2.3.4:443", "不通", "timeout", "落地", "US/Texas/Dallas"} {
		if !strings.Contains(text, want) {
			t.Errorf("failure log line is missing %q: %q", want, text)
		}
	}
}

// TestEmitTraceShowsASNAndRouteName 验证线路行同时含 ASN 编号与线路名。
//
// 这是第 2 条要求：两者缺一不可。
func TestEmitTraceShowsASNAndRouteName(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	server.emitTrace(service.TraceOutcome{
		Target:   "1.2.3.4:443",
		Landing:  "US/Illinois/Elk Grove Village",
		HopCount: 25,
		Route:    "CMNET(AS9808) > CMI(AS58453) > Cogent(AS174)",
		Success:  true,
	})

	text := lastEventText(t, server)

	for _, want := range []string{
		"1.2.3.4:443",
		"25 跳",
		// 线路名称
		"CMNET", "CMI", "Cogent",
		// 对应的 ASN 编号
		"AS9808", "AS58453", "AS174",
		// 落地地区
		"落地", "US/Illinois/Elk Grove Village",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("trace log line is missing %q: %q", want, text)
		}
	}
}

// TestEmitTraceWithoutRouteSaysSo 验证识别不出线路时如实说明。
func TestEmitTraceWithoutRouteSaysSo(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	server.emitTrace(service.TraceOutcome{
		Target:   "1.2.3.4:443",
		Landing:  "US/Illinois/Chicago",
		HopCount: 12,
		Success:  true,
	})

	text := lastEventText(t, server)

	if !strings.Contains(text, "未识别出已知骨干线路") {
		t.Errorf("log should say the route was not recognised: %q", text)
	}
	// 不该显示一个空白的"线路："。
	if strings.Contains(text, "，\n") || strings.HasSuffix(strings.TrimSpace(text), "，") {
		t.Errorf("log line ends with a dangling comma: %q", text)
	}
}

// TestEmitTraceFailureShowsReason 验证跟踪失败给出原因。
func TestEmitTraceFailureShowsReason(t *testing.T) {
	server := newTestServer(t, writeCache(t, listenLocal(t)))

	server.emitTrace(service.TraceOutcome{
		Target:       "1.2.3.4:443",
		Landing:      "US/Illinois/Chicago",
		Success:      false,
		ErrorType:    "timeout",
		ErrorMessage: "nexttrace timed out",
	})

	text := lastEventText(t, server)

	for _, want := range []string{"线路跟踪", "1.2.3.4:443", "失败", "nexttrace timed out"} {
		if !strings.Contains(text, want) {
			t.Errorf("trace failure line is missing %q: %q", want, text)
		}
	}
}

// TestLandingSuffixOmitsEmpty 验证没有地区数据时不产生空标注。
//
// 日志里出现"落地 -"只是噪声：它占位但不提供信息。
func TestLandingSuffixOmitsEmpty(t *testing.T) {
	for _, input := range []string{"", "   ", "-"} {
		if got := landingSuffix(input); got != "" {
			t.Errorf("landingSuffix(%q) = %q, want empty", input, got)
		}
	}
	if got := landingSuffix("US/Illinois/Chicago"); got != "  落地 US/Illinois/Chicago" {
		t.Errorf("landingSuffix produced %q", got)
	}
}

// lastEventText 返回最近一条日志事件的文本。
func lastEventText(t *testing.T, server *Server) string {
	t.Helper()

	events := server.events.Recent(0)
	if len(events) == 0 {
		t.Fatal("no log event was emitted")
	}
	return events[len(events)-1].Text
}

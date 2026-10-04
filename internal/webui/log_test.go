package webui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/service"
)

// 本文件覆盖"优质线路的日志行被特别标出"。
//
// 关键在于**判定发生在服务端**：线路是在 service 里解析出来的
// （手里就是 ASN 列表），界面只需要照着 outcome.Premium 标。
// 若让界面去解析日志文本，日志格式一改就会悄悄失效——而且失效后
// 的表现只是"某些行不再变红"，没有人会去核对。

// TestHighlightReachesTheClient 验证高亮标记真的出现在发给界面的 JSON 里。
//
// 后端把字段填进了结构体，不等于它会被序列化出去：少一个 tag、
// 名字拼错、或者被 omitempty 吞掉，界面就永远收不到这个信号——
// 而表现只是"某些行没有变红"，看不出是哪一层断的。
func TestHighlightReachesTheClient(t *testing.T) {
	highlighted := uiEvent{Level: levelGood, Text: "线路 1.2.3.4:443：20 跳，CN2(AS4809)", Highlight: true}

	raw, err := json.Marshal(highlighted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"highlight":true`) {
		t.Errorf("highlighted event does not carry the flag: %s", raw)
	}

	// 普通行不必带这个字段：每一行都写 highlight:false 只是噪声。
	plain := uiEvent{Level: levelGood, Text: "线路 1.2.3.4:443：15 跳，Cogent(AS174)"}
	raw, err = json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "highlight") {
		t.Errorf("plain event carries a highlight field: %s", raw)
	}
}

// TestPremiumRouteIsHighlighted 验证走了优质线路的那一行带高亮标记。
func TestPremiumRouteIsHighlighted(t *testing.T) {
	server := newTestServer(t, "")

	server.emitTrace(service.TraceOutcome{
		Target:   "1.2.3.4:443",
		Landing:  "US/California/San Jose",
		HopCount: 20,
		Route:    "CMNET(AS56046) > CN2(AS4809) > Cloudflare(AS13335)",
		Premium:  true,
		Success:  true,
	})

	events := server.events.Recent(1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	got := events[0]
	if !got.Highlight {
		t.Error("Highlight = false for a CN2 route")
	}
	// 级别仍然是 good：高亮与级别正交，"走了好线路"不是一种严重程度。
	if got.Level != levelGood {
		t.Errorf("Level = %q, want %q", got.Level, levelGood)
	}
}

// TestOrdinaryRouteIsNotHighlighted 验证普通线路不标。
//
// 标得太多等于没标：一屏全红之后使用者就再也不看颜色了。
func TestOrdinaryRouteIsNotHighlighted(t *testing.T) {
	server := newTestServer(t, "")

	server.emitTrace(service.TraceOutcome{
		Target:   "1.2.3.4:443",
		HopCount: 15,
		Route:    "CMNET(AS56046) > Cogent(AS174) > Vultr(AS20473)",
		Success:  true,
	})

	events := server.events.Recent(1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Highlight {
		t.Errorf("Highlight = true for an ordinary route: %s", events[0].Text)
	}
}

// TestFailedTraceIsNotHighlighted 验证**失败**的行不会被高亮。
//
// 失败的 outcome 里 Route 可能是空的、也可能带着"已经走到的那一段"，
// 此时标红会让人以为"这条路走了好线路"，而实际上这次跟踪根本没成。
// 失败本身有它自己的级别（warn），不需要再叠标记。
func TestFailedTraceIsNotHighlighted(t *testing.T) {
	server := newTestServer(t, "")

	server.emitTrace(service.TraceOutcome{
		Target:       "1.2.3.4:443",
		Route:        "CN2(AS4809)",
		Premium:      true,
		Success:      false,
		ErrorType:    "timeout",
		ErrorMessage: "trace timed out",
	})

	events := server.events.Recent(1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0]
	if got.Highlight {
		t.Errorf("Highlight = true for a FAILED trace: %s", got.Text)
	}
	if got.Level != levelWarn {
		t.Errorf("Level = %q, want %q for a failed trace", got.Level, levelWarn)
	}
}

// TestUnidentifiedRouteIsNotHighlighted 验证"没识别出线路"的行不会被高亮，
// 而且不是空的"线路："。
func TestUnidentifiedRouteIsNotHighlighted(t *testing.T) {
	server := newTestServer(t, "")

	server.emitTrace(service.TraceOutcome{
		Target:   "1.2.3.4:443",
		HopCount: 12,
		Route:    "",
		Success:  true,
	})

	events := server.events.Recent(1)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Highlight {
		t.Error("Highlight = true although no route was identified")
	}
}

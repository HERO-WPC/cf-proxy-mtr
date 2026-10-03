package trace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// ---------------------------------------------------------------------------
// 真实输出夹具
// ---------------------------------------------------------------------------

// realFixturePath 是 NextTrace v1.7.3 的**真实**输出（ICMP 模式，本机实测）。
//
// 用它而不是手写 JSON 的原因：这个文件的价值就在于它是真的。
// 我最初凭文档猜字段名，猜错了三处（大写 Hops、二维数组、RTT 单位），
// 这个夹具保证那些错误不会再回来。
const realFixturePath = "testdata/nexttrace_v1.7.3_icmp.json"

func loadRealFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(realFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// ---------------------------------------------------------------------------
// 对照真实输出的解析测试
// ---------------------------------------------------------------------------

// TestParseRealNextTraceOutput 用真实输出锁定解析契约。
func TestParseRealNextTraceOutput(t *testing.T) {
	parsed, err := ParseNextTraceJSON(loadRealFixture(t))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}

	// 外层数组是 30 个 TTL，每个含 3 次探测；必须聚合成 30 跳，而不是 90 跳。
	if len(parsed.Hops) != 30 {
		t.Fatalf("hops = %d, want 30 (each TTL is one hop, not one per probe)", len(parsed.Hops))
	}

	// 引擎停止原因必须是可读的字符串，而不是整个对象。
	if parsed.StopReason != "max_hops" {
		t.Errorf("StopReason = %q, want max_hops", parsed.StopReason)
	}
	if parsed.StopHop != 30 {
		t.Errorf("StopHop = %d, want 30", parsed.StopHop)
	}
	if parsed.TraceMapURL == "" {
		t.Error("TraceMapURL is empty, want the engine-provided map link")
	}

	// 跳必须按 TTL 升序，且 TTL 从 1 开始连续。
	for i, hop := range parsed.Hops {
		if hop.TTL != i+1 {
			t.Errorf("hops[%d].TTL = %d, want %d (path must be in TTL order)", i, hop.TTL, i+1)
		}
	}

	// 第一跳是本机网关，必须解析出 3 个 RTT 样本。
	first := parsed.Hops[0]
	if first.IP != "192.168.1.1" {
		t.Errorf("first hop IP = %q, want the local gateway", first.IP)
	}
	if len(first.RTTMS) != 3 {
		t.Fatalf("first hop has %d RTT samples, want 3", len(first.RTTMS))
	}
	if first.Timeout {
		t.Error("first hop marked as timeout, but it responded")
	}

	// **单位断言**：真实 RTT 字段是纳秒（1026700 = 1.0267 ms）。
	// 若有人把它当成毫秒，这里的数值会大 100 万倍。
	for i, rtt := range first.RTTMS {
		if rtt <= 0 || rtt > 100 {
			t.Errorf("first hop RTT[%d] = %v ms, want a plausible LAN value (<100ms)", i, rtt)
		}
	}
	if got := first.RTTMS[0]; got < 1.0 || got > 1.1 {
		t.Errorf("first hop RTT[0] = %v ms, want ~1.0267 (nanoseconds divided by 1e6)", got)
	}
	if got := first.MinRTT(); got < 0.99 || got > 1.0 {
		t.Errorf("first hop MinRTT() = %v, want ~0.9943", got)
	}

	// 公共路径上必须解析出 ASN 与运营商。
	var foundASN bool
	for _, hop := range parsed.Hops {
		if hop.ASN == "AS64500" {
			foundASN = true
			if hop.ASOrganization == "" {
				t.Errorf("hop %d has ASN but no organization", hop.TTL)
			}
			break
		}
	}
	if !foundASN {
		t.Error("no hop carried AS64500; ASN extraction is broken")
	}

	// 乱码治理：真实数据里 country/prov/city 是中文乱码，
	// country_en 是干净文本。我们优先取英文，且国家全称不硬凑成代码。
	for _, hop := range parsed.Hops {
		if hop.Country != "" && len(hop.Country) != 2 {
			t.Errorf("hop %d Country = %q, want either empty or a 2-letter code", hop.TTL, hop.Country)
		}
		for _, field := range []string{hop.Province, hop.City, hop.ASOrganization} {
			if strings.ContainsRune(field, '\uFFFD') {
				t.Errorf("hop %d has mojibake in a field: %q", hop.TTL, field)
			}
		}
	}

	// 超时跳必须被明确标记，且没有 RTT 样本（失败的探测时延是 0）。
	timeouts := 0
	for _, hop := range parsed.Hops {
		if !hop.Timeout {
			continue
		}
		timeouts++
		if hop.IP != "" {
			t.Errorf("hop %d is marked timeout but has IP %q", hop.TTL, hop.IP)
		}
		if len(hop.RTTMS) != 0 {
			t.Errorf("hop %d is marked timeout but has RTT samples %v (failed probes report 0)", hop.TTL, hop.RTTMS)
		}
	}
	if timeouts == 0 {
		t.Error("expected some timeout hops in the fixture (most of the internet filters ICMP)")
	}

	// 真实输出里第一跳私网地址必须**原样保留**（本地库存完整路径）。
	// 隐私过滤是导出层的事，不在解析层做——否则本地就丢了诊断信息。
	if !strings.HasPrefix(parsed.Hops[0].IP, "192.168.") {
		t.Errorf("first hop IP = %q, want the private address preserved in the local model", parsed.Hops[0].IP)
	}
}

// TestRealFixtureIsGenuineNextTraceOutput 确认夹具没有被手改过。
//
// 夹具一旦被手工编辑就不再是"真实输出"，也就失去了它的价值。
// 这里检查真实输出特有的结构痕迹。
func TestRealFixtureIsGenuineNextTraceOutput(t *testing.T) {
	raw := string(loadRealFixture(t))

	for _, marker := range []string{
		`"Hops"`,        // 真实字段名是大写 H
		`"Address"`,     // 地址是对象 {"IP":..,"Zone":..}
		`"SthopReason"`, // 故意写错，下面单独检查正确的
	} {
		if marker == `"SthopReason"` {
			continue
		}
		if !strings.Contains(raw, marker) {
			t.Errorf("fixture is missing the real structure marker %s", marker)
		}
	}
	if !strings.Contains(raw, `"StopReason"`) {
		t.Error("fixture is missing StopReason")
	}
	if !strings.Contains(raw, `"Geo"`) {
		t.Error("fixture is missing Geo")
	}
	// 真实输出有 RTT 纳秒量级的大整数；如果夹具被改成毫秒，这里会失败。
	// 真实输出的 RTT 是纳秒量级的大整数；若夹具被改成毫秒会失败。
	// 用正则匹配数字本体，不依赖 JSON 里的空格格式。
	if !regexp.MustCompile(`"RTT":s*1[0-9]{6}`).MatchString(raw) {
		t.Error("fixture no longer contains a raw nanosecond-scale RTT value")
	}
}

// ---------------------------------------------------------------------------
// 解析：容错与边界
// ---------------------------------------------------------------------------

func TestParseNextTraceJSONShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantHops int
		wantErr  bool
	}{
		{
			name:     "real shape (nested arrays)",
			body:     `{"Hops":[[{"Success":true,"Address":{"IP":"1.2.3.4"},"TTL":1,"RTT":1000000}]]}`,
			wantHops: 1,
		},
		{
			name:     "lowercase hops key",
			body:     `{"hops":[[{"Success":true,"Address":{"IP":"1.2.3.4"},"TTL":1,"RTT":1000000}]]}`,
			wantHops: 1,
		},
		{
			name:     "flat shape (one probe per entry)",
			body:     `{"Hops":[{"Success":true,"Address":{"IP":"1.2.3.4"},"TTL":1,"RTT":1000000}]}`,
			wantHops: 1,
		},
		{
			name:     "address as plain string",
			body:     `{"Hops":[[{"Success":true,"Address":"1.2.3.4","TTL":1,"RTT":2000000}]]}`,
			wantHops: 1,
		},
		{
			name:     "empty hop list is a valid result",
			body:     `{"Hops":[]}`,
			wantHops: 0,
		},
		{
			name:    "no hop list at all is an error",
			body:    `{"Something":"else"}`,
			wantErr: true,
		},
		{
			name:    "not json",
			body:    `hello`,
			wantErr: true,
		},
		{
			name:    "empty",
			body:    ``,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseNextTraceJSON([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNextTraceJSON: %v", err)
			}
			if len(parsed.Hops) != tc.wantHops {
				t.Errorf("hops = %d, want %d", len(parsed.Hops), tc.wantHops)
			}
		})
	}
}

// TestParseAggregatesProbesIntoOneHop 确认同一 TTL 的多次探测合成一跳。
//
// 这是最容易写错的地方：真实输出是二维数组，
// 如果把内层每次探测都当成一跳，30 跳会变成 90 跳，
// 而"平均跳数"这类统计会直接错三倍。
func TestParseAggregatesProbesIntoOneHop(t *testing.T) {
	body := `{"Hops":[
	  [
	    {"Success":true,"Address":{"IP":"10.0.0.1"},"TTL":1,"RTT":1000000},
	    {"Success":true,"Address":{"IP":"10.0.0.1"},"TTL":1,"RTT":2000000},
	    {"Success":false,"Address":null,"TTL":1,"RTT":0}
	  ],
	  [
	    {"Success":true,"Address":{"IP":"10.0.0.2"},"TTL":2,"RTT":5000000}
	  ]
	]}`

	parsed, err := ParseNextTraceJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	if len(parsed.Hops) != 2 {
		t.Fatalf("hops = %d, want 2 (probes of the same TTL must merge)", len(parsed.Hops))
	}

	first := parsed.Hops[0]
	// 只有成功的探测贡献 RTT：第三个探针失败（RTT 0）必须被排除，
	// 否则最小延迟会被拉成 0。
	if len(first.RTTMS) != 2 {
		t.Fatalf("RTT samples = %v, want 2 (the failed probe must be excluded)", first.RTTMS)
	}
	if first.MinRTT() != 1.0 {
		t.Errorf("MinRTT() = %v, want 1.0", first.MinRTT())
	}
	if first.Timeout {
		t.Error("hop marked timeout, but two probes succeeded")
	}

	// 全部探测都失败时，该跳必须标记超时。
	allFailed := `{"Hops":[[
	  {"Success":false,"Address":null,"TTL":1,"RTT":0},
	  {"Success":false,"Address":null,"TTL":1,"RTT":0}
	]]}`
	parsed, err = ParseNextTraceJSON([]byte(allFailed))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	if len(parsed.Hops) != 1 || !parsed.Hops[0].Timeout {
		t.Errorf("hops = %+v, want one timeout hop", parsed.Hops)
	}
}

// TestParseDropsProbesWithoutTTL 确认没有 TTL 的探测被丢弃。
//
// 没有 TTL 就无法定位到路径上的哪一跳，把它当成"第 0 跳"
// 会污染路径顺序。
func TestParseDropsProbesWithoutTTL(t *testing.T) {
	body := `{"Hops":[
	  [{"Success":true,"Address":{"IP":"1.2.3.4"},"RTT":1000000}],
	  [{"Success":true,"Address":{"IP":"5.6.7.8"},"TTL":2,"RTT":1000000}]
	]}`

	parsed, err := ParseNextTraceJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	if len(parsed.Hops) != 1 {
		t.Fatalf("hops = %d, want 1 (the entry without TTL must be dropped)", len(parsed.Hops))
	}
	if parsed.Hops[0].TTL != 2 {
		t.Errorf("TTL = %d, want 2", parsed.Hops[0].TTL)
	}
}

// TestParseHandlesMojibakeLocales 验证本地化乱码字段不会顶掉英文。
func TestParseHandlesMojibakeLocales(t *testing.T) {
	// 真实数据里 country/prov/city 是 "�й�" 这类乱码，
	// 而 country_en/prov_en/city_en 是干净的。必须优先英文。
	body := `{"Hops":[[{"Success":true,"Address":{"IP":"1.2.3.4"},"TTL":1,"RTT":1000000,
	  "Geo":{"asnumber":"64500","country":"�й�","country_en":"China",
	         "prov":"����","prov_en":"Zhejiang","city":"��ͨ��","city_en":"Hangzhou",
	         "owner":"example.net","isp":"�ƶ�"}}]]}`

	parsed, err := ParseNextTraceJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	hop := parsed.Hops[0]

	if hop.Province != "Zhejiang" {
		t.Errorf("Province = %q, want Zhejiang (the English variant)", hop.Province)
	}
	if hop.City != "Hangzhou" {
		t.Errorf("City = %q, want Hangzhou (the English variant)", hop.City)
	}
	if strings.ContainsRune(hop.Province, '\uFFFD') || strings.ContainsRune(hop.City, '\uFFFD') {
		t.Error("mojibake leaked into the normalized hop")
	}
	// "China" 是国家全称，没有权威映射表，因此留空而不是编一个代码。
	if hop.Country != "" {
		t.Errorf("Country = %q, want empty (full names cannot be trusted as codes)", hop.Country)
	}
	// ASN 要补 AS 前缀。
	if hop.ASN != "AS64500" {
		t.Errorf("ASN = %q, want AS64500", hop.ASN)
	}
}

func TestParseNextTraceJSONStripsLeadingNoise(t *testing.T) {
	// NextTrace 有时会在 JSON 前打印更新提示。
	body := "A new version is available: v1.8.0\n{\"Hops\":[[{\"Success\":true,\"Address\":{\"IP\":\"1.2.3.4\"},\"TTL\":1,\"RTT\":1000000}]]}"

	parsed, err := ParseNextTraceJSON([]byte(body))
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	if len(parsed.Hops) != 1 {
		t.Errorf("hops = %d, want 1", len(parsed.Hops))
	}
}

func TestParseNextTraceJSONHandlesBOM(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte(`{"Hops":[[{"Success":true,"Address":{"IP":"1.2.3.4"},"TTL":1,"RTT":1000000}]]}`)...)

	parsed, err := ParseNextTraceJSON(body)
	if err != nil {
		t.Fatalf("ParseNextTraceJSON: %v", err)
	}
	if len(parsed.Hops) != 1 {
		t.Errorf("hops = %d, want 1", len(parsed.Hops))
	}
}

// ---------------------------------------------------------------------------
// Hop 辅助
// ---------------------------------------------------------------------------

func TestHopHelpers(t *testing.T) {
	hop := Hop{TTL: 3, IP: "1.2.3.4", RTTMS: []float64{9.5, 4.2, 7.1}}
	if got := hop.MinRTT(); got != 4.2 {
		t.Errorf("MinRTT() = %v, want 4.2", got)
	}
	if got := hop.Target(); got != "1.2.3.4" {
		t.Errorf("Target() = %q", got)
	}
	if got := (Hop{}).MinRTT(); got != 0 {
		t.Errorf("MinRTT() = %v, want 0 for no samples", got)
	}
	if got := (Hop{}).Target(); got != "" {
		t.Errorf("Target() = %q, want empty", got)
	}
}

func TestTraceResultHelpers(t *testing.T) {
	result := TraceResult{
		Hops: []Hop{
			{TTL: 1, IP: "10.0.0.1"},
			{TTL: 2, Timeout: true},
			{TTL: 3, IP: "1.1.1.1"},
		},
	}
	if got := result.HopCount(); got != 3 {
		t.Errorf("HopCount() = %d, want 3", got)
	}
	// 超时跳不算"有回复"。
	if got := result.RespondedHops(); got != 2 {
		t.Errorf("RespondedHops() = %d, want 2", got)
	}
}

func TestTraceResultValid(t *testing.T) {
	ok := TraceResult{TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, Engine: "nexttrace"}
	if err := ok.Valid(); err != nil {
		t.Errorf("Valid() = %v, want nil", err)
	}

	failed := TraceResult{TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, ErrorType: ErrorTypeTimeout}
	if err := failed.Valid(); err != nil {
		t.Errorf("Valid() = %v, want nil for a classified failure", err)
	}

	cases := map[string]TraceResult{
		"no target":          {IP: "1.2.3.4", Port: 443, Success: true},
		"bad ip":             {TargetID: "x:443", IP: "nope", Port: 443, Success: true},
		"bad port":           {TargetID: "1.2.3.4:0", IP: "1.2.3.4", Port: 0, Success: true},
		"success with error": {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, ErrorType: ErrorTypeTimeout},
		"failure no type":    {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443},
		"unknown type":       {TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, ErrorType: "weird"},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			if err := result.Valid(); err == nil {
				t.Error("Valid() = nil, want error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mode
// ---------------------------------------------------------------------------

func TestModeNormalize(t *testing.T) {
	cases := map[string]Mode{
		"":      ModeTCP, // 默认 tcp
		"tcp":   ModeTCP,
		"TCP":   ModeTCP,
		" tcp ": ModeTCP,
		"icmp":  ModeICMP,
		"udp":   ModeUDP,
	}
	for in, want := range cases {
		got, err := Mode(in).Normalize()
		if err != nil {
			t.Errorf("Normalize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := Mode("gre").Normalize(); err == nil {
		t.Error("Normalize accepted an unsupported mode")
	}
	for _, mode := range []Mode{ModeTCP, ModeICMP, ModeUDP} {
		if !mode.Valid() {
			t.Errorf("%q.Valid() = false", mode)
		}
	}
	if Mode("").Valid() {
		t.Error("empty mode reported as valid")
	}
}

// TestErrorTypeSemantics 验证分类语义。
func TestErrorTypeSemantics(t *testing.T) {
	for _, kind := range AllErrorTypes() {
		if !kind.Valid() {
			t.Errorf("%q.Valid() = false", kind)
		}
	}
	if !ErrorTypeNone.Valid() {
		t.Error("ErrorTypeNone.Valid() = false")
	}
	if ErrorType("bogus").Valid() {
		t.Error("unknown error type reported as valid")
	}

	// 环境问题不能算作路径质量证据。
	for kind, want := range map[ErrorType]bool{
		ErrorTypeNone:          false,
		ErrorTypeNotFound:      false,
		ErrorTypePermission:    false,
		ErrorTypeParse:         false,
		ErrorTypeCanceled:      false,
		ErrorTypeInvalidTarget: false,
		ErrorTypeTimeout:       true,
		ErrorTypeExitCode:      true,
		ErrorTypeOther:         true,
	} {
		if got := kind.CountsTowardPathQuality(); got != want {
			t.Errorf("%q.CountsTowardPathQuality() = %v, want %v", kind, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 命令构造（需求第 27、28 条）
// ---------------------------------------------------------------------------

// TestBuildArgsUsesActualTargetPort 是需求第 27 条的核心测试。
//
// IP:Port 里的 Port 必须原样传给 NextTrace，不能固定成 443。
func TestBuildArgsUsesActualTargetPort(t *testing.T) {
	engine := mustEngine(t, EngineOptions{Mode: ModeTCP})

	cases := []struct {
		ip   string
		port int
	}{
		{"1.2.3.4", 443},
		{"1.2.3.4", 2053},
		{"1.2.3.4", 8443},
		{"1.2.3.4", 2087},
		{"2001:db8::1", 2053},
	}

	for _, tc := range cases {
		t.Run(tc.ip+":"+itoa(tc.port), func(t *testing.T) {
			target := mustTarget(t, tc.ip, tc.port)
			args, err := engine.BuildArgs(target)
			if err != nil {
				t.Fatalf("BuildArgs: %v", err)
			}

			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--port "+itoa(tc.port)) {
				t.Errorf("args = %v, want --port %d (the target's own port)", args, tc.port)
			}
			// 不能出现别的端口。
			if tc.port != 443 && strings.Contains(joined, "--port 443") {
				t.Errorf("args = %v, must not hardcode 443", args)
			}
			// 目标地址必须是最后一个参数，且是裸地址（无方括号）。
			if args[len(args)-1] != tc.ip {
				t.Errorf("last arg = %q, want the bare target ip %q", args[len(args)-1], tc.ip)
			}
		})
	}
}

// TestBuildArgsSelectsTCPByDefault 验证默认模式是 TCP。
//
// 项目测的是 IP:Port，因此真正相关的是 TCP -> 端口，
// 而不是 ICMP ping。
func TestBuildArgsSelectsTCPByDefault(t *testing.T) {
	engine := mustEngine(t, EngineOptions{})

	args, err := engine.BuildArgs(mustTarget(t, "1.2.3.4", 443))
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--tcp") {
		t.Errorf("args = %v, want --tcp", args)
	}
	if !strings.Contains(joined, "--json") {
		t.Errorf("args = %v, want --json (machine-readable output only)", args)
	}
}

func TestBuildArgsModes(t *testing.T) {
	cases := []struct {
		mode     Mode
		wantFlag string
		wantPort bool
	}{
		{ModeTCP, "--tcp", true},
		{ModeUDP, "--udp", true},
		{ModeICMP, "--icmp-mode", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			engine := mustEngine(t, EngineOptions{Mode: tc.mode})
			args, err := engine.BuildArgs(mustTarget(t, "1.2.3.4", 2053))
			if err != nil {
				t.Fatalf("BuildArgs: %v", err)
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, tc.wantFlag) {
				t.Errorf("args = %v, want %s", args, tc.wantFlag)
			}
			if tc.wantPort && !strings.Contains(joined, "--port 2053") {
				t.Errorf("args = %v, want --port 2053", args)
			}
		})
	}
}

// TestBuildArgsForIPv6HasNoBrackets 验证 IPv6 用裸地址。
//
// nexttrace 接受裸 IPv6；"[::1]:443" 这种形式它不认识。
func TestBuildArgsForIPv6HasNoBrackets(t *testing.T) {
	engine := mustEngine(t, EngineOptions{})
	args, err := engine.BuildArgs(mustTarget(t, "2001:db8::1", 443))
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if last := args[len(args)-1]; strings.ContainsAny(last, "[]") {
		t.Errorf("last arg = %q, want a bare IPv6 address without brackets", last)
	}
}

func TestBuildArgsRejectsInvalidTarget(t *testing.T) {
	engine := mustEngine(t, EngineOptions{})

	broken := model.Target{ID: "1.2.3.4:99999", IP: "1.2.3.4", Port: 99999}
	if _, err := engine.BuildArgs(broken); err == nil {
		t.Error("BuildArgs accepted an invalid port")
	}
}

// ---------------------------------------------------------------------------
// 进程执行（注入 runCommand）
// ---------------------------------------------------------------------------

func TestTraceSuccessPath(t *testing.T) {
	body := loadRealFixture(t)

	var gotArgs []string
	engine := mustEngine(t, EngineOptions{
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			gotArgs = args
			// 睡一小会儿：Windows 的计时器粒度约 15ms，
			// 否则 DurationMS 会量出恰好 0，断言失去意义。
			time.Sleep(2 * time.Millisecond)
			return body, nil, nil
		},
	})

	result, err := engine.Trace(context.Background(), mustTarget(t, "1.1.1.1", 443))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	if !result.Success {
		t.Fatalf("Success = false: %s", result.ErrorMessage)
	}
	if result.HopCount() != 30 {
		t.Errorf("HopCount() = %d, want 30", result.HopCount())
	}
	if result.Engine != "nexttrace" {
		t.Errorf("Engine = %q, want nexttrace", result.Engine)
	}
	if result.Mode != ModeTCP {
		t.Errorf("Mode = %q, want tcp", result.Mode)
	}
	if result.Port != 443 {
		t.Errorf("Port = %d, want 443", result.Port)
	}
	if result.DurationMS <= 0 {
		t.Error("DurationMS = 0, want positive")
	}
	// 原始输出必须保留（诊断用），但不参与业务判断。
	if result.RawJSON == "" {
		t.Error("RawJSON is empty, want the raw engine output kept for diagnostics")
	}
	if err := result.Valid(); err != nil {
		t.Errorf("Valid() = %v", err)
	}
	// 参数必须已经包含显式模式与端口。
	if joined := strings.Join(gotArgs, " "); !strings.Contains(joined, "--tcp") || !strings.Contains(joined, "--port 443") {
		t.Errorf("engine args = %v, want --tcp --port 443", gotArgs)
	}
}

func TestTraceClassifiesFailures(t *testing.T) {
	cases := []struct {
		name     string
		stdout   []byte
		stderr   []byte
		runErr   error
		wantType ErrorType
	}{
		{
			name:     "non-zero exit",
			stderr:   []byte("something went wrong"),
			runErr:   &exec.ExitError{},
			wantType: ErrorTypeExitCode,
		},
		{
			name:     "permission denied",
			stderr:   []byte("Windows TCP 探测 依赖 WinDivert，但当前进程没有管理员权限"),
			runErr:   errors.New("exit status 1"),
			wantType: ErrorTypePermission,
		},
		{
			name:     "deadline exceeded",
			runErr:   context.DeadlineExceeded,
			wantType: ErrorTypeTimeout,
		},
		{
			name:     "no output",
			stdout:   nil,
			runErr:   nil,
			wantType: ErrorTypeParse,
		},
		{
			name:     "unparseable output",
			stdout:   []byte("not json at all"),
			wantType: ErrorTypeParse,
		},
		{
			name:     "unknown failure",
			runErr:   errors.New("mysterious"),
			wantType: ErrorTypeOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := mustEngine(t, EngineOptions{
				RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
					return tc.stdout, tc.stderr, tc.runErr
				},
			})

			result, err := engine.Trace(context.Background(), mustTarget(t, "1.2.3.4", 443))
			// 单个目标失败不返回 error，而是带分类的结果。
			if err != nil {
				t.Fatalf("Trace returned error: %v", err)
			}
			if result.Success {
				t.Fatal("Success = true, want failure")
			}
			if result.ErrorType != tc.wantType {
				t.Errorf("ErrorType = %q, want %q (message=%q)", result.ErrorType, tc.wantType, result.ErrorMessage)
			}
			if result.ErrorMessage == "" {
				t.Error("ErrorMessage is empty")
			}
			if err := result.Valid(); err != nil {
				t.Errorf("Valid() = %v", err)
			}
		})
	}
}

// TestTracePermissionErrorIsNotPathQuality 验证权限问题不被当成路径问题。
func TestTracePermissionErrorIsNotPathQuality(t *testing.T) {
	engine := mustEngine(t, EngineOptions{
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return nil, []byte("需要管理员权限"), errors.New("exit status 1")
		},
	})

	result, err := engine.Trace(context.Background(), mustTarget(t, "1.2.3.4", 443))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if result.ErrorType.CountsTowardPathQuality() {
		t.Errorf("%q must not count toward path quality: it is an environment problem", result.ErrorType)
	}
}

func TestTraceCanceledIsNotTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	engine := mustEngine(t, EngineOptions{
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return nil, nil, ctx.Err()
		},
	})

	result, err := engine.Trace(ctx, mustTarget(t, "1.2.3.4", 443))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if result.ErrorType != ErrorTypeCanceled {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, ErrorTypeCanceled)
	}
	if result.ErrorType.CountsTowardPathQuality() {
		t.Error("canceled must not count toward path quality")
	}
}

func TestTraceInvalidTargetIsClassified(t *testing.T) {
	engine := mustEngine(t, EngineOptions{})

	broken := model.Target{ID: "1.2.3.4:99999", IP: "1.2.3.4", Port: 99999}
	result, err := engine.Trace(context.Background(), broken)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if result.ErrorType != ErrorTypeInvalidTarget {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, ErrorTypeInvalidTarget)
	}
	if result.ErrorType.CountsTowardPathQuality() {
		t.Error("invalid target must not count toward path quality")
	}
}

func TestTraceTimeoutIsApplied(t *testing.T) {
	var deadlineSet bool
	engine := mustEngine(t, EngineOptions{
		Timeout: 30 * time.Millisecond,
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			if _, ok := ctx.Deadline(); ok {
				deadlineSet = true
			}
			// 模拟慢进程：等 ctx 结束。
			<-ctx.Done()
			return nil, nil, ctx.Err()
		},
	})

	result, err := engine.Trace(context.Background(), mustTarget(t, "1.2.3.4", 443))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if !deadlineSet {
		t.Error("no deadline was set on the command context")
	}
	if result.ErrorType != ErrorTypeTimeout {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, ErrorTypeTimeout)
	}
}

// TestTraceStderrIsCaptured 验证标准错误被单独收集。
//
// stdout 是 JSON，必须干净；stderr 是诊断信息，不能混进 JSON。
func TestTraceStderrIsCaptured(t *testing.T) {
	engine := mustEngine(t, EngineOptions{
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return loadRealFixture(t), []byte("warning: something"), nil
		},
	})

	result, err := engine.Trace(context.Background(), mustTarget(t, "1.1.1.1", 443))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if !result.Success {
		t.Fatal("Success = false, want true (stderr is not a failure)")
	}
	if !strings.Contains(result.Stderr, "warning") {
		t.Errorf("Stderr = %q, want the engine stderr captured", result.Stderr)
	}
}

// ---------------------------------------------------------------------------
// 可用性检查
// ---------------------------------------------------------------------------

func TestCheckAvailabilityMissingBinary(t *testing.T) {
	avail := CheckAvailability(context.Background(), "definitely-not-installed-nexttrace-xyz")

	if avail.Found {
		t.Fatal("Found = true for a missing binary")
	}
	if avail.Err == nil {
		t.Error("Err = nil, want a reason")
	}
	// 提示信息必须给出下一步（需求第 25 条）。
	if !strings.Contains(NotFoundMessage, "NextTrace not found.") {
		t.Errorf("NotFoundMessage = %q", NotFoundMessage)
	}
	if !strings.Contains(NotFoundMessage, "trace.nexttrace.binary") {
		t.Errorf("NotFoundMessage = %q, want it to mention the config key", NotFoundMessage)
	}
}

func TestCheckAvailabilityMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.exe")
	avail := CheckAvailability(context.Background(), missing)
	if avail.Found {
		t.Error("Found = true for a non-existent path")
	}
}

func TestNewNextTraceEngineMissingBinary(t *testing.T) {
	_, err := NewNextTraceEngine(context.Background(), EngineOptions{
		BinaryPath: "definitely-not-installed-nexttrace-xyz",
	})
	if err == nil {
		t.Fatal("NewNextTraceEngine succeeded for a missing binary")
	}
	// 错误信息必须包含可操作的提示。
	if !strings.Contains(err.Error(), "NextTrace not found.") {
		t.Errorf("error = %v, want the not-found message", err)
	}
}

func TestNewNextTraceEngineRejectsBadMode(t *testing.T) {
	_, err := NewNextTraceEngine(context.Background(), EngineOptions{Mode: Mode("gre")})
	if err == nil {
		t.Error("NewNextTraceEngine accepted an unsupported mode")
	}
}

// TestNewNextTraceEngineWithFakeBinary 用一个真实的假二进制验证端到端进程调用。
//
// 这里真正启动一个子进程（而不是注入 runCommand），
// 从而覆盖 exec.CommandContext、stdout/stderr 分离、退出码这几条路径。
// Windows 下用 .bat 不方便，因此用"当前测试二进制自身"作为假引擎
// （通过 GO_FAKE_NEXTRACE 环境变量切换行为）。
func TestNewNextTraceEngineWithFakeBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("self-exec fake engine relies on a shell script; covered by the injected-runCommand tests on windows")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-nexttrace")
	body := loadRealFixture(t)
	// 写一个真实可执行脚本：打印 --version，或输出夹具 JSON。
	content := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'NextTrace v9.9.9 fake'; exit 0; fi\n" +
		"cat <<'EOF'\n" + string(body) + "\nEOF\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	engine, err := NewNextTraceEngine(context.Background(), EngineOptions{BinaryPath: script})
	if err != nil {
		t.Fatalf("NewNextTraceEngine: %v", err)
	}
	if engine.Version() != "9.9.9" {
		t.Errorf("Version() = %q, want 9.9.9 (parsed from --version)", engine.Version())
	}

	result, err := engine.Trace(context.Background(), mustTarget(t, "1.1.1.1", 2053))
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if !result.Success {
		t.Fatalf("Success = false: %s", result.ErrorMessage)
	}
	if result.HopCount() != 30 {
		t.Errorf("HopCount() = %d, want 30", result.HopCount())
	}
	if result.EngineVersion != "9.9.9" {
		t.Errorf("EngineVersion = %q, want 9.9.9", result.EngineVersion)
	}
	if result.Port != 2053 {
		t.Errorf("Port = %d, want 2053", result.Port)
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"NextTrace v1.7.3 2026-08-21T11:21:28Z 40ac803": "1.7.3",
		"nexttrace 1.7.3":         "1.7.3",
		"v1.7":                    "1.7",
		"NextTrace v1.7.3-beta.1": "1.7.3-beta.1",
		"no version here":         "no version here",
		"":                        "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func TestStats(t *testing.T) {
	var stats Stats

	stats.Add(&TraceResult{Success: true, Hops: make([]Hop, 12)})
	stats.Add(&TraceResult{Success: true, Hops: make([]Hop, 16)})
	stats.Add(&TraceResult{ErrorType: ErrorTypeTimeout})
	stats.Add(&TraceResult{ErrorType: ErrorTypeTimeout})
	stats.Add(&TraceResult{ErrorType: ErrorTypePermission})

	if stats.Completed != 5 {
		t.Errorf("Completed = %d, want 5", stats.Completed)
	}
	if stats.Success != 2 || stats.Failed != 3 {
		t.Errorf("Success/Failed = %d/%d, want 2/3", stats.Success, stats.Failed)
	}
	if got := stats.SuccessRate(); got != 0.4 {
		t.Errorf("SuccessRate() = %v, want 0.4", got)
	}
	if got := stats.AverageHops(); got != 14 {
		t.Errorf("AverageHops() = %v, want 14", got)
	}
	if stats.ErrorCounts[ErrorTypeTimeout] != 2 {
		t.Errorf("timeout count = %d, want 2", stats.ErrorCounts[ErrorTypeTimeout])
	}

	// 空统计不能 panic。
	var empty Stats
	if empty.SuccessRate() != 0 || empty.AverageHops() != 0 {
		t.Error("empty Stats should report zero rates")
	}
	empty.Add(nil) // 不应 panic
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// mustEngine 构造一个测试引擎。
//
// 即使不注入 runCommand，也不会真的去解析二进制：
// 这里显式提供 runCommand 或路径解析失败时直接让测试失败。
func mustEngine(t *testing.T, opts EngineOptions) *NextTraceEngine {
	t.Helper()

	// 默认注入一个"什么都不做"的执行器，避免测试真的去启动进程。
	if opts.RunCommand == nil {
		opts.RunCommand = func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return nil, nil, errors.New("test engine: no runCommand configured")
		}
	}

	engine, err := NewNextTraceEngine(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewNextTraceEngine: %v", err)
	}
	return engine
}

// mustTarget 构造一个合法的测试目标。
func mustTarget(t *testing.T, ip string, port int) model.Target {
	t.Helper()
	target, err := model.NewTargetFromStrings(ip, port)
	if err != nil {
		t.Fatalf("NewTargetFromStrings(%q, %d): %v", ip, port, err)
	}
	return target
}

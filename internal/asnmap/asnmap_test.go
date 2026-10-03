package asnmap

import (
	"testing"
)

// TestUserProvidedMappings 固定用户给出的那八条线路映射。
//
// 这些是这份表存在的理由，改动它们必须是有意的：
// 认错线路会直接导致使用者对"这条线路好不好"做出错误判断。
func TestUserProvidedMappings(t *testing.T) {
	cases := map[string]string{
		"AS4134":  "163",
		"AS4809":  "CN2",
		"AS4837":  "169",
		"AS9929":  "9929/CUII",
		"AS9808":  "CMNET",
		"AS58453": "CMI",
		"AS58807": "CMIN2",
		"AS10099": "CUG",
	}

	for asn, want := range cases {
		if got := Name(asn); got != want {
			t.Errorf("Name(%q) = %q, want %q", asn, got, want)
		}
	}
}

// TestNormalizeAcceptsCommonForms 验证各种常见写法都能识别。
//
// ASN 会从三个地方来：NextTrace 的纯数字、用户的随手输入、
// 以及已经规范化过的字符串。三者都得认。
func TestNormalizeAcceptsCommonForms(t *testing.T) {
	for _, raw := range []string{"AS4134", "as4134", "As4134", "4134", " 4134 ", " as4134 "} {
		if got := Normalize(raw); got != "AS4134" {
			t.Errorf("Normalize(%q) = %q, want AS4134", raw, got)
		}
	}
}

// TestNormalizeRejectsNonASN 验证不能识别时返回空。
//
// 关键是**不能**把路径串或乱码当成 ASN：错误地"识别"出
// 一个编号会让日志里出现一个看起来可信、实际错误的名字。
func TestNormalizeRejectsNonASN(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "AS", "AS-", "hello", "AS4134-AS4809", "AS12AB", "4134.5",
	} {
		if got := Normalize(raw); got != "" {
			t.Errorf("Normalize(%q) = %q, want empty", raw, got)
		}
	}
}

// TestUnknownASNReturnsNoName 验证未知 ASN 不编造名称。
//
// 这是刻意的：猜错的名字比没有名字更糟，使用者会据此判断线路质量。
func TestUnknownASNReturnsNoName(t *testing.T) {
	if got := Name("AS65001"); got != "" {
		t.Errorf("Name(AS65001) = %q, want empty (unknown ASN must not invent a name)", got)
	}
	if _, ok := Lookup("AS65001"); ok {
		t.Error("Lookup reported an unknown ASN as known")
	}
}

// TestLabelKeepsASNNumber 验证显示文本保留编号。
//
// 名称只是便于阅读，排查问题时需要编号去核对。
func TestLabelKeepsASNNumber(t *testing.T) {
	if got := Label("AS4134"); got != "AS4134 (163)" {
		t.Errorf("Label(AS4134) = %q, want %q", got, "AS4134 (163)")
	}
	// 未知 ASN 只显示编号。
	if got := Label("AS65001"); got != "AS65001" {
		t.Errorf("Label(AS65001) = %q, want AS65001", got)
	}
	// 空输入返回空，而不是 "()"。
	if got := Label(""); got != "" {
		t.Errorf("Label(\"\") = %q, want empty", got)
	}
}

// TestShortPathUsesNames 验证短路径用的是名称而不是编号。
func TestShortPathUsesNames(t *testing.T) {
	got := ShortPath([]string{"AS9808", "AS58453", "AS13335"})
	want := "CMNET > CMI > Cloudflare"
	if got != want {
		t.Errorf("ShortPath = %q, want %q", got, want)
	}
}

// TestShortPathSkipsUnknown 验证未知 ASN 在短路径里被略去。
//
// 一行日志的位置有限，把"AS65001"塞进去只会挤掉有用信息。
func TestShortPathSkipsUnknown(t *testing.T) {
	got := ShortPath([]string{"AS4134", "AS65001", "AS4809"})
	want := "163 > CN2"
	if got != want {
		t.Errorf("ShortPath = %q, want %q (unknown ASN should be skipped)", got, want)
	}
}

// TestShortPathCollapsesAdjacentDuplicates 验证相邻重复合并。
func TestShortPathCollapsesAdjacentDuplicates(t *testing.T) {
	got := ShortPath([]string{"AS9808", "AS9808", "AS9808", "AS58453"})
	want := "CMNET > CMI"
	if got != want {
		t.Errorf("ShortPath = %q, want %q", got, want)
	}
}

// TestShortPathKeepsNonAdjacentDuplicates 验证不相邻的重复**保留**。
//
// "163 > CN2 > 163" 表示流量出去又绕回普通出口，
// 这是重要的路径异常信息，合并掉就等于隐瞒。
func TestShortPathKeepsNonAdjacentDuplicates(t *testing.T) {
	got := ShortPath([]string{"AS4134", "AS4809", "AS4134"})
	want := "163 > CN2 > 163"
	if got != want {
		t.Errorf("ShortPath = %q, want %q (non-adjacent repeats are meaningful)", got, want)
	}
}

// TestShortPathAllUnknownReturnsEmpty 验证全部未知时返回空。
//
// 调用方据此显示"未识别出已知骨干线路"，
// 而不是显示一个空白的"线路："。
func TestShortPathAllUnknownReturnsEmpty(t *testing.T) {
	if got := ShortPath([]string{"AS65001", "AS65002", ""}); got != "" {
		t.Errorf("ShortPath = %q, want empty when nothing is recognized", got)
	}
	if got := ShortPath(nil); got != "" {
		t.Errorf("ShortPath(nil) = %q, want empty", got)
	}
}

// TestFullPathKeepsNumbersAndUnknowns 验证完整路径保留编号与未知项。
func TestFullPathKeepsNumbersAndUnknowns(t *testing.T) {
	got := FullPath([]string{"AS4134", "AS65001", "AS13335"})
	want := "163(AS4134) > AS65001 > Cloudflare(AS13335)"
	if got != want {
		t.Errorf("FullPath = %q, want %q", got, want)
	}
}

// TestKnownIsSortedAndComplete 验证已知表导出完整且有序。
func TestKnownIsSortedAndComplete(t *testing.T) {
	known := Known()
	if len(known) < 8 {
		t.Fatalf("Known() has %d entries, want at least the 8 core routes", len(known))
	}

	// 必须包含用户给出的八条。
	required := []string{"AS4134", "AS4809", "AS4837", "AS9929", "AS9808", "AS58453", "AS58807", "AS10099"}
	have := make(map[string]bool, len(known))
	for _, route := range known {
		have[route.ASN] = true
	}
	for _, asn := range required {
		if !have[asn] {
			t.Errorf("Known() is missing %s", asn)
		}
	}

	// 按编号升序，便于人查找。
	for i := 1; i < len(known); i++ {
		if asnNumber(known[i-1].ASN) > asnNumber(known[i].ASN) {
			t.Errorf("Known() is not sorted: %s before %s", known[i-1].ASN, known[i].ASN)
			break
		}
	}

	// 每条都必须自洽（名称非空、ASN 规范）。
	for _, route := range known {
		if route.Name == "" {
			t.Errorf("%s has an empty name", route.ASN)
		}
		if Normalize(route.ASN) != route.ASN {
			t.Errorf("%s is not in canonical form", route.ASN)
		}
	}
}

// TestOperatorLookup 验证运营商查询。
func TestOperatorLookup(t *testing.T) {
	if got := Operator("AS4134"); got != "中国电信" {
		t.Errorf("Operator(AS4134) = %q, want 中国电信", got)
	}
	if got := Operator("AS9929"); got != "中国联通" {
		t.Errorf("Operator(AS9929) = %q, want 中国联通", got)
	}
	if got := Operator("AS58807"); got != "中国移动" {
		t.Errorf("Operator(AS58807) = %q, want 中国移动", got)
	}
	if got := Operator("AS65001"); got != "" {
		t.Errorf("Operator(AS65001) = %q, want empty", got)
	}
}

// TestShortPathCollapsesSameNameFromDifferentASNs 验证同一运营商
// 的多个 ASN 在路径里只显示一次。
//
// 真实场景：阿里云/腾讯云各有多个 ASN，一条路径里连着出现
// 会让日志变成 "Alibaba > Alibaba"。
func TestShortPathCollapsesSameNameFromDifferentASNs(t *testing.T) {
	// Alibaba: AS45102 与 AS37963 都叫 Alibaba。
	got := ShortPath([]string{"AS45102", "AS37963", "AS13335"})
	want := "Alibaba > Cloudflare"
	if got != want {
		t.Errorf("ShortPath = %q, want %q", got, want)
	}
}

// TestDistinctRoutesDoNotShareNames 验证**不同**线路不会显示成同一个名字。
//
// 这条是为 CMI / CMIN2 加的：两者都是中国移动的出口但档次不同，
// 如果在路径里显示成同一个词，使用者就无法区分"走了 CMI 还是 CMIN2"，
// 而这正是这份表要回答的问题。
func TestDistinctRoutesDoNotShareNames(t *testing.T) {
	// 每一对"应当被区分"的 ASN。
	pairs := [][2]string{
		{"AS58453", "AS58807"}, // CMI vs CMIN2
		{"AS4134", "AS4809"},   // 163 vs CN2
		{"AS4837", "AS9929"},   // 169 vs 9929
		{"AS9808", "AS58453"},  // CMNET vs CMI
		{"AS10099", "AS9929"},  // CUG vs 9929
	}

	for _, pair := range pairs {
		left, right := Name(pair[0]), Name(pair[1])
		if left == "" || right == "" {
			t.Errorf("%s/%s: one of them has no name", pair[0], pair[1])
			continue
		}
		if left == right {
			t.Errorf("%s and %s both display as %q; they would be indistinguishable",
				pair[0], pair[1], left)
		}
	}
}

// TestPathKeepsOrderAndDistinguishesRoutes 验证路径既保序又能区分档次。
func TestPathKeepsOrderAndDistinguishesRoutes(t *testing.T) {
	// 一条典型回程：移动普通出口 -> 移动国际 -> 移动优质出口 -> Cloudflare。
	got := ShortPath([]string{"AS9808", "AS58453", "AS58807", "AS13335"})
	want := "CMNET > CMI > CMIN2 > Cloudflare"
	if got != want {
		t.Errorf("ShortPath = %q, want %q", got, want)
	}
}

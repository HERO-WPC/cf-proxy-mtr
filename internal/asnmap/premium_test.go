package asnmap

import "testing"

// 本文件覆盖优质线路的判定。
//
// 它有两个使用者，而且吃进去的东西不一样：service 手里是 ASN，
// csvstore 手里是 CSV 的短名。两处各存一份清单必然漂移，而漂移的
// 表现只是"日志标红了但表格没置顶"这类没有报错的怪现象——
// 所以清单只放在这里，两个入口都从同一份数据推导。

// TestPremiumRoutesMatchTheCatalogue 验证清单里的 ASN 与名字确实存在于线路表。
//
// 这条测试挡的是"写错一个字"：清单里写 AS58907（少一位）不会报任何错，
// 只会永远匹配不上——而那看起来就像"这条线路今天没出现"。
func TestPremiumRoutesMatchTheCatalogue(t *testing.T) {
	for _, premium := range PremiumRoutes {
		route, ok := Lookup(premium.ASN)
		if !ok {
			t.Errorf("%s is not a known ASN", premium.ASN)
			continue
		}
		if route.Name != premium.Name {
			t.Errorf("%s: catalogue says %q, premium list says %q",
				premium.ASN, route.Name, premium.Name)
		}
	}
}

// TestPremiumRankByASN 验证按 ASN 判定的优先级。
func TestPremiumRankByASN(t *testing.T) {
	cases := map[string]int{
		"AS58807":  0, // CMIN2
		"as58807":  0, // 大小写容忍
		"58807":    0, // 不带 AS 前缀也认
		"AS4809":   1, // CN2
		"AS9929":   2, // 9929/CUII
		"AS4134":   -1,
		"AS13335":  -1,
		"":         -1,
		"nonsense": -1,
	}
	for input, want := range cases {
		if got := PremiumRank(input); got != want {
			t.Errorf("PremiumRank(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestPremiumPathRankTakesHighestPriority 验证一条路径经过多条优质线路时
// 取等级最高的那条。
//
// 它确实走了那条更好的线路，只报低的那条会低估它。
func TestPremiumPathRankTakesHighestPriority(t *testing.T) {
	cases := []struct {
		name string
		asns []string
		want int
	}{
		{"only cmin2", []string{"AS4134", "AS58807"}, 0},
		{"only cn2", []string{"AS4134", "AS4809"}, 1},
		{"only 9929", []string{"AS4837", "AS9929"}, 2},
		{"cn2 before cmin2 in path", []string{"AS4809", "AS58807"}, 0},
		{"ordinary", []string{"AS4134", "AS174"}, -1},
		{"empty", nil, -1},
	}
	for _, tc := range cases {
		if got := PremiumPathRank(tc.asns); got != tc.want {
			t.Errorf("%s: PremiumPathRank(%v) = %d, want %d", tc.name, tc.asns, got, tc.want)
		}
	}
}

// TestPremiumNameRankMatchesShortNames 验证按短名判定（CSV 的 as_path 用的是它）。
func TestPremiumNameRankMatchesShortNames(t *testing.T) {
	cases := map[string]int{
		"CMIN2":     0,
		"cmin2":     0,
		" CN2 ":     1,
		"9929/CUII": 2,
		"9929":      2, // 别名
		"CUII":      2, // 别名
		"163":       -1,
		"CMNET":     -1,
		"CMI":       -1,
		"":          -1,
	}
	for input, want := range cases {
		if got := PremiumNameRank(input); got != want {
			t.Errorf("PremiumNameRank(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestPremiumNameRankDoesNotMatchSubstrings 验证**不做子串匹配**。
//
// 子串会让一个恰好包含 "CN2" 的名字被误判。那种错误只表现为
// "排序看起来不太对"或"日志红了一条不该红的"，极难排查。
func TestPremiumNameRankDoesNotMatchSubstrings(t *testing.T) {
	for _, lookalike := range []string{"CMIN2X", "XCN2", "CN2X", "9929/CUIIX", "CMIN"} {
		if got := PremiumNameRank(lookalike); got != -1 {
			t.Errorf("PremiumNameRank(%q) = %d; only an exact name should match", lookalike, got)
		}
	}
}

// TestIsPremiumASN 验证便捷判定。
func TestIsPremiumASN(t *testing.T) {
	if !IsPremiumASN("AS4809") {
		t.Error("AS4809 (CN2) should be premium")
	}
	if IsPremiumASN("AS4134") {
		t.Error("AS4134 (163) should not be premium")
	}
}

package cli

import (
	"github.com/cf-route-tester/cf-route-tester/internal/asnmap"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// formatASPathSummary 把逐跳的 ASN 压成一行"线路串"。
//
// 例如：163(AS4134) > CN2(AS4809) > Cloudflare(AS13335)
//
// 为什么值得单独一行：逐跳表里 ASN 混在几十行里，很难一眼看出
// "回程走的是哪条线路"；而这恰恰是使用者最关心的一件事。
//
// 具体的合并规则（相邻去重、保留非相邻重复）在 asnmap.FullPath 里，
// 与 web 界面共用同一份实现——两处各写一遍必然分叉。
func formatASPathSummary(hops []trace.Hop) string {
	return asnmap.FullPath(hopASNs(hops))
}

// formatASPathCompact 返回更短的线路串（只有名称），用于表格与日志行。
func formatASPathCompact(hops []trace.Hop) string {
	return asnmap.ShortPath(hopASNs(hops))
}

// hopASNs 抽出逐跳的 ASN。
//
// asnmap 刻意不依赖 trace 包（否则任何用到它的地方都会被拖上
// 那条依赖链），因此转换放在这里。
func hopASNs(hops []trace.Hop) []string {
	out := make([]string, 0, len(hops))
	for _, hop := range hops {
		out = append(out, hop.ASN)
	}
	return out
}

// asnLabel 返回 hop 的 ASN 显示文本（含线路名称）。
func asnLabel(asn string) string {
	return asnmap.Label(asn)
}

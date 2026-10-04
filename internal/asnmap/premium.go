package asnmap

import "strings"

// 本文件定义"优质线路"：大陆方向那三条公认更好的线路。
//
// == 为什么这份清单只能有一份 ==
//
// 它有两个使用者，而且吃进去的东西不一样：
//
//   - service：手里是 ASN 列表（`resolveRoute` 返回的就是 ASN），
//     用 PremiumRank 判断日志里这条线路值不值得标出来；
//   - csvstore：手里是 CSV 的 as_path 短名（"CMNET > CN2"），
//     用 PremiumNameRank 决定结果表格里哪些排在最上面。
//
// 两处各存一份必然漂移：改了一边、另一边漏掉，表现只是"日志标红了
// 但表格没置顶"或者反过来——没有人会想到去核对两份清单。
// 因此清单放在这里（线路名的权威来源），两个包都来查。

// PremiumRoute 是一条优质线路的定义。
type PremiumRoute struct {
	// ASN 是这条线路的编号，例如 AS4809。
	ASN string

	// Name 是线路名，与 Route.Name 一致（CSV 的 as_path 里写的就是它）。
	Name string

	// Alias 是同一线路可能出现的其它写法。
	//
	// 存在的理由：同一个 ASN 在不同数据里可能只写编号（"9929"）
	// 或者写成别的形式。匹配时一并接受，免得"明明走了这条线路却没
	// 被认出来"——那种失败没有任何提示。
	Alias []string
}

// PremiumRoutes 是优质线路，**按优先级从高到低**。
//
// 顺序即优先级，两个使用者都依赖它：日志里若一条路径同时经过多条，
// 取等级最高的那条；表格里也按这个顺序分组置顶。
var PremiumRoutes = []PremiumRoute{
	{ASN: "AS58807", Name: "CMIN2"},
	{ASN: "AS4809", Name: "CN2"},
	{ASN: "AS9929", Name: "9929/CUII", Alias: []string{"9929", "CUII"}},
}

// PremiumRank 返回该 ASN 的优先级；-1 表示不是优质线路。
func PremiumRank(asn string) int {
	normalized := Normalize(asn)
	for rank, route := range PremiumRoutes {
		if route.ASN == normalized {
			return rank
		}
	}
	return -1
}

// PremiumPathRank 返回一组 ASN 里**等级最高**的那条优质线路。
//
// 一条路径可能同时经过多条（例如 "CN2 > CMIN2"），这时取最高的：
// 它确实走了那条更好的线路，只报低的那条会低估它。
func PremiumPathRank(asns []string) int {
	for rank, route := range PremiumRoutes {
		for _, asn := range asns {
			if Normalize(asn) == route.ASN {
				return rank
			}
		}
	}
	return -1
}

// PremiumNameRank 按**线路名**判断优先级；-1 表示不是优质线路。
//
// 给只用得上短名的调用方（CSV 的 as_path），例如结果表格的排序。
func PremiumNameRank(name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}

	for rank, route := range PremiumRoutes {
		if strings.EqualFold(name, route.Name) {
			return rank
		}
		for _, alias := range route.Alias {
			if strings.EqualFold(name, alias) {
				return rank
			}
		}
	}
	return -1
}

// IsPremiumASN 报告该 ASN 是否属于优质线路。
func IsPremiumASN(asn string) bool { return PremiumRank(asn) >= 0 }

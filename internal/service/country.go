package service

import (
	"context"
	"sort"
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 本文件是"按国家挑目标"的实现。
//
// == 为什么只认 CCA2 ==
//
// 上游数据里 target.Location 有两个国家字段（Country 与 CCA2），
// 实测 14720 个目标里有 2660 个（约 18%）**互相矛盾**：
//
//	country=FI  cca2=SE   412 条
//	country=NL  cca2=DE   220 条
//	country=DE  cca2=FR   190 条
//
// 因此必须**只认一个**并让界面与筛选共用它。两边口径不同会得到
// 最糟糕的一类 bug：下拉框显示"美国 1388 个"，筛出来却是 1580 个，
// 而且没有任何报错。
//
// 选 CCA2 的理由：它是 ISO 两位码、14720 条全部非空，而且
// 上游自己的 country_counts 也用两位码作键。

// filterByCountries 只保留指定国家的目标。
//
// 国家为空的目标**不会**被选中：把它们算进任何一个国家都是猜。
// 想连它们一起测，就不要用国家筛选。
func filterByCountries(targets []model.Target, countries []string) []model.Target {
	wanted := normalizeCountries(countries)
	if len(wanted) == 0 {
		// 全是空白：视为没有筛选条件，而不是"筛掉所有"。
		// 后者会让界面上的空输入框把结果清空，很难理解。
		return targets
	}

	out := make([]model.Target, 0, len(targets))
	for _, target := range targets {
		if wanted[targetLocationCCA2(target)] {
			out = append(out, target)
		}
	}
	return out
}

// normalizeCountries 把国家列表归一化成集合。
//
// 大小写与首尾空白都容忍：使用者会写 "us"、也会从别处粘 " US "。
func normalizeCountries(countries []string) map[string]bool {
	out := make(map[string]bool, len(countries))
	for _, country := range countries {
		code := strings.ToUpper(strings.TrimSpace(country))
		if code == "" {
			continue
		}
		out[code] = true
	}
	return out
}

// targetsByCountry 统计一份目标列表里每个国家的数量。
//
// 界面用它填下拉框。**必须与 filterByCountries 用同一个字段**
// （见文件头的说明），否则数字会对不上。
func targetsByCountry(targets []model.Target) []CountryCount {
	counts := make(map[string]int)
	for _, target := range targets {
		code := targetLocationCCA2(target)
		if code == "" {
			continue
		}
		counts[code]++
	}

	out := make([]CountryCount, 0, len(counts))
	for code, count := range counts {
		out = append(out, CountryCount{CCA2: code, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].CCA2 < out[j].CCA2
	})
	return out
}

// CountryCount 是一个国家及其目标数。
type CountryCount struct {
	CCA2  string `json:"cca2"`
	Count int    `json:"count"`
}

// TargetCountries 返回目标列表里每个国家的数量，以及目标总数。
//
// 界面用它填第一步的国家下拉框。数量与筛选共用同一个字段
// （Location.CCA2），因此下拉框里的数字就是筛出来的数字。
//
// 会加载（必要时下载）目标列表，因此可能耗时数秒。
func (s *Service) TargetCountries(ctx context.Context) ([]CountryCount, int, error) {
	targets, _, _, err := s.loadTargets(ctx)
	if err != nil {
		return nil, 0, err
	}
	return targetsByCountry(targets), len(targets), nil
}

// summarizeCountries 把国家统计压成一行短文本，用于报错。
//
// 只列前几个：使用者真正需要的是"原来有这么些国家"这个事实，
// 以及去界面里看完整列表——把 70 个国家全塞进错误信息没人读。
func summarizeCountries(counts []CountryCount) string {
	if len(counts) == 0 {
		return "（无）"
	}
	const limit = 8

	parts := make([]string, 0, limit+1)
	for i, count := range counts {
		if i == limit {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, count.CCA2)
	}
	return strings.Join(parts, ", ")
}

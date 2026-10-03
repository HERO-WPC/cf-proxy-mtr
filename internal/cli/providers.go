package cli

import (
	"strings"

	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// providerList 返回数据源的可选值（用于 --help）。
//
// 从 trace.DataProviders() 生成而不是写死字符串：
// 手工维护的列表迟早与真正接受的值漂移，
// 而"帮助里列了但用不了"是最烦人的那类问题。
func providerList() string {
	providers := trace.DataProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, string(provider))
	}
	return strings.Join(names, " / ")
}

// powProviderList 返回 PoW 源的可选值（用于 --help）。
func powProviderList() string {
	providers := trace.PowProviders()
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, string(provider))
	}
	return strings.Join(names, " / ")
}

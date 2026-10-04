package cli

import (
	"fmt"
	"strings"
)

// countryList 是可以重复出现、也接受逗号分隔的国家代码列表。
//
// == 为什么需要单独一个类型 ==
//
// 现有的 stringList 把每次出现的值**原样**追加，不拆逗号。国家对
// 使用者来说是自然的逗号列表（`--target-country US,DE`），用它就会
// 得到元素 "US,DE"——一个谁都匹配不上的值，而且**不会报错**，
// 表现只是"筛完一个目标都没有"。这种失败方式最难查。
//
// 因此这里显式拆开，并把大小写归一化（使用者会写 us、US、Us）。
type countryList []string

// String 实现 flag.Value。
func (l *countryList) String() string { return strings.Join(*l, ",") }

// Set 实现 flag.Value，按逗号拆分。
func (l *countryList) Set(value string) error {
	added := false
	for _, part := range strings.Split(value, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if code == "" {
			continue
		}
		*l = append(*l, code)
		added = true
	}
	if !added {
		return fmt.Errorf("empty country code")
	}
	return nil
}

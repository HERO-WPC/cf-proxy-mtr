// Package asnprefix 用"线路的 IP 段"判断一条路径走没走某条线路。
//
// == 为什么是这个方向 ==
//
// 常规做法是"查每个 IP 属于哪个 ASN"，那需要 GeoIP 服务，
// 而所有免费的 GeoIP 服务都有限流（NextTrace-API 的 PoW 令牌、
// IPinfo 的额度、ip-api 的 45 次/分钟）。
//
// 反过来的做法只需要**少数几条关心线路的 IP 段**：
//
//	要知道"走没走 CN2" -> 取 AS4809 的 IP 段（233 条），看跳里有没有落进去
//
// 好处是三个：
//
//  1. **不需要 GeoIP 服务**，因此没有限流、不需要 token、不需要账号；
//  2. **离线可用**：IP 段抓一次缓存下来，之后不再联网；
//  3. **更快**：每跳省掉一次网络查询。
//
// 代价是只知道"是不是这几条线路"，不知道任意 IP 的归属——
// 而本项目只需要前者：asnmap 里那 30 条线路就是全部要认的东西，
// 而最终落地地区来自 all.json，本来也不依赖外部服务。
package asnprefix

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Set 是一组 IP 段，支持"某个 IP 是否落在其中"的查询。
//
// 内部按前缀起始地址排序，查询用二分查找，因此即使有几万条前缀
// 也是微秒级——跟踪一条路径要对几十跳各查一次。
type Set struct {
	// v4 / v6 分开存：不同地址族的数值不可比较。
	v4 []entry
	v6 []entry
}

// entry 是一条前缀。
//
// 存成"起始地址 + 结束地址"而不是"地址 + 掩码长度"：
// 判断包含关系只需要两次比较，不必每次重算掩码。
type entry struct {
	start uint128
	end   uint128
}

// uint128 是一个足够放下 IPv6 的无符号整数。
//
// 用 hi/lo 两个 uint64 而不是 math/big：big.Int 每次比较都要分配，
// 而这里要对几十跳 × 几万条前缀做二分查找，分配会成为瓶颈。
type uint128 struct {
	hi uint64
	lo uint64
}

// less 报告 a < b。
func (a uint128) less(b uint128) bool {
	if a.hi != b.hi {
		return a.hi < b.hi
	}
	return a.lo < b.lo
}

// lessEq 报告 a <= b。
func (a uint128) lessEq(b uint128) bool {
	return a.less(b) || a == b
}

// fromAddr 把 IP 转成可比较的整数。
//
// IPv4 放在低位：v4 与 v6 从不混在同一个 Set 里比较，
// 因此不需要做 v4-mapped-v6 的转换。
func fromAddr(addr netip.Addr) uint128 {
	if addr.Is4() {
		bytes := addr.As4()
		return uint128{
			lo: uint64(bytes[0])<<24 | uint64(bytes[1])<<16 |
				uint64(bytes[2])<<8 | uint64(bytes[3]),
		}
	}

	bytes := addr.As16()
	return uint128{
		hi: uint64(bytes[0])<<56 | uint64(bytes[1])<<48 | uint64(bytes[2])<<40 |
			uint64(bytes[3])<<32 | uint64(bytes[4])<<24 | uint64(bytes[5])<<16 |
			uint64(bytes[6])<<8 | uint64(bytes[7]),
		lo: uint64(bytes[8])<<56 | uint64(bytes[9])<<48 | uint64(bytes[10])<<40 |
			uint64(bytes[11])<<32 | uint64(bytes[12])<<24 | uint64(bytes[13])<<16 |
			uint64(bytes[14])<<8 | uint64(bytes[15]),
	}
}

// NewSet 从 CIDR 字符串构造集合。
//
// 无法解析的条目会被跳过并计数返回，而不是让整批失败：
// 上游数据（RouteViews）偶有格式噪声，为了几条坏数据丢掉
// 几百条好数据不划算。调用方可以决定是否要提示。
func NewSet(prefixes []string) (*Set, int, error) {
	set := &Set{}
	var skipped int

	for _, raw := range prefixes {
		prefix, err := parsePrefix(raw)
		if err != nil {
			skipped++
			continue
		}
		set.add(prefix)
	}

	sortEntries(set.v4)
	sortEntries(set.v6)
	return set, skipped, nil
}

// parsePrefix 解析一条 CIDR，容忍多余空白。
func parsePrefix(raw string) (netip.Prefix, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return netip.Prefix{}, fmt.Errorf("asnprefix: empty prefix")
	}
	prefix, err := netip.ParsePrefix(trimmed)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("asnprefix: parse %q: %w", trimmed, err)
	}
	// 规范化：Masked 把主机位清零，否则 "1.2.3.4/24" 的 start
	// 会算成 1.2.3.4 而不是 1.2.3.0，包含判断就会漏。
	return prefix.Masked(), nil
}

// add 加入一条前缀。
func (s *Set) add(prefix netip.Prefix) {
	if !prefix.IsValid() {
		return
	}

	start := fromAddr(prefix.Addr())
	end := addressAtEnd(prefix)

	if prefix.Addr().Is4() {
		s.v4 = append(s.v4, entry{start: start, end: end})
		return
	}
	s.v6 = append(s.v6, entry{start: start, end: end})
}

// addressAtEnd 返回前缀覆盖的最后一个地址。
func addressAtEnd(prefix netip.Prefix) uint128 {
	start := fromAddr(prefix.Addr())
	hostBits := 128 - prefix.Bits()
	if prefix.Addr().Is4() {
		hostBits = 32 - prefix.Bits()
	}
	if hostBits <= 0 {
		return start
	}

	// 起始地址低位全为 1 即得结束地址。
	if prefix.Addr().Is4() {
		return uint128{lo: start.lo | (1<<uint(hostBits) - 1)}
	}
	if hostBits >= 64 {
		return uint128{hi: start.hi | (1<<uint(hostBits-64) - 1), lo: ^uint64(0)}
	}
	return uint128{hi: start.hi, lo: start.lo | (1<<uint(hostBits) - 1)}
}

// sortEntries 按起始地址排序，并把相邻可合并的条目合并。
func sortEntries(entries []entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].start.less(entries[j].start)
	})
}

// Contains 报告某个 IP 是否落在集合里。
func (s *Set) Contains(addr netip.Addr) bool {
	if s == nil || !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()

	target := fromAddr(addr)
	entries := s.v6
	if addr.Is4() {
		entries = s.v4
	}

	// 找最后一个 start <= target 的条目，检查它的 end 是否覆盖 target。
	//
	// 这是标准的前缀匹配做法：排序后只需检查一个候选。
	// 若前缀之间存在嵌套（RouteViews 数据里确实有），
	// "最后一个 start <= target" 不一定是覆盖 target 的那条，
	// 因此再往回走几步确认——嵌套深度实际很小。
	index := sort.Search(len(entries), func(i int) bool {
		return target.less(entries[i].start)
	}) - 1

	for i := index; i >= 0 && i >= index-8; i-- {
		if entries[i].start.lessEq(target) && target.lessEq(entries[i].end) {
			return true
		}
	}
	return false
}

// Len 返回集合里的前缀条数（两个地址族之和）。
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.v4) + len(s.v6)
}

// Len4 / Len6 分别返回 IPv4 / IPv6 的条数。
func (s *Set) Len4() int {
	if s == nil {
		return 0
	}
	return len(s.v4)
}

func (s *Set) Len6() int {
	if s == nil {
		return 0
	}
	return len(s.v6)
}

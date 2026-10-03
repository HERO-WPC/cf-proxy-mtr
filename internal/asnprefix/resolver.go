package asnprefix

import (
	"net/netip"
	"sort"
	"strings"
)

// Match 返回某个 IP 所属的已知线路（ASN 列表，按 asnmap 的固定顺序）。
//
// 返回**全部**命中的线路而不是第一个：一个 IP 可能同时落在多条
// 线路的宣告里（转售、代理、嵌套宣告）。挑一个就等于丢信息，
// 而调用方可以自己决定怎么呈现。
//
// 传入空串、非法 IP、私有地址都会返回 nil。
func (r *Resolver) Match(ip string) []string {
	if r == nil {
		return nil
	}

	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return nil
	}
	addr = addr.Unmap()

	// 私有地址不可能属于任何骨干线路，早点返回省掉两次二分查找。
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var matched []string
	for _, asn := range r.asnOrder {
		set, ok := r.byASN[asn]
		if !ok || set == nil {
			continue
		}
		if set.Contains(addr) {
			matched = append(matched, asn)
		}
	}
	return matched
}

// ResolvePath 把一条路径的跳 IP 转成"线路序列"。
//
// 输入是逐跳的 IP（超时跳用空串表示），输出是按跳序去重后的 ASN 列表。
//
// == 为什么按跳序去重 ==
//
// 同一条线路通常会连续出现在好几跳上（比如 CN2 的入口与出口都在
// AS4809 里）。逐跳输出会得到 "4809, 4809, 4809, 58453"，
// 而使用者想看的是"走了哪几条线路"——即 "4809 > 58453"。
//
// 只去掉**相邻重复**，不全局去重：一条路径可能先走 CN2、
// 出去绕一圈又回到 CN2，那是真实且有意义的信息
// （例如"CN2 出去，绕美国，又回到 CN2"），全局去重会抹掉它。
func (r *Resolver) ResolvePath(hopIPs []string) []string {
	if r == nil {
		return nil
	}

	var (
		out  []string
		last string
	)

	for _, ip := range hopIPs {
		for _, asn := range r.Match(ip) {
			if asn == last {
				continue
			}
			out = append(out, asn)
			last = asn
		}
	}
	return out
}

// PathWithNames 把线路序列转成带名称的展示串。
//
// 形如 "CN2(AS4809) > CMI(AS58453)"。名称取自 asnmap，
// 找不到名称时只留编号——宁可能核对，也不要编一个名字。
//
// namer 由调用方注入，避免本包依赖 asnmap（asnmap 不依赖本包，
// 但保持单向依赖让两者都能单独测试）。
func PathWithNames(asns []string, namer func(asn string) string) string {
	if len(asns) == 0 {
		return ""
	}

	parts := make([]string, 0, len(asns))
	for _, asn := range asns {
		name := ""
		if namer != nil {
			name = namer(asn)
		}
		if name == "" {
			parts = append(parts, asn)
			continue
		}
		parts = append(parts, name+"("+asn+")")
	}
	return strings.Join(parts, " > ")
}

// Size 返回已加载的 ASN 数量与前缀总数（供诊断输出）。
func (r *Resolver) Size() (asns int, prefixes int, ipv4 int, ipv6 int) {
	if r == nil {
		return 0, 0, 0, 0
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, set := range r.byASN {
		prefixes += set.Len()
		ipv4 += set.Len4()
		ipv6 += set.Len6()
	}
	return len(r.byASN), prefixes, ipv4, ipv6
}

// ASNs 返回已加载的 ASN 列表（固定顺序）。
func (r *Resolver) ASNs() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.byASN))
	for _, asn := range r.asnOrder {
		if _, ok := r.byASN[asn]; ok {
			out = append(out, asn)
		}
	}
	return out
}

// Ready 报告是否至少加载了一个 ASN 的前缀。
func (r *Resolver) Ready() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byASN) > 0
}

// sortedASNs 返回排序后的 ASN 列表（测试与诊断用）。
func sortedASNs(asns []string) []string {
	out := append([]string(nil), asns...)
	sort.Strings(out)
	return out
}

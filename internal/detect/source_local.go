package detect

import (
	"context"
	"fmt"
	"net"
)

// ---------------------------------------------------------------------------
// 本地源：完全不联网
// ---------------------------------------------------------------------------

// LocalSource 从本机网络栈推断**能可靠推断的那一个字段**：出口 IP 版本。
//
// 它刻意什么外部服务都不联系，因此永远不会把本机 IP 透露给第三方。
//
// 为什么只推断 IP 版本，而不猜国家 / 城市 / 运营商：
//
//	离线推断地理位置的唯一"技巧"是问公共 DNS 解析器"我的地址是什么"，
//	但那返回的是**解析器自己**的出口地址，不是用户的；
//	用它去查 ASN 得到的是 DNS 服务商（Cloudflare / Google）的 ASN，
//	而不是用户接入的运营商。把它写进采集者画像就是**错误数据**，
//	而错误的地区/运营商分组会直接毁掉整个众测数据库的可比性。
//
//	所以这里宁可少给一个字段，也不给一个错的。
type LocalSource struct {
	// dialContext 允许注入拨号实现（测试用）。
	dialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewLocalSource 创建本地源。
func NewLocalSource() *LocalSource { return &LocalSource{} }

// NewLocalSourceWithDialer 创建使用自定义拨号的本地源（测试用）。
func NewLocalSourceWithDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) *LocalSource {
	return &LocalSource{dialContext: dial}
}

// Name 实现 Source。
func (s *LocalSource) Name() string { return "local" }

// Describe 实现 Source。
func (s *LocalSource) Describe() string {
	return "inspects the local network stack for an egress IP version; contacts no external service " +
		"(country/ISP cannot be determined offline without guessing)"
}

// ExposesLocalIP 实现 Source。本地源永远不暴露。
func (s *LocalSource) ExposesLocalIP() bool { return false }

// Detect 实现 Source。
func (s *LocalSource) Detect(ctx context.Context) (Values, error) {
	dial := s.dialContext
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}

	// 探测两个地址族是否可用。
	//
	// 用 UDP "dial" 到文档用途地址（RFC 5737 / RFC 3849）：
	// UDP 的 connect 只在本机内核里确定出接口与地址，**不会发出任何报文**，
	// 因此这一步是真的零网络流量。选文档地址而不是公网地址，
	// 是为了即使某个平台意外发包，也只会打在保留地址上。
	v4 := probeFamily(ctx, dial, "udp4", "192.0.2.1:9")
	v6 := probeFamily(ctx, dial, "udp6", "[2001:db8::1]:9")

	var out Values
	switch {
	case v4 && v6:
		// 双栈时无法只凭本机判断出口走哪一族：那取决于目标地址与
		// 路由策略。留空比猜错好，也如实反映"离线测不出来"。
		out.IPVersion = ""
	case v4:
		out.IPVersion = "ipv4"
	case v6:
		out.IPVersion = "ipv6"
	default:
		return out, fmt.Errorf("no usable IP family detected on this host")
	}

	return out, nil
}

// probeFamily 报告本机能否在该地址族上"连接"（不产生实际流量）。
func probeFamily(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), network, address string) bool {
	conn, err := dial(ctx, network, address)
	if err != nil {
		return false
	}
	// UDP 连接没有握手，关闭即可。
	_ = conn.Close()
	return true
}

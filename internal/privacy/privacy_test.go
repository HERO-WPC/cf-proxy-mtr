package privacy

import (
	"net"
	"testing"
)

// TestIsPrivateAddress 覆盖各类不可公开的地址。
//
// 这个测试是隐私约束的**第一道防线**：任何一类地址被漏判，
// 就意味着它可能被写进公开数据。
func TestIsPrivateAddress(t *testing.T) {
	cases := []struct {
		addr    string
		private bool
		why     string
	}{
		// 应当判为不可公开。
		{"127.0.0.1", true, "环回"},
		{"::1", true, "IPv6 环回"},
		{"0.0.0.0", true, "未指定"},
		{"::", true, "IPv6 未指定"},
		{"10.0.0.1", true, "RFC1918 10/8"},
		{"10.255.255.254", true, "RFC1918 10/8 边界"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"172.31.255.254", true, "RFC1918 172.16/12 上边界"},
		{"192.168.0.1", true, "RFC1918 192.168/16"},
		{"192.168.1.215", true, "常见家用网关"},
		{"169.254.1.1", true, "链路本地 APIPA"},
		{"100.64.0.1", true, "运营商级 NAT（能定位运营商内网）"},
		{"100.127.255.254", true, "CGNAT 上边界"},
		{"192.0.2.1", true, "文档用途 TEST-NET-1"},
		{"198.51.100.1", true, "文档用途 TEST-NET-2"},
		{"203.0.113.1", true, "文档用途 TEST-NET-3"},
		{"198.18.0.1", true, "基准测试"},
		{"240.0.0.1", true, "保留"},
		{"255.255.255.255", true, "广播"},
		{"224.0.0.1", true, "组播"},
		{"fe80::1", true, "IPv6 链路本地"},
		{"fc00::1", true, "IPv6 唯一本地"},
		{"fd12:3456::1", true, "IPv6 唯一本地"},
		{"2001:db8::1", true, "IPv6 文档用途"},
		{"::ffff:10.0.0.1", true, "IPv4 映射的私有地址"},
		{"::ffff:192.168.1.1", true, "IPv4 映射的家用地址"},

		// 应当判为可公开。
		{"1.1.1.1", false, "Cloudflare"},
		{"8.8.8.8", false, "Google DNS"},
		{"45.63.67.144", false, "常见 VPS"},
		{"104.16.0.1", false, "Cloudflare 边缘"},
		{"172.15.255.255", false, "紧邻 RFC1918 之下的公网地址"},
		{"172.32.0.1", false, "紧邻 RFC1918 之上的公网地址"},
		{"100.63.255.255", false, "紧邻 CGNAT 之下的公网地址"},
		{"100.128.0.1", false, "紧邻 CGNAT 之上的公网地址"},
		{"192.0.3.1", false, "紧邻 TEST-NET-1 之外"},
		{"198.20.0.1", false, "紧邻基准测试之外"},
		{"2606:4700::1", false, "Cloudflare IPv6"},
		{"2001:4860:4860::8888", false, "Google IPv6"},
		{"2400:3200::1", false, "阿里 IPv6"},
	}

	// 组播（含 239/8 管理范围组播）不可公开：它虽然不属于"保留段"，
	// 但把组播地址写进公开数据同样没有意义，而且会污染统计。
	for _, addr := range []string{"224.0.0.1", "239.255.255.255", "ff02::1"} {
		t.Run("multicast "+addr, func(t *testing.T) {
			if !IsPrivateAddress(net.ParseIP(addr)) {
				t.Errorf("IsPrivateAddress(%s) = false, want true (multicast is not public data)", addr)
			}
		})
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			ip := net.ParseIP(tc.addr)
			if ip == nil {
				t.Fatalf("test bug: %q is not a valid IP", tc.addr)
			}
			if got := IsPrivateAddress(ip); got != tc.private {
				t.Errorf("IsPrivateAddress(%s) = %v, want %v (%s)",
					tc.addr, got, tc.private, tc.why)
			}
			// 字符串入口必须给出一致结论。
			if got := IsPrivateString(tc.addr); got != tc.private {
				t.Errorf("IsPrivateString(%s) = %v, want %v", tc.addr, got, tc.private)
			}
		})
	}
}

// TestAddressBoundaryDetails 单独确认几处最容易写错一位的边界。
func TestAddressBoundaryDetails(t *testing.T) {
	// 239/8 是"管理范围组播"，不属于保留段，但仍是组播 —— 不可公开。
	// 这里同时断言 240/4 与 239/8 都不可公开，且原因是各自那条规则。
	for _, addr := range []string{"239.0.0.1", "240.0.0.1"} {
		if !IsPrivateAddress(net.ParseIP(addr)) {
			t.Errorf("IsPrivateAddress(%s) = false, want true", addr)
		}
	}
}

// TestIsPrivateStringEdgeCases 覆盖字符串入口的边界。
func TestIsPrivateStringEdgeCases(t *testing.T) {
	cases := []struct {
		in      string
		private bool
		why     string
	}{
		{"", false, "空字符串表示'没有地址'，不泄露任何东西"},
		{"   ", false, "只有空白等同于空"},
		{"not-an-ip", true, "解析不了就无法证明可公开"},
		{"192.168.1.1", true, "普通私有地址"},
		{"fe80::1%eth0", true, "带 zone 的链路本地地址"},
		{"  1.1.1.1  ", false, "两侧空白应被忽略"},
		{"1.2.3.4:443", true, "带端口的地址不是合法 IP，按不可公开处理"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := IsPrivateString(tc.in); got != tc.private {
				t.Errorf("IsPrivateString(%q) = %v, want %v (%s)", tc.in, got, tc.private, tc.why)
			}
		})
	}

	// nil 必须按"不可公开"处理（宁可少公开，不可多公开）。
	if !IsPrivateAddress(nil) {
		t.Error("IsPrivateAddress(nil) = false, want true (fail closed)")
	}
}

// TestRedact 覆盖替换行为。
func TestRedact(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"1.1.1.1":      "1.1.1.1",
		"2606:4700::1": "2606:4700::1",
		"192.168.1.1":  RedactedIPv4,
		"10.0.0.1":     RedactedIPv4,
		"100.64.0.1":   RedactedIPv4,
		"fe80::1":      RedactedIPv6,
		"fc00::1":      RedactedIPv6,
		"2001:db8::1":  RedactedIPv6,
		// IPv4 映射地址按 IPv4 归类：占位符要说明"这是个 v4 内网地址"。
		"::ffff:192.168.1.1": RedactedIPv4,
	}

	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}

	// 无法解析的地址也必须被替换掉（fail closed）。
	if got := Redact("garbage"); got == "garbage" {
		t.Error("Redact left an unparseable address untouched")
	}
}

// TestClassifyTarget 覆盖目标分类。
func TestClassifyTarget(t *testing.T) {
	cases := map[string]TargetKind{
		"1.1.1.1":      TargetPublic,
		"45.63.67.144": TargetPublic,
		"2606:4700::1": TargetPublic,
		"192.168.1.1":  TargetPrivate,
		"10.0.0.1":     TargetPrivate,
		"127.0.0.1":    TargetPrivate,
		"100.64.0.1":   TargetPrivate,
		"203.0.113.5":  TargetPrivate,
		"":             TargetInvalid,
		"nonsense":     TargetInvalid,
		"1.2.3.4:443":  TargetInvalid,
	}

	for in, want := range cases {
		if got := ClassifyTarget(in); got != want {
			t.Errorf("ClassifyTarget(%q) = %v, want %v", in, got, want)
		}
	}

	if !PublicTarget("1.1.1.1") {
		t.Error("PublicTarget(1.1.1.1) = false")
	}
	if PublicTarget("192.168.1.1") {
		t.Error("PublicTarget(192.168.1.1) = true, private targets must not be public")
	}
	if PublicTarget("") {
		t.Error("PublicTarget(\"\") = true, invalid targets must not be public")
	}
}

// TestTargetKindString 覆盖展示文本。
func TestTargetKindString(t *testing.T) {
	for kind, want := range map[TargetKind]string{
		TargetPublic:  "public",
		TargetPrivate: "private",
		TargetInvalid: "invalid",
	} {
		if got := kind.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", kind, got, want)
		}
	}
}

// TestCoordinatesForTarget 验证坐标只在真的存在时才被公开。
func TestCoordinatesForTarget(t *testing.T) {
	// 没有坐标：不公开。
	coordinate := CoordinatesForTarget(nil, nil)
	if coordinate.Set {
		t.Error("Set = true for missing coordinates")
	}

	// 只有一半：也不公开（单边坐标是无意义的数据）。
	lat := 37.3
	coordinate = CoordinatesForTarget(&lat, nil)
	if coordinate.Set {
		t.Error("Set = true for half a coordinate")
	}

	// 完整坐标：公开。
	lng := -121.8
	coordinate = CoordinatesForTarget(&lat, &lng)
	if !coordinate.Set || coordinate.Lat != lat || coordinate.Lng != lng {
		t.Errorf("coordinate = %+v, want {%v %v true}", coordinate, lat, lng)
	}

	// (0,0) 是几内亚湾的合法坐标，必须被当作"有坐标"而不是"缺失"。
	zero := 0.0
	coordinate = CoordinatesForTarget(&zero, &zero)
	if !coordinate.Set {
		t.Error("Set = false for (0,0); it is a legal coordinate in the Gulf of Guinea")
	}
}

package model

import (
	"net/netip"
	"testing"
)

func TestNewTargetAndID(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		port    int
		wantID  string
		wantVer IPVersion
		wantOK  bool
	}{
		{"ipv4", "1.2.3.4", 443, "1.2.3.4:443", IPVersionIPv4, true},
		{"ipv4 high port", "1.2.3.4", 65535, "1.2.3.4:65535", IPVersionIPv4, true},
		{"ipv6 compressed", "2001:db8::1", 8443, "[2001:db8::1]:8443", IPVersionIPv6, true},
		{"port zero", "1.2.3.4", 0, "", "", false},
		{"port negative", "1.2.3.4", -1, "", "", false},
		{"port too large", "1.2.3.4", 65536, "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tc.ip)
			if err != nil {
				t.Fatalf("ParseAddr(%q): %v", tc.ip, err)
			}

			target, ok := NewTarget(addr, tc.port)
			if ok != tc.wantOK {
				t.Fatalf("NewTarget ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if target.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", target.ID, tc.wantID)
			}
			if target.IPVersion != tc.wantVer {
				t.Errorf("IPVersion = %q, want %q", target.IPVersion, tc.wantVer)
			}
			if target.String() != tc.wantID {
				t.Errorf("String() = %q, want %q", target.String(), tc.wantID)
			}
		})
	}
}

func TestNewTargetRejectsInvalidAddr(t *testing.T) {
	if _, ok := NewTarget(netip.Addr{}, 443); ok {
		t.Error("NewTarget accepted zero Addr, want false")
	}
}

func TestNewTargetStripsZone(t *testing.T) {
	// zone 是本机信息（例如网卡名），绝不能进入数据库或公开数据。
	addr := netip.MustParseAddr("fe80::1%eth0")
	target, ok := NewTarget(addr, 443)
	if !ok {
		t.Fatal("NewTarget failed")
	}
	if target.IP != "fe80::1" {
		t.Errorf("IP = %q, want fe80::1 (zone stripped)", target.IP)
	}
	if target.AddrMustString() != "fe80::1" {
		t.Errorf("Addr = %q, want fe80::1", target.AddrMustString())
	}
}

func TestTargetAddrPort(t *testing.T) {
	target := Target{ID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443}

	ap, ok := target.AddrPort()
	if !ok {
		t.Fatal("AddrPort failed")
	}
	if ap.String() != "1.2.3.4:443" {
		t.Errorf("AddrPort = %q, want 1.2.3.4:443", ap)
	}

	bad := Target{IP: "not-an-ip", Port: 443}
	if _, ok := bad.AddrPort(); ok {
		t.Error("AddrPort succeeded for invalid IP, want false")
	}
	if _, ok := bad.Addr(); ok {
		t.Error("Addr succeeded for invalid IP, want false")
	}
}

func TestTargetStringFallsBackToConstructedID(t *testing.T) {
	// ID 为空时（例如手工构造的 Target）仍要输出可用标识。
	target := Target{IP: "1.2.3.4", Port: 8443}
	if got := target.String(); got != "1.2.3.4:8443" {
		t.Errorf("String() = %q, want 1.2.3.4:8443", got)
	}
}

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1.2.3.4", "1.2.3.4", true},
		{" 1.2.3.4 ", "1.2.3.4", true},
		{"::ffff:1.2.3.4", "1.2.3.4", true}, // IPv4-mapped 归一化
		{"2001:0db8::1", "2001:db8::1", true},
		{"fe80::1%eth0", "fe80::1", true},
		{"", "", false},
		{"1.2.3.4:443", "", false},
		{"not-an-ip", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseAddr(tc.in)
		if ok != tc.ok {
			t.Errorf("ParseAddr(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got.String() != tc.want {
			t.Errorf("ParseAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidPort(t *testing.T) {
	for port, want := range map[int]bool{
		-1: false, 0: false, 1: true, 22: true, 443: true, 65535: true, 65536: false,
	} {
		if got := ValidPort(port); got != want {
			t.Errorf("ValidPort(%d) = %v, want %v", port, got, want)
		}
	}
}

func TestIPVersionOf(t *testing.T) {
	if got := IPVersionOf(netip.MustParseAddr("1.2.3.4")); got != IPVersionIPv4 {
		t.Errorf("ipv4 -> %q", got)
	}
	if got := IPVersionOf(netip.MustParseAddr("::1")); got != IPVersionIPv6 {
		t.Errorf("ipv6 -> %q", got)
	}
	if got := IPVersionOf(netip.Addr{}); got != IPVersionUnknown {
		t.Errorf("invalid -> %q", got)
	}
}

func TestLocationHelpers(t *testing.T) {
	if !(Location{}).IsZero() {
		t.Error("zero Location IsZero = false, want true")
	}
	// 只有坐标也算"有信息"，因为 0,0 是合法坐标。
	withCoords := Location{HasCoordinates: true}
	if withCoords.IsZero() {
		t.Error("Location with coordinates IsZero = true, want false")
	}

	cases := []struct {
		loc  Location
		want string
	}{
		{Location{Country: "US", IATA: "ORD", City: "Chicago"}, "US/ORD/Chicago"},
		{Location{Country: "US", City: "Chicago"}, "US/Chicago"},
		{Location{IATA: "NRT"}, "NRT"},
		{Location{}, "-"},
	}
	for _, tc := range cases {
		if got := tc.loc.String(); got != tc.want {
			t.Errorf("Location%+v.String() = %q, want %q", tc.loc, got, tc.want)
		}
	}
}

func TestCollectorProfileGroupKeyIsStable(t *testing.T) {
	profile := CollectorProfile{
		Country:   "CN",
		Province:  "Zhejiang",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "AS9808",
		IPVersion: IPVersionIPv4,
	}

	first := profile.GroupKey()
	second := profile.GroupKey()
	if first != second {
		t.Fatalf("GroupKey not stable: %q vs %q", first, second)
	}
	if first != "CN|Zhejiang|Hangzhou|China Mobile|AS9808|ipv4" {
		t.Errorf("GroupKey = %q", first)
	}

	// 字段不同必须产生不同的键，否则聚合会错误合并分组。
	other := profile
	other.ISP = "China Telecom"
	if other.GroupKey() == first {
		t.Error("GroupKey collision between different ISPs")
	}

	if !(CollectorProfile{}).IsZero() {
		t.Error("zero CollectorProfile IsZero = false, want true")
	}
	if profile.IsZero() {
		t.Error("filled CollectorProfile IsZero = true, want false")
	}
}

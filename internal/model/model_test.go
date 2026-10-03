package model

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// sampleTarget 返回一个字段完整的 Target，用于各用例做局部修改。
func sampleTarget() Target {
	return Target{
		ID:        "1.2.3.4:443",
		IP:        "1.2.3.4",
		Port:      443,
		IPVersion: IPVersionIPv4,
		Location: Location{
			Country:        "US",
			CCA2:           "US",
			Region:         "Illinois",
			City:           "Chicago",
			Latitude:       41.85003,
			Longitude:      -87.65005,
			CountryEN:      "United States",
			HasCoordinates: true,
		},
		Colo: ColoInfo{
			IATA:           "ORD",
			CCA2:           "US",
			Region:         "North America",
			City:           "Chicago",
			Latitude:       41.9786,
			Longitude:      -87.9048,
			HasCoordinates: true,
		},
	}
}

// ---------------------------------------------------------------------------
// Target 构造与不变量
// ---------------------------------------------------------------------------

func TestNewTarget(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		port    int
		wantID  string
		wantVer IPVersion
		wantOK  bool
	}{
		{"ipv4", "1.2.3.4", 443, "1.2.3.4:443", IPVersionIPv4, true},
		{"ipv4 low port", "1.2.3.4", 1, "1.2.3.4:1", IPVersionIPv4, true},
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
			// 构造出来的目标必须自洽。
			if err := target.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestNewTargetRejectsInvalidAddr(t *testing.T) {
	if _, ok := NewTarget(netip.Addr{}, 443); ok {
		t.Error("NewTarget accepted zero Addr, want false")
	}
}

// TestNewTargetNormalizesAddr 验证构造时就去掉 zone 并还原 IPv4-mapped IPv6。
func TestNewTargetNormalizesAddr(t *testing.T) {
	cases := []struct {
		in     string
		wantIP string
	}{
		{"fe80::1%eth0", "fe80::1"},
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"2001:0db8:0:0:0:0:0:1", "2001:db8::1"},
		{"1.2.3.4", "1.2.3.4"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.in)
			target, ok := NewTarget(addr, 443)
			if !ok {
				t.Fatal("NewTarget failed")
			}
			if target.IP != tc.wantIP {
				t.Errorf("IP = %q, want %q", target.IP, tc.wantIP)
			}
			if target.AddrMustString() != tc.wantIP {
				t.Errorf("AddrMustString() = %q, want %q", target.AddrMustString(), tc.wantIP)
			}
		})
	}
}

func TestNewTargetFromStrings(t *testing.T) {
	target, err := NewTargetFromStrings(" 1.2.3.4 ", 2053)
	if err != nil {
		t.Fatalf("NewTargetFromStrings: %v", err)
	}
	if target.ID != "1.2.3.4:2053" || target.Port != 2053 {
		t.Errorf("target = %+v, want 1.2.3.4:2053", target)
	}

	for name, tc := range map[string]struct {
		ip   string
		port int
	}{
		"bad ip":       {"not-an-ip", 443},
		"empty ip":     {"", 443},
		"bad port":     {"1.2.3.4", 0},
		"huge port":    {"1.2.3.4", 70000},
		"neg port":     {"1.2.3.4", -5},
		"ip with port": {"1.2.3.4:443", 443},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewTargetFromStrings(tc.ip, tc.port); err == nil {
				t.Fatalf("NewTargetFromStrings(%q, %d) succeeded, want error", tc.ip, tc.port)
			} else if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("error = %v, want ErrInvalidTarget", err)
			}
		})
	}
}

func TestTargetValidate(t *testing.T) {
	valid := sampleTarget()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}

	// IPv6 目标：ID 必须是方括号形式。
	v6, err := NewTargetFromStrings("2001:db8::1", 8443)
	if err != nil {
		t.Fatal(err)
	}
	if err := v6.Validate(); err != nil {
		t.Errorf("ipv6 target rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(*Target)
		want   string
	}{
		"empty ip":           {func(tg *Target) { tg.IP = "" }, "not a valid address"},
		"malformed ip":       {func(tg *Target) { tg.IP = "1.2.3" }, "not a valid address"},
		"ip with zone":       {func(tg *Target) { tg.IP = "fe80::1%eth0" }, "id"},
		"port zero":          {func(tg *Target) { tg.Port = 0; tg.ID = "1.2.3.4:0" }, "port 0"},
		"port too large":     {func(tg *Target) { tg.Port = 65536; tg.ID = "1.2.3.4:65536" }, "port 65536"},
		"id mismatch":        {func(tg *Target) { tg.ID = "1.2.3.4:8443" }, "does not match"},
		"empty id":           {func(tg *Target) { tg.ID = "" }, "does not match"},
		"wrong ip version":   {func(tg *Target) { tg.IPVersion = IPVersionIPv6 }, "ip_version"},
		"unknown ip version": {func(tg *Target) { tg.IPVersion = IPVersionUnknown }, "ip_version"},
		"bad location cca2":  {func(tg *Target) { tg.Location.CCA2 = "USA" }, "location.cca2"},
		"lowercase country":  {func(tg *Target) { tg.Location.Country = "us" }, "location.country"},
		"bad colo cca2":      {func(tg *Target) { tg.Colo.CCA2 = "1S" }, "colo.cca2"},
		"bad colo iata":      {func(tg *Target) { tg.Colo.IATA = "ORDX" }, "colo.iata"},
		"lowercase colo iata": {
			func(tg *Target) { tg.Colo.IATA = "ord" },
			"colo.iata",
		},
		"lat out of range": {
			func(tg *Target) { tg.Location.Latitude = 91 },
			"out of range",
		},
		"lon out of range": {
			func(tg *Target) { tg.Location.Longitude = -181 },
			"out of range",
		},
		"coords without flag": {
			func(tg *Target) { tg.Location.HasCoordinates = false },
			"has_coordinates is false",
		},
		"colo coords without flag": {
			func(tg *Target) { tg.Colo.HasCoordinates = false },
			"has_coordinates is false",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			target := sampleTarget()
			tc.mutate(&target)

			err := target.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.want)
			}
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("error = %v, want ErrInvalidTarget", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want message containing %q", err, tc.want)
			}
		})
	}
}

// TestTargetValidateAcceptsZeroCoordinates 确认 0,0 是合法坐标。
func TestTargetValidateAcceptsZeroCoordinates(t *testing.T) {
	target := sampleTarget()
	target.Location.Latitude = 0
	target.Location.Longitude = 0
	target.Location.HasCoordinates = true

	if err := target.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for legal 0,0 coordinates", err)
	}

	// 边界值也必须通过。
	target.Location.Latitude = -90
	target.Location.Longitude = 180
	if err := target.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for boundary coordinates", err)
	}
}

// TestTargetValidateAcceptsEmptyOptionalFields 确认元数据缺失不是错误。
func TestTargetValidateAcceptsEmptyOptionalFields(t *testing.T) {
	target, err := NewTargetFromStrings("9.9.9.9", 443)
	if err != nil {
		t.Fatal(err)
	}
	// 完全没有 Location 与 Colo 的目标必须合法：
	// 生产数据里确实有缺 colo 的记录，不能因此判定为非法数据。
	if err := target.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for target without metadata", err)
	}
	if !target.Location.IsZero() {
		t.Error("Location should be zero for a target built from ip:port only")
	}
}

// ---------------------------------------------------------------------------
// Normalize
// ---------------------------------------------------------------------------

func TestTargetNormalizeCanonicalizesFields(t *testing.T) {
	target := Target{
		ID:        "wrong-id",
		IP:        "::FFFF:1.2.3.4",
		Port:      443,
		IPVersion: IPVersionUnknown,
		Location: Location{
			Country:   " us ",
			CCA2:      "us",
			Region:    "  Illinois  ",
			City:      "Chicago ",
			CountryEN: " United States",
		},
		Colo: ColoInfo{IATA: " ord ", CCA2: "us", City: " Chicago"},
	}

	if !target.Normalize() {
		t.Fatal("Normalize() = false, want true for non-canonical input")
	}

	if target.ID != "1.2.3.4:443" {
		t.Errorf("ID = %q, want 1.2.3.4:443", target.ID)
	}
	if target.IP != "1.2.3.4" {
		t.Errorf("IP = %q, want 1.2.3.4", target.IP)
	}
	if target.IPVersion != IPVersionIPv4 {
		t.Errorf("IPVersion = %q, want ipv4", target.IPVersion)
	}
	if target.Location.Country != "US" || target.Location.CCA2 != "US" {
		t.Errorf("Location codes = %q/%q, want US/US", target.Location.Country, target.Location.CCA2)
	}
	if target.Location.Region != "Illinois" || target.Location.City != "Chicago" {
		t.Errorf("Location region/city = %q/%q, want trimmed", target.Location.Region, target.Location.City)
	}
	if target.Colo.IATA != "ORD" || target.Colo.CCA2 != "US" || target.Colo.City != "Chicago" {
		t.Errorf("Colo = %+v, want normalized", target.Colo)
	}
	if err := target.Validate(); err != nil {
		t.Errorf("Validate() after Normalize = %v, want nil", err)
	}

	// 幂等：再次归一化不应报告修改。
	if target.Normalize() {
		t.Error("Normalize() = true on already-canonical target, want false")
	}
}

func TestNormalizeDropsInvalidCoordinates(t *testing.T) {
	loc := Location{Latitude: 91, Longitude: 181, HasCoordinates: true}
	if !loc.Normalize() {
		t.Fatal("Normalize() = false, want true")
	}
	if loc.HasCoordinates {
		t.Error("HasCoordinates = true, want false for out-of-range coordinates")
	}
	// 数值本身不被改写：只调整"是否可信"的标志，不做静默数据修正。
	if loc.Latitude != 91 || loc.Longitude != 181 {
		t.Errorf("coordinates were silently rewritten: %v/%v", loc.Latitude, loc.Longitude)
	}
}

func TestNormalizeIsIdempotentAndNilSafe(t *testing.T) {
	var loc *Location
	if loc.Normalize() {
		t.Error("nil Location.Normalize() = true, want false")
	}
	var colo *ColoInfo
	if colo.Normalize() {
		t.Error("nil ColoInfo.Normalize() = true, want false")
	}
	var target *Target
	if target.Normalize() {
		t.Error("nil Target.Normalize() = true, want false")
	}

	canonical := sampleTarget()
	if canonical.Normalize() {
		t.Error("already canonical target reported a change")
	}
}

// ---------------------------------------------------------------------------
// Location / ColoInfo 语义
// ---------------------------------------------------------------------------

func TestLocationEffectiveCountry(t *testing.T) {
	cases := []struct {
		loc  Location
		want string
	}{
		{Location{CCA2: "NL", Country: "DE"}, "NL"},
		{Location{Country: "DE"}, "DE"},
		{Location{}, ""},
	}
	for _, tc := range cases {
		if got := tc.loc.EffectiveCountry(); got != tc.want {
			t.Errorf("EffectiveCountry(%+v) = %q, want %q", tc.loc, got, tc.want)
		}
	}
}

// TestResolveCoordinates 确认坐标回退是显式行为，且能区分来源。
func TestResolveCoordinates(t *testing.T) {
	withLoc := Location{Latitude: 1, Longitude: 2, HasCoordinates: true}
	withColo := ColoInfo{Latitude: 3, Longitude: 4, HasCoordinates: true}
	empty := Location{}
	emptyColo := ColoInfo{}

	lat, lon, ok, fromColo := withLoc.ResolveCoordinates(withColo)
	if !ok || fromColo || lat != 1 || lon != 2 {
		t.Errorf("target coordinates = (%v,%v,ok=%v,fromColo=%v), want (1,2,true,false)", lat, lon, ok, fromColo)
	}

	lat, lon, ok, fromColo = empty.ResolveCoordinates(withColo)
	if !ok || !fromColo || lat != 3 || lon != 4 {
		t.Errorf("colo fallback = (%v,%v,ok=%v,fromColo=%v), want (3,4,true,true)", lat, lon, ok, fromColo)
	}

	if _, _, ok, _ := empty.ResolveCoordinates(emptyColo); ok {
		t.Error("no coordinates available, want ok=false")
	}
}

func TestColoInfoIsZero(t *testing.T) {
	if !(ColoInfo{}).IsZero() {
		t.Error("zero ColoInfo IsZero = false, want true")
	}
	if (ColoInfo{IATA: "ORD"}).IsZero() {
		t.Error("ColoInfo with IATA IsZero = true, want false")
	}
}

func TestLocationAndColoString(t *testing.T) {
	loc := Location{CCA2: "US", Region: "Illinois", City: "Chicago"}
	if got := loc.String(); got != "US/Illinois/Chicago" {
		t.Errorf("Location.String() = %q", got)
	}
	if got := (Location{}).String(); got != "-" {
		t.Errorf("empty Location.String() = %q, want -", got)
	}

	colo := ColoInfo{IATA: "AMS", CCA2: "NL", City: "Amsterdam"}
	if got := colo.String(); got != "AMS/NL/Amsterdam" {
		t.Errorf("ColoInfo.String() = %q", got)
	}
	if got := (ColoInfo{}).String(); got != "-" {
		t.Errorf("empty ColoInfo.String() = %q, want -", got)
	}
}

func TestLocationIsZero(t *testing.T) {
	if !(Location{}).IsZero() {
		t.Error("zero Location IsZero = false, want true")
	}
	// 只有坐标也算"有信息"：0,0 是合法坐标。
	if (Location{HasCoordinates: true}).IsZero() {
		t.Error("Location with HasCoordinates IsZero = true, want false")
	}
	if (Location{CountryEN: "Japan"}).IsZero() {
		t.Error("Location with only CountryEN IsZero = true, want false")
	}
}

// ---------------------------------------------------------------------------
// 地址辅助
// ---------------------------------------------------------------------------

func TestTargetNetworkAndAddress(t *testing.T) {
	v4 := sampleTarget()
	if got := v4.Network(); got != "tcp4" {
		t.Errorf("Network() = %q, want tcp4", got)
	}
	if got := v4.Address(); got != "1.2.3.4:443" {
		t.Errorf("Address() = %q, want 1.2.3.4:443", got)
	}

	v6, err := NewTargetFromStrings("2001:db8::1", 8443)
	if err != nil {
		t.Fatal(err)
	}
	if got := v6.Network(); got != "tcp6" {
		t.Errorf("Network() = %q, want tcp6", got)
	}
	// IPv6 的 host:port 必须带方括号，net.Dial 才接受。
	if got := v6.Address(); got != "[2001:db8::1]:8443" {
		t.Errorf("Address() = %q, want [2001:db8::1]:8443", got)
	}

	broken := Target{IP: "not-an-ip", Port: 443}
	if got := broken.Network(); got != "tcp" {
		t.Errorf("broken Network() = %q, want tcp", got)
	}
}

func TestTargetAddrPort(t *testing.T) {
	target := sampleTarget()

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

func TestIPVersionOfAndValid(t *testing.T) {
	if got := IPVersionOf(netip.MustParseAddr("1.2.3.4")); got != IPVersionIPv4 {
		t.Errorf("ipv4 -> %q", got)
	}
	if got := IPVersionOf(netip.MustParseAddr("::1")); got != IPVersionIPv6 {
		t.Errorf("ipv6 -> %q", got)
	}
	if got := IPVersionOf(netip.Addr{}); got != IPVersionUnknown {
		t.Errorf("invalid -> %q", got)
	}

	for v, want := range map[IPVersion]bool{
		IPVersionIPv4: true, IPVersionIPv6: true, IPVersionUnknown: true,
		"": false, "IPv4": false, "ipv5": false,
	} {
		if got := v.Valid(); got != want {
			t.Errorf("IPVersion(%q).Valid() = %v, want %v", v, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Merge / Equal / SameEndpoint
// ---------------------------------------------------------------------------

func TestTargetMergeFillsOnlyMissingFields(t *testing.T) {
	sparse, err := NewTargetFromStrings("1.2.3.4", 443)
	if err != nil {
		t.Fatal(err)
	}
	rich := sampleTarget()

	merged := sparse.Merge(rich)
	if merged.Location.City != "Chicago" || merged.Colo.IATA != "ORD" {
		t.Errorf("merged = %+v, want metadata filled from other", merged)
	}
	if !merged.Location.HasCoordinates || merged.Location.Latitude != 41.85003 {
		t.Errorf("merged coordinates = %+v, want filled", merged.Location)
	}
	if err := merged.Validate(); err != nil {
		t.Errorf("merged Validate() = %v, want nil", err)
	}

	// 反向合并：已有值不能被覆盖。
	back := rich.Merge(sparse)
	if back.Location.City != "Chicago" || back.Colo.IATA != "ORD" {
		t.Errorf("reverse merge overwrote existing values: %+v", back)
	}
	if !back.Equal(rich) {
		t.Error("reverse merge changed an already-complete target")
	}
}

func TestTargetEqualAndSameEndpoint(t *testing.T) {
	a := sampleTarget()
	b := sampleTarget()

	if !a.Equal(b) {
		t.Error("identical targets reported as different")
	}

	b.Location.City = "Springfield"
	if a.Equal(b) {
		t.Error("Equal() = true for targets differing in metadata")
	}
	// 端点相同但元数据不同：SameEndpoint 必须仍然为真。
	if !a.SameEndpoint(b) {
		t.Error("SameEndpoint() = false for the same ip:port")
	}

	c := sampleTarget()
	c.ID = "1.2.3.4:8443"
	c.Port = 8443
	if a.SameEndpoint(c) {
		t.Error("SameEndpoint() = true for different ports")
	}
}

// ---------------------------------------------------------------------------
// 列表辅助
// ---------------------------------------------------------------------------

func TestDedupKeepsFirstOccurrenceAndOrder(t *testing.T) {
	first := sampleTarget()
	first.Location.City = "First"

	duplicate := sampleTarget()
	duplicate.Location.City = "Second"

	second := sampleTarget()
	second.ID = "5.6.7.8:443"
	second.IP = "5.6.7.8"

	d := NewDedup(4)
	if !d.Add(first) {
		t.Fatal("first Add returned false")
	}
	if d.Add(duplicate) {
		t.Fatal("duplicate Add returned true")
	}
	if !d.Add(second) {
		t.Fatal("second Add returned false")
	}

	if d.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", d.Len())
	}
	got := d.Targets()
	if got[0].Location.City != "First" {
		t.Errorf("kept %q, want the first occurrence", got[0].Location.City)
	}
	if got[1].ID != "5.6.7.8:443" {
		t.Errorf("order not preserved: %v", Keys(got))
	}
	if !d.Has("1.2.3.4:443") || d.Has("9.9.9.9:443") {
		t.Error("Has() returned wrong results")
	}
}

func TestDedupHandlesEmptyIDAndNilReceiver(t *testing.T) {
	// ID 为空时退化为按 (IP, Port) 计算，避免手工构造的目标
	// 因为共享空 ID 而被错误地全部去重。
	d := NewDedup(2)
	if !d.Add(Target{IP: "1.2.3.4", Port: 443}) {
		t.Fatal("first Add returned false")
	}
	if d.Add(Target{IP: "1.2.3.4", Port: 443}) {
		t.Fatal("duplicate not detected by ip:port")
	}
	if !d.Add(Target{IP: "1.2.3.4", Port: 8443}) {
		t.Fatal("different port must not be treated as duplicate")
	}

	var nilDedup *Dedup
	if nilDedup.Add(sampleTarget()) {
		t.Error("nil Dedup.Add returned true")
	}
	if nilDedup.Len() != 0 || nilDedup.Targets() != nil || nilDedup.Has("x") {
		t.Error("nil Dedup methods must be safe")
	}
}

func TestDedupTargetsHelper(t *testing.T) {
	a := sampleTarget()
	b := sampleTarget()
	c := sampleTarget()
	c.ID, c.IP = "5.6.7.8:443", "5.6.7.8"

	got := DedupTargets([]Target{a, b, c})
	if len(got) != 2 {
		t.Fatalf("DedupTargets len = %d, want 2", len(got))
	}
	if Keys(got)[0] != "1.2.3.4:443" || Keys(got)[1] != "5.6.7.8:443" {
		t.Errorf("keys = %v", Keys(got))
	}
}

func TestSortTargets(t *testing.T) {
	targets := []Target{
		{ID: "9.9.9.9:443"},
		{ID: "1.2.3.4:8443"},
		{ID: "1.2.3.4:443"},
	}
	SortTargets(targets)

	want := []string{"1.2.3.4:443", "1.2.3.4:8443", "9.9.9.9:443"}
	if strings.Join(Keys(targets), ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", Keys(targets), want)
	}
}

func TestValidateAllReportsPosition(t *testing.T) {
	good := sampleTarget()
	bad := sampleTarget()
	bad.ID = "1.2.3.4:99999"
	bad.Port = 99999

	err := ValidateAll([]Target{good, good, bad})
	if err == nil {
		t.Fatal("ValidateAll = nil, want error")
	}
	if !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("error = %v, want ErrInvalidTarget", err)
	}

	var lve *ListValidationError
	if !errors.As(err, &lve) {
		t.Fatalf("error = %v, want *ListValidationError", err)
	}
	if lve.Index != 2 {
		t.Errorf("Index = %d, want 2", lve.Index)
	}
	if !strings.Contains(err.Error(), "#2") {
		t.Errorf("error message = %q, want index included", err.Error())
	}

	if err := ValidateAll(nil); err != nil {
		t.Errorf("ValidateAll(nil) = %v, want nil", err)
	}
}

func TestKeys(t *testing.T) {
	targets := []Target{{ID: "a:1"}, {ID: "b:2"}}
	if got := fmt.Sprint(Keys(targets)); got != "[a:1 b:2]" {
		t.Errorf("Keys = %v", got)
	}
	if got := Keys(nil); len(got) != 0 {
		t.Errorf("Keys(nil) = %v, want empty", got)
	}
}

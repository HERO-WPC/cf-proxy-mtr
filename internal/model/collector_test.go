package model

import (
	"errors"
	"strings"
	"testing"
)

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

	// 任一字段不同都必须产生不同的键，否则聚合会错误合并分组。
	variants := map[string]CollectorProfile{}
	v := profile
	v.ISP = "China Telecom"
	variants["isp"] = v
	v = profile
	v.Province = "Guangdong"
	variants["province"] = v
	v = profile
	v.IPVersion = IPVersionIPv6
	variants["ip_version"] = v
	v = profile
	v.ASN = "AS9808 "
	variants["asn trailing space (before normalize)"] = v

	for name, other := range variants {
		if other.GroupKey() == first && name != "asn trailing space (before normalize)" {
			t.Errorf("GroupKey collision for variant %q", name)
		}
	}

	// 归一化之后，未归一化的变体应该得到相同的键。
	unnormalized := profile
	unnormalized.Country = " cn "
	unnormalized.ASN = "as9808"
	if !unnormalized.Normalize() {
		t.Error("Normalize() = false, want true")
	}
	if unnormalized.GroupKey() != first {
		t.Errorf("normalized GroupKey = %q, want %q", unnormalized.GroupKey(), first)
	}
}

func TestCollectorProfileNormalize(t *testing.T) {
	profile := CollectorProfile{
		Country:   " cn ",
		Province:  " Zhejiang ",
		City:      "Hangzhou  ",
		ISP:       "  China Mobile",
		ASN:       "as9808",
		IPVersion: "IPv4",
	}

	if !profile.Normalize() {
		t.Fatal("Normalize() = false, want true")
	}

	if profile.Country != "CN" {
		t.Errorf("Country = %q, want CN", profile.Country)
	}
	if profile.Province != "Zhejiang" || profile.City != "Hangzhou" {
		t.Errorf("Province/City = %q/%q, want trimmed", profile.Province, profile.City)
	}
	// ISP 保留原始大小写：上游与用户写法不统一，强行改会破坏可读性。
	if profile.ISP != "China Mobile" {
		t.Errorf("ISP = %q, want China Mobile", profile.ISP)
	}
	if profile.ASN != "AS9808" {
		t.Errorf("ASN = %q, want AS9808", profile.ASN)
	}
	if profile.IPVersion != IPVersionIPv4 {
		t.Errorf("IPVersion = %q, want ipv4", profile.IPVersion)
	}

	// 幂等。
	if profile.Normalize() {
		t.Error("Normalize() = true on already-normalized profile, want false")
	}
}

func TestNormalizeASN(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"   ":        "",
		"9808":       "AS9808",
		"AS9808":     "AS9808",
		"as9808":     "AS9808",
		" As9808 ":   "AS9808",
		"AS09808":    "AS9808",
		"0":          "AS0",
		"AS0":        "AS0",
		"AS00131335": "AS131335",
		"Chinanet":   "Chinanet", // 看不懂就原样保留，不丢用户填的信息
		"AS":         "AS",
		"ASabc":      "ASabc",
		"9808 extra": "9808 extra",
	}
	for in, want := range cases {
		if got := NormalizeASN(in); got != want {
			t.Errorf("NormalizeASN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectorProfileValidate(t *testing.T) {
	valid := CollectorProfile{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: IPVersionIPv4,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}

	// 空 Profile 也合法：Phase 6 允许只填部分字段。
	if err := (CollectorProfile{}).Validate(); err != nil {
		t.Errorf("empty profile rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(*CollectorProfile)
		want   string
	}{
		"bad country length": {func(c *CollectorProfile) { c.Country = "CHN" }, "country"},
		"lowercase country":  {func(c *CollectorProfile) { c.Country = "cn" }, "country"},
		"non-alpha country":  {func(c *CollectorProfile) { c.Country = "C1" }, "country"},
		"asn without prefix": {func(c *CollectorProfile) { c.ASN = "9808" }, "asn"},
		"asn non numeric":    {func(c *CollectorProfile) { c.ASN = "ASCMCC" }, "asn"},
		"bad ip version":     {func(c *CollectorProfile) { c.IPVersion = "ipv5" }, "ip_version"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			profile := valid
			tc.mutate(&profile)

			err := profile.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.want)
			}
			if !errors.Is(err, ErrInvalidProfile) {
				t.Errorf("error = %v, want ErrInvalidProfile", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want message containing %q", err, tc.want)
			}
		})
	}
}

func TestCollectorProfileIsAnonymous(t *testing.T) {
	// CollectorProfile 的字段类型决定了它无法承载公网 IP / MAC / 主机名。
	// 这个断言是"隐私约束可测试"的落点：如果以后有人往结构里加了
	// 可识别字段，review 时应当在这里补充反驳理由，而不是删掉测试。
	profile := CollectorProfile{
		Country: "CN", Province: "Zhejiang", City: "Hangzhou",
		ISP: "China Mobile", ASN: "AS9808", IPVersion: IPVersionIPv4,
	}
	if !profile.IsAnonymous() {
		t.Error("IsAnonymous() = false, want true")
	}
	if !(CollectorProfile{}).IsZero() {
		t.Error("zero CollectorProfile IsZero = false, want true")
	}
	if profile.IsZero() {
		t.Error("filled CollectorProfile IsZero = true, want false")
	}
}

// ---------------------------------------------------------------------------
// collector_id
// ---------------------------------------------------------------------------

func TestNewCollectorID(t *testing.T) {
	const iterations = 64

	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		id, err := NewCollectorID()
		if err != nil {
			t.Fatalf("NewCollectorID: %v", err)
		}
		if !ValidCollectorID(id) {
			t.Fatalf("generated id %q is not valid", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}

	// 必须足够长，避免碰撞；同时不能包含任何可识别信息。
	id, err := NewCollectorID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != len("c-")+32 {
		t.Errorf("len(%q) = %d, want %d", id, len(id), len("c-")+32)
	}
	if !strings.HasPrefix(id, "c-") {
		t.Errorf("id %q missing prefix", id)
	}
}

func TestValidCollectorID(t *testing.T) {
	valid := "c-0123456789abcdef0123456789abcdef"
	if !ValidCollectorID(valid) {
		t.Errorf("ValidCollectorID(%q) = false, want true", valid)
	}

	cases := map[string]string{
		"empty":         "",
		"no prefix":     "0123456789abcdef0123456789abcdef",
		"wrong prefix":  "x-0123456789abcdef0123456789abcdef",
		"too short":     "c-0123",
		"too long":      "c-0123456789abcdef0123456789abcdef00",
		"uppercase hex": "c-0123456789ABCDEF0123456789abcdef",
		"non hex":       "c-0123456789abcdef0123456789abcdeg",
		"prefix only":   "c-",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if ValidCollectorID(in) {
				t.Errorf("ValidCollectorID(%q) = true, want false", in)
			}
		})
	}
}

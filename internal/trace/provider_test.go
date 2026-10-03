package trace

import (
	"context"
	"strings"
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// 本文件锁定"数据源"与"PoW 源"这两个选项。
//
// 为什么要专门测：nexttrace 拿到不认识的数据源名时**不报错**，
// 而是换一个源继续跑。那意味着"以为在用 IPInfo，实际在用别的"
// ——从结果上完全看不出来。因此必须把校验放在构造期，
// 并且必须有测试证明它真的挡住了。

// TestDataProviderNormalizeAcceptsCanonical 验证规范值原样通过。
func TestDataProviderNormalizeAcceptsCanonical(t *testing.T) {
	for _, provider := range DataProviders() {
		got, err := provider.Normalize()
		if err != nil {
			t.Errorf("Normalize(%q) returned error: %v", provider, err)
			continue
		}
		if got != provider {
			t.Errorf("Normalize(%q) = %q, want unchanged", provider, got)
		}
		if !provider.Valid() {
			t.Errorf("%q should report Valid()", provider)
		}
	}
}

// TestDataProviderIsCaseInsensitiveAndAcceptsAliases 验证大小写与常见叫法。
//
// 使用者不一定记得帮助里那串带点的写法（"IP-API.com"），
// 直接写 "ipapi" 也该能用。
func TestDataProviderIsCaseInsensitiveAndAcceptsAliases(t *testing.T) {
	cases := map[string]DataProvider{
		"ipinfo":        ProviderIPInfo,
		"IPINFO":        ProviderIPInfo,
		"  IPInfo  ":    ProviderIPInfo,
		"ipinfo.io":     ProviderIPInfo,
		"ipsb":          ProviderIPSB,
		"IP.SB":         ProviderIPSB,
		"ipapi":         ProviderIPAPI,
		"ip-api.com":    ProviderIPAPI,
		"leomoeapi":     ProviderNextTraceAPI,
		"ntapi":         ProviderNextTraceAPI,
		"local":         ProviderIPInfoLocal,
		"ipdb.one":      ProviderIPDBOne,
		"chunzhen":      ProviderChunzhen,
		"none":          ProviderDisabled,
		"off":           ProviderDisabled,
		"disable-geoip": ProviderDisabled,
	}

	for input, want := range cases {
		got, err := DataProvider(input).Normalize()
		if err != nil {
			t.Errorf("Normalize(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestDataProviderEmptyMeansDefault 验证空值走默认源。
func TestDataProviderEmptyMeansDefault(t *testing.T) {
	for _, input := range []string{"", "   "} {
		got, err := DataProvider(input).Normalize()
		if err != nil {
			t.Fatalf("Normalize(%q) returned error: %v", input, err)
		}
		if got != DefaultDataProvider {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, DefaultDataProvider)
		}
	}
}

// TestDataProviderRejectsUnknown 验证拼错的名字被拒绝，且错误里列出可选值。
//
// 这是本文件最重要的一条：如果做成了透传，拼错时 nexttrace 会
// 悄悄换源，使用者永远发现不了。
func TestDataProviderRejectsUnknown(t *testing.T) {
	for _, input := range []string{"nope", "ipinf", "IPInfoX", "google", "1.1.1.1"} {
		got, err := DataProvider(input).Normalize()
		if err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", input, got)
			continue
		}
		// 错误信息必须能指导使用者改对：列出可选值。
		if !strings.Contains(err.Error(), string(ProviderIPInfo)) {
			t.Errorf("error for %q does not list the valid choices: %v", input, err)
		}
	}
}

// TestDataProviderNeedsGeoIP 验证能区分"会不会查外部地区"。
//
// disable-geoip 时 as_path 与落地地区必然为空，调用方据此解释，
// 而不是让人以为跟踪坏了。
func TestDataProviderNeedsGeoIP(t *testing.T) {
	if ProviderDisabled.NeedsGeoIP() {
		t.Error("disable-geoip should report that it needs no GeoIP lookup")
	}
	for _, provider := range DataProviders() {
		if provider == ProviderDisabled {
			continue
		}
		if !provider.NeedsGeoIP() {
			t.Errorf("%q should report that it needs GeoIP", provider)
		}
	}
}

// TestPowProviderNormalize 验证 PoW 源。
func TestPowProviderNormalize(t *testing.T) {
	cases := map[string]PowProvider{
		"":                DefaultPowProvider,
		"   ":             DefaultPowProvider,
		"sakura":          PowSakura,
		"SAKURA":          PowSakura,
		"api.nxtrace.org": PowDefault,
		"default":         PowDefault,
	}
	for input, want := range cases {
		got, err := PowProvider(input).Normalize()
		if err != nil {
			t.Errorf("PowProvider(%q).Normalize() error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("PowProvider(%q) = %q, want %q", input, got, want)
		}
	}

	if _, err := PowProvider("cloudflare").Normalize(); err == nil {
		t.Error("PowProvider accepted an unknown provider")
	}
}

// TestBuildArgsIncludesDataProvider 验证数据源被传给 nexttrace。
//
// 显式传即使等于默认值：这样命令行本身就能证明用的是哪个源。
func TestBuildArgsIncludesDataProvider(t *testing.T) {
	engine := newTestEngine(t, EngineOptions{
		DataProvider: ProviderIPInfo,
	})

	target := model.Target{ID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443}
	args, err := engine.BuildArgs(target)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	if !hasFlagValue(args, "--data-provider", string(ProviderIPInfo)) {
		t.Errorf("args do not carry the data provider: %v", args)
	}
}

// TestBuildArgsIncludesPowProviderOnlyForNextTraceAPI 验证 PoW 源只在
// 数据源是 NextTrace-API 时才传。
//
// 用第三方源时这个参数没有意义，传了只会让命令行更难读。
func TestBuildArgsIncludesPowProviderOnlyForNextTraceAPI(t *testing.T) {
	target := model.Target{ID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443}

	withAPI := newTestEngine(t, EngineOptions{
		DataProvider: ProviderNextTraceAPI,
		PowProvider:  PowSakura,
	})
	args, err := withAPI.BuildArgs(target)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlagValue(args, "--pow-provider", string(PowSakura)) {
		t.Errorf("args do not carry the pow provider: %v", args)
	}

	// 换第三方数据源：PoW 源不该出现。
	withIPInfo := newTestEngine(t, EngineOptions{
		DataProvider: ProviderIPInfo,
		PowProvider:  PowSakura,
	})
	args, err = withIPInfo.BuildArgs(target)
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(args, "--pow-provider") {
		t.Errorf("--pow-provider was passed with a third-party data provider: %v", args)
	}
}

// TestBuildArgsOmitsPowProviderWhenUnset 验证不指定时不传 PoW 源。
//
// 传空值会让 nexttrace 收到一个空参数，而"不传"才是
// "用上游默认值"的正确表达。
func TestBuildArgsOmitsPowProviderWhenUnset(t *testing.T) {
	engine := newTestEngine(t, EngineOptions{DataProvider: ProviderNextTraceAPI})

	target := model.Target{ID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443}
	args, err := engine.BuildArgs(target)
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(args, "--pow-provider") {
		t.Errorf("--pow-provider was passed even though it was not configured: %v", args)
	}
}

// TestNewEngineRejectsUnknownProvider 验证构造期就挡住非法数据源。
func TestNewEngineRejectsUnknownProvider(t *testing.T) {
	_, err := NewNextTraceEngine(context.Background(), EngineOptions{
		BinaryPath:       "fake-nexttrace",
		SkipVersionCheck: true,
		RunCommand: func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return []byte("{}"), nil, nil
		},
		DataProvider: DataProvider("nope"),
	})
	if err == nil {
		t.Fatal("NewNextTraceEngine accepted an unknown data provider")
	}
	if !strings.Contains(err.Error(), "unknown data provider") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

// TestDataProvidersMatchesNextTraceHelp 钉住可选值与 nexttrace 帮助一致。
//
// 这些名字是**外部契约**：写错了 nexttrace 会静默换源。
// 因此它们不能随手改——要改必须同时确认本地 nexttrace 的 --help。
func TestDataProvidersMatchesNextTraceHelp(t *testing.T) {
	want := map[string]bool{
		"NextTrace-API": true,
		"IP.SB":         true,
		"IPInfo":        true,
		"IPInsight":     true,
		"IP-API.com":    true,
		"IPInfoLocal":   true,
		"ipdb.one":      true,
		"chunzhen":      true,
		"disable-geoip": true,
	}

	got := make(map[string]bool)
	for _, provider := range DataProviders() {
		got[string(provider)] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("DataProviders() is missing %q, which nexttrace --help lists", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("DataProviders() has %q, which nexttrace --help does not list", name)
		}
	}
}

// newTestEngine 构造一个不启动真实进程的引擎。
func newTestEngine(t *testing.T, opts EngineOptions) *NextTraceEngine {
	t.Helper()

	opts.BinaryPath = "fake-nexttrace"
	opts.SkipVersionCheck = true
	if opts.RunCommand == nil {
		opts.RunCommand = func(ctx context.Context, path string, args []string) ([]byte, []byte, error) {
			return []byte("{}"), nil, nil
		}
	}

	engine, err := NewNextTraceEngine(context.Background(), opts)
	if err != nil {
		t.Fatalf("NewNextTraceEngine: %v", err)
	}
	return engine
}

// hasFlag 报告参数里是否出现某个 flag。
func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// hasFlagValue 报告参数里是否有 "flag value" 这一对。
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

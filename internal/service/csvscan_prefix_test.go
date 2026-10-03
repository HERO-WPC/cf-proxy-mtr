package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/asnprefix"
	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/trace"
)

// 本文件覆盖"用线路的 IP 段识别线路"这一层。
//
// 为什么要单独测：前缀匹配的用途是**替代**限流的 GeoIP 服务，
// 因此它必须在数据源是 disable-geoip（引擎不给任何 ASN）时
// 仍然产出线路名。否则使用者按文档配好之后会发现线路名全空，
// 而日志上看不出原因。

// fakeResolver 用手工前缀构造解析器。
func fakeResolver(t *testing.T, data map[string][]string, order ...string) *asnprefix.Resolver {
	t.Helper()

	// asnprefix 没有导出"手工构造"的入口（生产代码只能 Load），
	// 因此这里走它的缓存路径：把手工数据写成缓存文件再 Load。
	dir := t.TempDir()
	for asn, prefixes := range data {
		blob := "["
		for i, prefix := range prefixes {
			if i > 0 {
				blob += ","
			}
			blob += `"` + prefix + `"`
		}
		blob += "]"
		if err := writeCacheFile(dir, asn, blob); err != nil {
			t.Fatalf("write cache for %s: %v", asn, err)
		}
	}

	return asnprefix.Load(context.Background(), asnprefix.Options{
		Dir:  dir,
		ASNs: order,
		// 不提供 BaseURL / HTTPClient：缓存已就绪，不该联网。
		// 万一缓存没命中就会失败，从而暴露测试假设被破坏。
		Logf: func(format string, args ...any) {
			t.Logf("asnprefix: "+format, args...)
		},
	})
}

// writeCacheFile 写一个 ASN 的缓存文件。
func writeCacheFile(dir, asn, content string) error {
	return os.WriteFile(filepath.Join(dir, strings.ToUpper(strings.TrimSpace(asn))+".json"), []byte(content), 0o644)
}

// TestTraceRowUsesPrefixMatchWhenEngineGivesNoASN 验证引擎不给 ASN 时
// 仍然能由前缀匹配产出线路。
//
// 这是核心场景：`--data-provider disable-geoip` 下引擎的每一跳
// 都没有 ASN，线路名完全依赖前缀匹配。
func TestTraceRowUsesPrefixMatchWhenEngineGivesNoASN(t *testing.T) {
	resolver := fakeResolver(t, map[string][]string{
		"AS4809":  {"1.1.1.0/24"},
		"AS58453": {"2.2.2.0/24"},
	}, "AS4809", "AS58453")

	// 引擎的输出：有 IP，但 **ASN 全空**（disable-geoip 的效果）。
	result := &trace.TraceResult{
		Success: true,
		Hops: []trace.Hop{
			{TTL: 1, IP: "192.168.1.1"},
			{TTL: 2, IP: "1.1.1.1"}, // CN2
			{TTL: 3, IP: "1.1.1.2"}, // 仍是 CN2
			{TTL: 4, IP: "2.2.2.1"}, // CMI
		},
	}

	route := resolveRoute(result, resolver)
	if strings.Join(route, ",") != "AS4809,AS58453" {
		t.Fatalf("resolveRoute = %v, want [AS4809 AS58453] from prefix matching", route)
	}

	// 日志形式必须带 ASN 编号与线路名。
	summary := routeSummary(result, resolver)
	for _, want := range []string{"CN2(AS4809)", "CMI(AS58453)"} {
		if !strings.Contains(summary, want) {
			t.Errorf("route summary = %q, missing %q", summary, want)
		}
	}
}

// TestResolveRoutePrefersPrefixMatchOverEngineASN 验证前缀匹配优先。
//
// 前缀数据来自 BGP 表，而引擎的 ASN 来自 GeoIP——前者对
// "是否经过某条骨干"更可靠。至少要保证两者冲突时行为是**确定**的。
func TestResolveRoutePrefersPrefixMatchOverEngineASN(t *testing.T) {
	resolver := fakeResolver(t, map[string][]string{
		"AS4809": {"1.1.1.0/24"},
	}, "AS4809")

	result := &trace.TraceResult{
		Success: true,
		Hops: []trace.Hop{
			// 引擎说这是 AS9999，但 IP 落在 AS4809 的段里。
			{TTL: 1, IP: "1.1.1.1", ASN: "AS9999"},
		},
	}

	route := resolveRoute(result, resolver)
	if len(route) != 1 || route[0] != "AS4809" {
		t.Errorf("resolveRoute = %v, want [AS4809] (prefix data wins)", route)
	}
}

// TestResolveRouteFallsBackToEngineASN 验证前缀没认出时退回引擎的 ASN。
func TestResolveRouteFallsBackToEngineASN(t *testing.T) {
	resolver := fakeResolver(t, map[string][]string{
		"AS4809": {"1.1.1.0/24"},
	}, "AS4809")

	result := &trace.TraceResult{
		Success: true,
		Hops: []trace.Hop{
			// 这些 IP 都不在 AS4809 的段里，前缀匹配认不出来。
			{TTL: 1, IP: "9.9.9.1", ASN: "AS3356"},
			{TTL: 2, IP: "9.9.9.2", ASN: "AS1299"},
		},
	}

	route := resolveRoute(result, resolver)
	if strings.Join(route, ",") != "AS3356,AS1299" {
		t.Errorf("resolveRoute = %v, want the engine's ASNs as fallback", route)
	}
}

// TestResolveRouteWithoutResolverUsesEngineASN 验证关掉前缀匹配时
// 行为退回原样（--trace-no-asn-prefix）。
func TestResolveRouteWithoutResolverUsesEngineASN(t *testing.T) {
	result := &trace.TraceResult{
		Success: true,
		Hops: []trace.Hop{
			{TTL: 1, IP: "1.1.1.1", ASN: "AS4809"},
			{TTL: 2, IP: "2.2.2.1", ASN: "AS58453"},
		},
	}

	route := resolveRoute(result, nil)
	if strings.Join(route, ",") != "AS4809,AS58453" {
		t.Errorf("resolveRoute(result, nil) = %v, want the engine's ASNs", route)
	}
}

// TestResolveRouteHandlesNilResult 验证空结果不 panic。
func TestResolveRouteHandlesNilResult(t *testing.T) {
	if got := resolveRoute(nil, nil); got != nil {
		t.Errorf("resolveRoute(nil, nil) = %v, want nil", got)
	}
	if got := routeSummary(nil, nil); got != "" {
		t.Errorf("routeSummary(nil, nil) = %q, want empty", got)
	}
}

// TestTraceRowCarriesPrefixBasedRoute 验证 CSV 行的 as_path 也来自前缀匹配。
//
// CSV 与日志必须一致：两份说法不同（日志说走 CN2、CSV 说走 163）
// 会让人无法判断哪个是真的。
func TestTraceRowCarriesPrefixBasedRoute(t *testing.T) {
	resolver := fakeResolver(t, map[string][]string{
		"AS4809": {"1.1.1.0/24"},
	}, "AS4809")

	target := model.Target{ID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443}
	result := &trace.TraceResult{
		Success: true,
		Hops: []trace.Hop{
			{TTL: 1, IP: "1.1.1.1"}, // ASN 为空，靠前缀匹配
		},
	}

	row := traceRow(target, result, resolver)
	if row.ASPath != "CN2" {
		t.Errorf("as_path = %q, want %q (from prefix matching)", row.ASPath, "CN2")
	}
}

// TestRunCSVScanSkipsPrefixFetchWhenDisabled 验证关掉开关时不建缓存。
func TestRunCSVScanSkipsPrefixFetchWhenDisabled(t *testing.T) {
	port := listenLocal(t)
	svc := testService(t, writeCache(t, port))

	dir := filepath.Join(t.TempDir(), "prefix")
	out := filepath.Join(t.TempDir(), "results.csv")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	engine, _ := fakeTraceEngine(t, 0)

	if _, err := svc.RunCSVScan(ctx, CSVScanOptions{
		OutputPath:          out,
		Timeout:             2 * time.Second,
		Trace:               true,
		traceEngineOverride: engine,
		NoASNPrefix:         true,
		ASNPrefixOptions:    asnprefix.Options{Dir: dir},
	}); err != nil {
		t.Fatalf("RunCSVScan: %v", err)
	}

	// 关掉之后不该有任何前缀缓存产生。
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("prefix cache was created despite NoASNPrefix: %v", entries)
	}
}

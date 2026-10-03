package asnprefix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖**内置快照兜底**。
//
// 为什么值得单独测：这条路径只在"下载失败且没有缓存"时走到，
// 也就是最不容易被手工验证到的场景（离线、站点挂掉）。
// 如果它坏了，表现是"离线时线路名全空"——而使用者会以为
// 是线路识别功能本身不工作。

// TestSnapshotIsEmbeddedAndParsable 验证内置快照存在且能解析。
//
// 这条测试也是**构建护栏**：如果有人改了 Package 结构却没重新
// 生成快照（go:generate），这里会直接失败，而不是等到离线时
// 才发现兜底是空的。
func TestSnapshotIsEmbeddedAndParsable(t *testing.T) {
	generatedAt, err := SnapshotGeneratedAt()
	if err != nil {
		t.Fatalf("内置快照不可用：%v", err)
	}
	if strings.TrimSpace(generatedAt) == "" {
		t.Error("内置快照没有生成时间；使用者无法判断数据有多旧")
	}

	// 至少要能覆盖 asnmap 里那几条国内骨干线——快照的意义就在于此。
	wanted := map[string]bool{
		"AS4134":  true, // 163
		"AS4809":  true, // CN2
		"AS4837":  true, // 169
		"AS9929":  true, // CUII
		"AS9808":  true, // CMNET
		"AS58453": true, // CMI
	}

	prefixes, _, err := loadSnapshot(wanted)
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}

	for asn := range wanted {
		if len(prefixes[asn]) == 0 {
			t.Errorf("内置快照缺少 %s 的前缀", asn)
		}
	}
}

// TestSnapshotNormalizesASNKeys 验证 ASN 写法差异不会导致取不到数据。
//
// 快照里存的是 "AS4809"，而调用方可能传 "4809"。
// 不做归一化会**一个都取不到**，而且没有任何报错。
func TestSnapshotNormalizesASNKeys(t *testing.T) {
	withPrefix, _, err := loadSnapshot(map[string]bool{"AS4809": true})
	if err != nil {
		t.Fatal(err)
	}
	withoutPrefix, _, err := loadSnapshot(map[string]bool{"4809": true})
	if err != nil {
		t.Fatal(err)
	}

	if len(withPrefix["AS4809"]) == 0 {
		t.Fatal("AS4809 lookup returned nothing")
	}
	if len(withoutPrefix["4809"]) == 0 {
		t.Error("bare ASN number 4809 returned nothing; key normalisation is missing")
	}
}

// TestLoadFallsBackToSnapshotWhenDownloadFailsAndNoCache 验证核心场景。
//
// 下载失败 + 完全没有缓存 -> 用内置快照，线路仍然认得出。
func TestLoadFallsBackToSnapshotWhenDownloadFailsAndNoCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var logs []string
	resolver := Load(context.Background(), Options{
		// 全新的空目录：没有任何缓存。
		Dir:        t.TempDir(),
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809", "AS9808"},
		HTTPClient: server.Client(),
		Logf: func(format string, args ...any) {
			logs = append(logs, strings.TrimSpace(format))
		},
	})

	if !resolver.Ready() {
		t.Fatalf("下载失败且无缓存时没有退回内置快照；日志：%v", logs)
	}
	if !containsAny(logs, "内置快照") {
		t.Errorf("日志没有说明用了内置快照：%v", logs)
	}

	// 真的能用：CN2 的某个段必须命中。
	// 用快照里第一条前缀来验证，避免把测试绑死在具体数据上。
	prefixes, _, err := loadSnapshot(map[string]bool{"AS4809": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes["AS4809"]) == 0 {
		t.Fatal("内置快照里没有 CN2 的段")
	}
	probe := prefixProbeAddress(t, prefixes["AS4809"][0])

	if got := resolver.Match(probe); len(got) == 0 {
		t.Errorf("Match(%s) 没有命中任何线路；内置快照没有真正生效", probe)
	}
}

// TestLoadPrefersCacheOverSnapshot 验证快照不会盖过可用缓存。
//
// 快照是兜底，不是数据源：有更新的缓存时必须用缓存。
func TestLoadPrefersCacheOverSnapshot(t *testing.T) {
	// 缓存里放一个**编造**的段，用来区分"用了缓存"与"用了快照"。
	const marker = "203.0.113.0/24"

	dir := t.TempDir()
	if err := writeCacheFileForTest(dir, "AS4809", []string{marker}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	resolver := Load(context.Background(), Options{
		Dir:        dir,
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
		Logf:       func(string, ...any) {},
	})

	if got := resolver.Match("203.0.113.5"); len(got) != 1 || got[0] != "AS4809" {
		t.Errorf("Match(203.0.113.5) = %v, want the cached marker to win over the snapshot", got)
	}
}

// TestLoadPrefersFreshDownloadOverSnapshot 验证刚下载的数据优先。
func TestLoadPrefersFreshDownloadOverSnapshot(t *testing.T) {
	// 让服务端返回一条只属于 AS4809 的编造段。
	rows := []string{"203.0.113.0\t203.0.113.255\t4809\tCN\tmarker"}
	server, _ := fakeBulkServer(t, rows)

	resolver := Load(context.Background(), Options{
		Dir:        t.TempDir(),
		BulkURL:    server.URL,
		ASNs:       []string{"AS4809"},
		HTTPClient: server.Client(),
		Logf:       func(string, ...any) {},
	})

	if got := resolver.Match("203.0.113.5"); len(got) != 1 {
		t.Errorf("Match(203.0.113.5) = %v, want the freshly downloaded marker", got)
	}
}

// TestSnapshotDoesNotPretendToHaveUnknownASNs 验证快照里没有的 ASN
// 不会被假装加载成功。
//
// 否则"就绪 N/N"会虚高，而实际认不出任何东西。
func TestSnapshotDoesNotPretendToHaveUnknownASNs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var logs []string
	resolver := Load(context.Background(), Options{
		Dir:     t.TempDir(),
		BulkURL: server.URL,
		// 一个几乎不可能出现在快照里的 ASN。
		ASNs:       []string{"AS4294967294"},
		HTTPClient: server.Client(),
		Logf: func(format string, args ...any) {
			logs = append(logs, format)
		},
	})

	if resolver.Ready() {
		t.Error("resolver claims to be ready for an ASN that is not in the snapshot")
	}
	if !containsAny(logs, "没有可用的线路段") {
		t.Errorf("日志没有说明快照里没有对应线路：%v", logs)
	}
}

// prefixProbeAddress 返回某条前缀里的一个可用地址（用来验证命中）。
//
// 用前缀自身而不是硬编码 IP：测试不该绑死在快照的具体数据上，
// 否则每次重新生成快照都要改测试。
func prefixProbeAddress(t *testing.T, prefix string) string {
	t.Helper()

	parsed, err := parsePrefix(prefix)
	if err != nil {
		t.Fatalf("parsePrefix(%q): %v", prefix, err)
	}
	// 前缀的网络地址本身就在集合里。
	return parsed.Addr().String()
}

// writeCacheFileForTest 往缓存目录写一个 ASN 的前缀列表。
func writeCacheFileForTest(dir, asn string, prefixes []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	blob, err := json.Marshal(prefixes)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, asn+".json"), blob, 0o644)
}

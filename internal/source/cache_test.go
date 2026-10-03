package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// sampleTargets 返回一组用于缓存往返测试的目标。
func sampleTargets(t *testing.T) []model.Target {
	t.Helper()

	v4 := model.Target{
		ID:        "1.2.3.4:443",
		IP:        "1.2.3.4",
		Port:      443,
		IPVersion: model.IPVersionIPv4,
		Location: model.Location{
			CCA2:           "US",
			IATA:           "ORD",
			Region:         "Illinois",
			City:           "Chicago",
			Latitude:       41.85003,
			Longitude:      -87.65005,
			Country:        "US",
			CountryEN:      "United States",
			HasCoordinates: true,
		},
	}
	v6 := model.Target{
		ID:        "[2001:db8::1]:8443",
		IP:        "2001:db8::1",
		Port:      8443,
		IPVersion: model.IPVersionIPv6,
		Location: model.Location{
			CCA2:             "DE",
			IATA:             "FRA",
			City:             "Frankfurt",
			Latitude:         50.026402,
			Longitude:        8.54313,
			HasCoordinates:   true,
			FromColoFallback: true,
		},
	}
	// 0,0 是合法坐标，必须能往返而不被当成"无坐标"。
	zero := model.Target{
		ID:        "9.9.9.9:443",
		IP:        "9.9.9.9",
		Port:      443,
		IPVersion: model.IPVersionIPv4,
		Location: model.Location{
			CCA2:           "GH",
			Latitude:       0,
			Longitude:      0,
			HasCoordinates: true,
		},
	}

	return []model.Target{v4, v6, zero}
}

func sampleMeta() SourceMeta {
	return SourceMeta{
		URL:                  "https://example.test/all.json",
		Format:               "json",
		GeneratorGeneratedAt: time.Date(2026, 10, 3, 3, 52, 35, 771190000, time.UTC),
		HasGeneratorTime:     true,
		ReportedCount:        11610,
		CountryCounts:        map[string]int{"US": 1388, "DE": 2495},
	}
}

func sampleStats() ParseStats {
	return ParseStats{
		RawItems:         11610,
		TargetCandidates: 11610,
		RawCombos:        14635,
		Duplicates:       0,
		InvalidPorts:     0,
		Reasons:          map[string]int{"duplicate ip:port": 0},
	}
}

// ---------------------------------------------------------------------------
// 缓存往返
// ---------------------------------------------------------------------------

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.json")
	targets := sampleTargets(t)

	if err := WriteCache(path, sampleMeta(), sampleStats(), targets); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	meta, stats, got, writtenAt, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}

	if len(got) != len(targets) {
		t.Fatalf("targets = %d, want %d", len(got), len(targets))
	}
	if got[0] != targets[0] {
		t.Errorf("target[0] = %+v, want %+v", got[0], targets[0])
	}
	if got[1] != targets[1] {
		t.Errorf("target[1] = %+v, want %+v", got[1], targets[1])
	}
	// 0,0 坐标必须原样往返，且 HasCoordinates 保持 true。
	if !got[2].Location.HasCoordinates || got[2].Location.Latitude != 0 || got[2].Location.Longitude != 0 {
		t.Errorf("target[2] location = %+v, want 0,0 with HasCoordinates", got[2].Location)
	}

	if meta.URL != sampleMeta().URL || meta.Format != "json" {
		t.Errorf("meta = %+v, want url/format preserved", meta)
	}
	if !meta.HasGeneratorTime || !meta.GeneratorGeneratedAt.Equal(sampleMeta().GeneratorGeneratedAt) {
		t.Errorf("generated time = %v (valid=%v), want %v", meta.GeneratorGeneratedAt, meta.HasGeneratorTime, sampleMeta().GeneratorGeneratedAt)
	}
	if meta.ReportedCount != 11610 || meta.CountryCounts["DE"] != 2495 {
		t.Errorf("meta summary = %+v, want reported count and country counts", meta)
	}

	if stats.RawCombos != 14635 || stats.RawItems != 11610 {
		t.Errorf("stats = %+v, want preserved", stats)
	}

	if writtenAt.IsZero() || time.Since(writtenAt) > time.Minute {
		t.Errorf("writtenAt = %v, want recent timestamp", writtenAt)
	}
}

func TestCacheIsValidJSONWithSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}

	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		WrittenAt     string `json:"written_at"`
		Source        struct {
			URL string `json:"url"`
		} `json:"source"`
		Targets []struct {
			ID       string `json:"id"`
			IP       string `json:"ip"`
			Port     int    `json:"port"`
			Location struct {
				Lat *float64 `json:"lat"`
			} `json:"location"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("cache is not valid json: %v", err)
	}

	// 公共数据必须带 schema_version；缓存文件同样带上，便于失效判断。
	if doc.SchemaVersion != version.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, version.SchemaVersion)
	}
	if doc.WrittenAt == "" {
		t.Error("written_at is empty")
	}
	if len(doc.Targets) != 3 {
		t.Fatalf("targets in file = %d, want 3", len(doc.Targets))
	}
	if doc.Targets[0].ID != "1.2.3.4:443" || doc.Targets[0].Port != 443 {
		t.Errorf("target[0] = %+v, want 1.2.3.4:443", doc.Targets[0])
	}
	// omitempty 会让 0 值坐标消失：这正是需要 HasCoordinates 字段的原因。
	if doc.Targets[2].Location.Lat != nil {
		t.Errorf("zero latitude should be omitted by omitempty, got %v", *doc.Targets[2].Location.Lat)
	}
}

func TestCacheCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "all.json")

	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not created: %v", err)
	}
}

func TestCacheWriteIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "all.json")

	// 写两次，模拟覆盖刷新。
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)[:1]); err != nil {
		t.Fatalf("second write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "all.json" {
			t.Errorf("unexpected leftover file %q (temp file not cleaned up)", e.Name())
		}
	}

	// 覆盖后的内容必须是第二次写入的（1 个目标），而不是两个文件叠加。
	_, _, targets, _, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if len(targets) != 1 {
		t.Errorf("targets = %d, want 1 after overwrite", len(targets))
	}
}

func TestCacheWriteRejectsEmptyPath(t *testing.T) {
	if err := WriteCache("", sampleMeta(), sampleStats(), nil); err == nil {
		t.Fatal("WriteCache(\"\") succeeded, want error")
	}
}

func TestReadCacheErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, _, _, _, err := ReadCache(filepath.Join(dir, "nope.json")); err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("truncated json", func(t *testing.T) {
		path := filepath.Join(dir, "truncated.json")
		if err := os.WriteFile(path, []byte(`{"schema_version":1,"targets":[{"id":"1.2.3`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := ReadCache(path); err == nil {
			t.Fatal("expected error for truncated json")
		}
	})

	t.Run("schema mismatch", func(t *testing.T) {
		path := filepath.Join(dir, "old.json")
		blob := `{"schema_version":999,"written_at":"2026-10-03T00:00:00Z","targets":[]}`
		if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, _, _, err := ReadCache(path)
		if err == nil {
			t.Fatal("expected error for schema mismatch")
		}
		if !strings.Contains(err.Error(), "schema_version") {
			t.Errorf("error = %v, want schema_version mention", err)
		}
	})

	t.Run("not an object", func(t *testing.T) {
		path := filepath.Join(dir, "text.json")
		if err := os.WriteFile(path, []byte("1.2.3.4:443#US\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := ReadCache(path); err == nil {
			t.Fatal("expected error for non-object cache")
		}
	})
}

// ---------------------------------------------------------------------------
// InspectCache
// ---------------------------------------------------------------------------

func TestInspectCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	now := time.Now()
	info, err := InspectCache(path, now)
	if err != nil {
		t.Fatalf("InspectCache: %v", err)
	}

	if !info.Exists {
		t.Error("Exists = false")
	}
	if info.TargetCount != 3 {
		t.Errorf("TargetCount = %d, want 3", info.TargetCount)
	}
	if info.SourceURL != sampleMeta().URL {
		t.Errorf("SourceURL = %q, want %q", info.SourceURL, sampleMeta().URL)
	}
	if info.SchemaVersion != version.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", info.SchemaVersion, version.SchemaVersion)
	}
	if info.Age < 0 || info.Age > time.Minute {
		t.Errorf("Age = %s, want small positive duration", info.Age)
	}
	// 用未来时间检查 Age 计算。
	if future, err := InspectCache(path, now.Add(10*time.Hour)); err != nil {
		t.Fatalf("InspectCache(future): %v", err)
	} else if future.Age < 9*time.Hour {
		t.Errorf("Age = %s, want ~10h", future.Age)
	}
}

func TestInspectCacheErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := InspectCache(filepath.Join(dir, "missing.json"), time.Now()); err == nil {
		t.Fatal("expected error for missing file")
	}

	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectCache(path, time.Now()); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

// ---------------------------------------------------------------------------
// 兼容性
// ---------------------------------------------------------------------------

func TestFromCacheTargetInfersMissingFields(t *testing.T) {
	// 模拟"没有 id / 没有 ip_version"的缓存条目（早期格式），
	// 读取后必须能补全，而不是留下空 ID。
	got := fromCacheTarget(cacheTarget{IP: "1.2.3.4", Port: 443})
	if got.ID != "1.2.3.4:443" {
		t.Errorf("ID = %q, want 1.2.3.4:443", got.ID)
	}
	if got.IPVersion != model.IPVersionIPv4 {
		t.Errorf("IPVersion = %q, want ipv4", got.IPVersion)
	}

	v6 := fromCacheTarget(cacheTarget{IP: "2001:db8::1", Port: 443})
	if v6.IPVersion != model.IPVersionIPv6 {
		t.Errorf("IPVersion = %q, want ipv6", v6.IPVersion)
	}

	bad := fromCacheTarget(cacheTarget{IP: "not-an-ip", Port: 443})
	if bad.IPVersion != model.IPVersionUnknown {
		t.Errorf("IPVersion = %q, want unknown", bad.IPVersion)
	}
}

func TestSortTargetsIsStableAndTotal(t *testing.T) {
	targets := []model.Target{
		{ID: "9.9.9.9:443"},
		{ID: "1.2.3.4:8443"},
		{ID: "1.2.3.4:443"},
	}
	SortTargets(targets)

	want := []string{"1.2.3.4:443", "1.2.3.4:8443", "9.9.9.9:443"}
	for i := range want {
		if targets[i].ID != want[i] {
			t.Fatalf("targets = %v, want %v", targets, want)
		}
	}
}

func TestWriteCacheFileModeIsReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 不使用 POSIX 权限位，跳过。
		t.Skip("file mode semantics differ on windows")
	}

	path := filepath.Join(t.TempDir(), "all.json")
	if err := WriteCache(path, sampleMeta(), sampleStats(), sampleTargets(t)); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o400 == 0 {
		t.Errorf("mode = %v, want owner-readable", info.Mode())
	}
}

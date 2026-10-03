package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// 缓存相关常量。
const (
	// DefaultCacheTTL 是缓存的默认有效期。
	//
	// 6 小时是刻意的选择：all.json 每天更新数次，
	// 而一次完整扫描本身就要持续数十分钟到数小时，
	// 因此 6 小时既可以避免重复下载 10 MiB 数据，
	// 又不会让目标列表明显过期。
	DefaultCacheTTL = 6 * time.Hour

	// cacheFileMode 是新缓存文件的权限。
	cacheFileMode = fs.FileMode(0o644)
)

// cacheDoc 是缓存文件的顶层结构。
//
// 为什么缓存的是解析后的目标列表，而不是原始响应字节？
//
//  1. 后续阶段（scan / resume）需要的是目标列表，而不是原始 JSON；
//     缓存解析结果可以避免每次启动都重新解析 10 MiB JSON。
//  2. 原始 JSON 的格式随时可能变化，缓存解析结果相当于把
//     "格式变化"隔离在解析层，重跑一次 fetch 即可恢复。
//  3. 目标列表本身是紧凑、稳定、可读的 JSON，便于排查问题。
//
// 缓存文件属于**本地中间数据**，不是公开数据，因此不承诺向后兼容；
// 通过 SchemaVersion 校验，不匹配时视为缓存失效并重新下载。
type cacheDoc struct {
	// SchemaVersion 是缓存文件格式版本。
	SchemaVersion int `json:"schema_version"`

	// WrittenAt 是缓存写入时间。
	WrittenAt time.Time `json:"written_at"`

	// Source 是数据源元数据。
	Source cacheSource `json:"source"`

	// Stats 是解析统计。
	Stats ParseStats `json:"stats"`

	// Targets 是去重后的目标列表。
	Targets []cacheTarget `json:"targets"`
}

// cacheSource 是缓存中的数据源元数据。
type cacheSource struct {
	URL                  string         `json:"url"`
	Format               string         `json:"format"`
	GeneratorGeneratedAt *time.Time     `json:"generator_generated_at,omitempty"`
	ReportedCount        int            `json:"reported_count,omitempty"`
	CountryCounts        map[string]int `json:"country_counts,omitempty"`
}

// cacheTarget 是缓存中的单个目标。
//
// 使用短字段名是为了控制文件体积（生产数据约 1.5 万个目标），
// 同时保持可读性。
type cacheTarget struct {
	ID string `json:"id"`

	IP   string `json:"ip"`
	Port int    `json:"port"`

	IPVersion string `json:"ip_version,omitempty"`

	// Location 是目标 IP 自身的地理信息。
	Location cacheLocation `json:"location"`
}

// cacheLocation 是缓存中的目标地理位置。
type cacheLocation struct {
	CCA2      string  `json:"cca2,omitempty"`
	IATA      string  `json:"iata,omitempty"`
	Region    string  `json:"region,omitempty"`
	City      string  `json:"city,omitempty"`
	Latitude  float64 `json:"lat,omitempty"`
	Longitude float64 `json:"lon,omitempty"`
	Country   string  `json:"country,omitempty"`
	CountryEN string  `json:"country_en,omitempty"`

	// HasCoordinates 必须显式保存：0,0 是合法坐标，
	// 不能靠零值推断"没有坐标"。
	HasCoordinates bool `json:"has_coordinates,omitempty"`

	// FromColoFallback 标记坐标来自 colo 回退。
	FromColoFallback bool `json:"from_colo_fallback,omitempty"`
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// WriteCache 把解析结果原子地写入缓存文件。
//
// 原子性做法：先写同目录下的临时文件，fsync，再 Rename 覆盖目标。
// 这样即使进程被 Ctrl+C 或断电打断，也不会留下半个 JSON 文件
// 导致下次启动读到损坏缓存。
//
// 目录不存在时自动创建（含父目录）。
func WriteCache(path string, meta SourceMeta, stats ParseStats, targets []model.Target) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("empty cache path")
	}

	doc := cacheDoc{
		SchemaVersion: version.SchemaVersion,
		WrittenAt:     time.Now().UTC(),
		Source: cacheSource{
			URL:           meta.URL,
			Format:        meta.Format,
			ReportedCount: meta.ReportedCount,
			CountryCounts: meta.CountryCounts,
		},
		Stats:   stats,
		Targets: make([]cacheTarget, 0, len(targets)),
	}
	if meta.HasGeneratorTime {
		t := meta.GeneratorGeneratedAt.UTC()
		doc.Source.GeneratorGeneratedAt = &t
	}

	for _, t := range targets {
		doc.Targets = append(doc.Targets, toCacheTarget(t))
	}

	// 不使用缩进：文件更小，读取更快；这是机器中间数据，不需要人工阅读友好到
	// 牺牲体积。需要人工检查时可配合 jq。
	blob, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create cache dir %q: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()

	// 任何失败路径都要清理临时文件，避免留下垃圾。
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(blob); err != nil {
		cleanup()
		return fmt.Errorf("write temp cache: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp cache: %w", err)
	}

	// Windows 上 os.Rename 覆盖已存在文件是允许的（Go 内部使用
	// MoveFileEx + MOVEFILE_REPLACE_EXISTING），因此不需要先删除目标文件。
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace cache file %q: %w", path, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// ReadCache 读取缓存文件。
//
// 返回的 SourceMeta.URL 是缓存中记录的原始数据源地址；
// 调用方需要把当前生效的 URL 填回去（缓存本身不决定"应该用哪个源"）。
//
// 返回 error 的情形：文件不存在、JSON 损坏、schema 版本不匹配。
// 这些情况一律视为"缓存不可用"，而不是致命错误——
// 调用方应当继续走网络下载。
func ReadCache(path string) (*SourceMeta, ParseStats, []model.Target, time.Time, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, ParseStats{}, nil, time.Time{}, err
	}

	var doc cacheDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, ParseStats{}, nil, time.Time{}, fmt.Errorf("decode cache %q: %w", path, err)
	}
	if doc.SchemaVersion != version.SchemaVersion {
		return nil, ParseStats{}, nil, time.Time{}, fmt.Errorf(
			"cache %q has schema_version %d, want %d",
			path, doc.SchemaVersion, version.SchemaVersion)
	}

	meta := SourceMeta{
		URL:           doc.Source.URL,
		Format:        doc.Source.Format,
		ReportedCount: doc.Source.ReportedCount,
		CountryCounts: doc.Source.CountryCounts,
	}
	if doc.Source.GeneratorGeneratedAt != nil {
		meta.GeneratorGeneratedAt = *doc.Source.GeneratorGeneratedAt
		meta.HasGeneratorTime = true
	}

	targets := make([]model.Target, 0, len(doc.Targets))
	for _, ct := range doc.Targets {
		targets = append(targets, fromCacheTarget(ct))
	}

	return &meta, doc.Stats, targets, doc.WrittenAt, nil
}

// CacheInfo 是缓存的轻量状态描述，用于决定是否需要刷新。
type CacheInfo struct {
	// Exists 表示缓存文件存在且可解析。
	Exists bool

	// WrittenAt 是缓存写入时间。
	WrittenAt time.Time

	// Age 是缓存年龄（相对于给定 now）。
	Age time.Duration

	// TargetCount 是缓存中的目标数量。
	TargetCount int

	// SourceURL 是缓存记录的数据源地址。
	SourceURL string

	// SchemaVersion 是缓存文件格式版本。
	SchemaVersion int
}

// InspectCache 只读取缓存的元数据部分（不构建目标列表）。
//
// 目的：在"只需要判断缓存是否新鲜"的场景下避免完整解析大文件。
// 实现上仍然解析 JSON，但只保留头部字段后即丢弃 targets，
// 因此内存占用显著低于 ReadCache。
func InspectCache(path string, now time.Time) (CacheInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return CacheInfo{}, err
	}
	defer func() { _ = f.Close() }()

	var head struct {
		SchemaVersion int       `json:"schema_version"`
		WrittenAt     time.Time `json:"written_at"`
		Source        struct {
			URL string `json:"url"`
		} `json:"source"`
		Targets []json.RawMessage `json:"targets"`
	}
	dec := json.NewDecoder(f)
	if err := dec.Decode(&head); err != nil {
		return CacheInfo{}, fmt.Errorf("decode cache %q: %w", path, err)
	}
	if head.SchemaVersion != version.SchemaVersion {
		return CacheInfo{}, fmt.Errorf(
			"cache %q has schema_version %d, want %d",
			path, head.SchemaVersion, version.SchemaVersion)
	}

	return CacheInfo{
		Exists:        true,
		WrittenAt:     head.WrittenAt,
		Age:           now.Sub(head.WrittenAt),
		TargetCount:   len(head.Targets),
		SourceURL:     head.Source.URL,
		SchemaVersion: head.SchemaVersion,
	}, nil
}

// ---------------------------------------------------------------------------
// 转换
// ---------------------------------------------------------------------------

func toCacheTarget(t model.Target) cacheTarget {
	return cacheTarget{
		ID:        t.ID,
		IP:        t.IP,
		Port:      t.Port,
		IPVersion: string(t.IPVersion),
		Location: cacheLocation{
			CCA2:             t.Location.CCA2,
			IATA:             t.Location.IATA,
			Region:           t.Location.Region,
			City:             t.Location.City,
			Latitude:         t.Location.Latitude,
			Longitude:        t.Location.Longitude,
			Country:          t.Location.Country,
			CountryEN:        t.Location.CountryEN,
			HasCoordinates:   t.Location.HasCoordinates,
			FromColoFallback: t.Location.FromColoFallback,
		},
	}
}

func fromCacheTarget(ct cacheTarget) model.Target {
	ipVersion := model.IPVersion(ct.IPVersion)
	if ipVersion == "" {
		// 兼容早期缓存文件：根据 IP 推断版本。
		if addr, ok := model.ParseAddr(ct.IP); ok {
			ipVersion = model.IPVersionOf(addr)
		} else {
			ipVersion = model.IPVersionUnknown
		}
	}

	id := ct.ID
	if id == "" {
		id = model.TargetID(ct.IP, ct.Port)
	}

	return model.Target{
		ID:        id,
		IP:        ct.IP,
		Port:      ct.Port,
		IPVersion: ipVersion,
		Location: model.Location{
			CCA2:             ct.Location.CCA2,
			IATA:             ct.Location.IATA,
			Region:           ct.Location.Region,
			City:             ct.Location.City,
			Latitude:         ct.Location.Latitude,
			Longitude:        ct.Location.Longitude,
			Country:          ct.Location.Country,
			CountryEN:        ct.Location.CountryEN,
			HasCoordinates:   ct.Location.HasCoordinates,
			FromColoFallback: ct.Location.FromColoFallback,
		},
	}
}

// SortTargets 按 ID 排序目标列表。
//
// 缓存与扫描都保持"源顺序"以保证可复现；只有在导出、
// 对比两份列表等场景才需要稳定排序。
func SortTargets(targets []model.Target) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
}

// Command snapshotgen 把本地缓存的线路前缀打包成**内置快照**。
//
// 用法（在 internal/asnprefix 目录下）：
//
//	go generate ./...
//
// 输入是 data/asnprefix/*.json（由正常运行时的整表下载产生），
// 输出是 snapshot/snapshot.json.gz，由 snapshot.go 通过 go:embed
// 打进二进制。
//
// == 为什么要内置一份 ==
//
// 整表下载是单点故障：离线、站点挂掉、被中间设备拦截，
// 都会让线路名完全失效。内置快照让"认不出线路"退化成
// "用一份可能略旧的线路段"——后者远比前者有用。
//
// 快照会随着时间过期（骨干线路的 IP 段会变），因此它
// **只是兜底**，正常运行永远优先用下载或本地缓存。
package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotFile 是输出的快照结构。
type snapshotFile struct {
	// GeneratedAt 是打包时间。
	//
	// 运行时会把它在日志里说出来：使用者需要知道
	// "这份线路段有多旧"，否则会把过期数据当成现状。
	GeneratedAt string `json:"generated_at"`

	// Source 说明数据来源。
	Source string `json:"source"`

	// ASNs 是 ASN -> 前缀列表。
	ASNs map[string][]string `json:"asns"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "snapshotgen:", err)
		os.Exit(1)
	}
}

func run() error {
	// 脚本按约定在包目录下运行（go generate 的行为）。
	cacheDir := filepath.Join("..", "..", "data", "asnprefix")
	if _, err := os.Stat(cacheDir); err != nil {
		return fmt.Errorf("找不到缓存目录 %s（先正常运行一次以产生缓存）: %w", cacheDir, err)
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return err
	}

	snapshot := snapshotFile{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      "iptoasn.com ip2asn-combined.tsv.gz",
		ASNs:        make(map[string][]string),
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || strings.Contains(name, ".tmp") {
			continue
		}

		blob, readErr := os.ReadFile(filepath.Join(cacheDir, name))
		if readErr != nil {
			return readErr
		}

		var prefixes []string
		if err := json.Unmarshal(blob, &prefixes); err != nil {
			return fmt.Errorf("解析 %s: %w", name, err)
		}
		if len(prefixes) == 0 {
			// 空列表有信息量（"这个 ASN 没有段"），但内置快照里
			// 存空数组没意义——运行时缺条目与空条目等价。
			continue
		}

		snapshot.ASNs[strings.TrimSuffix(name, ".json")] = prefixes
	}

	if len(snapshot.ASNs) == 0 {
		return fmt.Errorf("缓存里没有任何前缀，拒绝生成空快照")
	}

	// 按 ASN 排序输出，让快照文件的 diff 稳定（便于 code review）。
	ordered := make(map[string][]string, len(snapshot.ASNs))
	keys := make([]string, 0, len(snapshot.ASNs))
	for key := range snapshot.ASNs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ordered[key] = snapshot.ASNs[key]
	}
	snapshot.ASNs = ordered

	if err := os.MkdirAll("snapshot", 0o755); err != nil {
		return err
	}

	target := filepath.Join("snapshot", "snapshot.json.gz")
	file, err := os.Create(target)
	if err != nil {
		return err
	}

	gz, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		_ = file.Close()
		return err
	}

	encoder := json.NewEncoder(gz)
	if err := encoder.Encode(snapshot); err != nil {
		_ = gz.Close()
		_ = file.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	info, err := os.Stat(target)
	if err != nil {
		return err
	}

	total := 0
	for _, prefixes := range snapshot.ASNs {
		total += len(prefixes)
	}
	fmt.Printf("已写入 %s\n  %d 个 ASN，%d 条前缀，%.0f KB\n  生成时间 %s\n",
		target, len(snapshot.ASNs), total, float64(info.Size())/1024, snapshot.GeneratedAt)

	return nil
}

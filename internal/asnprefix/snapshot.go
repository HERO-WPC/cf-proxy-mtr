package asnprefix

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// 内置快照：整表下载失败、且本地也没有可用缓存时的**最后兜底**。
//
// 为什么需要它：整表下载是单点故障。离线、站点挂掉、被中间设备
// 拦掉，都会让线路名彻底失效。有了一份内置快照，"认不出线路"
// 就退化成"用一份可能略旧的线路段"——后者远比前者有用。
//
// 它是**兜底**而不是数据源：正常运行永远优先用刚下载的数据或
// 本地缓存。快照会过期（骨干线路的 IP 段会随时间变化），
// 因此使用时会明确说出它的生成时间，让人知道自己在看多旧的数据。
//
//go:generate go run ./snapshotgen
//go:embed snapshot/snapshot.json.gz
var snapshotFS embed.FS

// snapshotPath 是嵌入文件的路径。
const snapshotPath = "snapshot/snapshot.json.gz"

// snapshotFile 是快照文件的结构（与 snapshotgen 输出一致）。
type snapshotFile struct {
	// GeneratedAt 是打包时间（RFC3339）。
	GeneratedAt string `json:"generated_at"`

	// Source 说明数据来源。
	Source string `json:"source"`

	// ASNs 是 ASN -> 前缀列表。
	ASNs map[string][]string `json:"asns"`
}

// loadSnapshot 读出内置快照里我们关心的那些 ASN 的前缀。
//
// 返回值为 ASN -> 前缀列表，以及快照的生成时间（供日志说明新旧）。
// 找不到嵌入数据时返回错误——那说明构建时漏了 go:generate，
// 是构建问题，不该被静默吞掉。
func loadSnapshot(wanted map[string]bool) (map[string][]string, string, error) {
	file, err := snapshotFS.Open(snapshotPath)
	if err != nil {
		return nil, "", fmt.Errorf("asnprefix: 内置快照缺失: %w", err)
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, "", fmt.Errorf("asnprefix: 内置快照不是有效的 gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	// 限制解压体积：嵌入数据虽由我们生成，但设一个上限可以
	// 避免"生成过程出错写出巨大文件"变成内存问题。
	limited := io.LimitReader(gz, 64<<20)

	var snapshot snapshotFile
	if err := json.NewDecoder(limited).Decode(&snapshot); err != nil {
		return nil, "", fmt.Errorf("asnprefix: 解析内置快照: %w", err)
	}

	out := make(map[string][]string, len(wanted))
	for asn := range wanted {
		// 两边都过一遍归一化：快照里存的是 "AS4809"，
		// 而调用方可能传 "4809"，不做归一化会一个都取不到。
		prefixes, ok := snapshot.ASNs[normalizeASNKey(asn)]
		if !ok || len(prefixes) == 0 {
			continue
		}
		out[asn] = prefixes
	}

	return out, snapshot.GeneratedAt, nil
}

// loadFromSnapshot 用内置快照填满 resolver。
//
// 返回实际用上的 ASN 数。返回 0 表示快照里没有我们关心的线路
// （例如 ASN 清单变了但快照没重新生成）——那时调用方应当如实
// 说明"线路名不可用"，而不是假装成功。
func (r *Resolver) loadFromSnapshot() (int, string, error) {
	wanted := make(map[string]bool, len(r.asnOrder))
	for _, asn := range r.asnOrder {
		wanted[normalizeASNKey(asn)] = true
	}

	prefixesByASN, generatedAt, err := loadSnapshot(wanted)
	if err != nil {
		return 0, "", err
	}

	loaded := 0
	for _, asn := range r.asnOrder {
		prefixes, ok := prefixesByASN[asn]
		if !ok {
			continue
		}
		set, _, buildErr := NewSet(prefixes)
		if buildErr != nil || set.Len() == 0 {
			continue
		}

		r.mu.Lock()
		r.byASN[asn] = set
		r.mu.Unlock()
		loaded++
	}

	return loaded, generatedAt, nil
}

// SnapshotGeneratedAt 返回内置快照的生成时间（供排查）。
//
// 失败时返回空串与错误；调用方通常只在诊断输出里用。
func SnapshotGeneratedAt() (string, error) {
	_, generatedAt, err := loadSnapshot(nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(generatedAt), nil
}

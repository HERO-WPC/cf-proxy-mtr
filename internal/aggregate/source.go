package aggregate

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SourceKind 描述一个输入文件的读取方式。
type SourceKind int

const (
	// SourcePlain 是纯文本 JSONL。
	SourcePlain SourceKind = iota

	// SourceGzip 是 gzip 压缩的 JSONL。
	SourceGzip
)

// String 实现 fmt.Stringer。
func (k SourceKind) String() string {
	if k == SourceGzip {
		return "gzip"
	}
	return "plain"
}

// Source 是一个待读取的输入文件。
type Source struct {
	Path string
	Kind SourceKind
}

// Open 打开文件并返回读取器（自动处理 gzip）。
//
// 返回的 close 必须被调用。
func (s Source) Open() (io.Reader, func() error, error) {
	file, err := os.Open(s.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", s.Path, err)
	}

	closeFn := func() error { return file.Close() }

	if s.Kind != SourceGzip {
		return file, closeFn, nil
	}

	reader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("open gzip stream %s: %w", s.Path, err)
	}

	return reader, func() error {
		// 顺序：先关 gzip（校验尾部），再关文件。
		//
		// gzip.Reader.Close 不校验 CRC 时也要调用，否则底层
		// 读取器不会被标记为"读完"，截断的文件可能被当成正常文件。
		gzipErr := reader.Close()
		fileErr := file.Close()
		if gzipErr != nil {
			return fmt.Errorf("close gzip stream %s: %w", s.Path, gzipErr)
		}
		return fileErr
	}, nil
}

// DiscoverSources 展开输入路径列表。
//
// 规则：
//
//   - 目录会被递归扫描，收集 .jsonl / .jsonl.gz / .gz 文件；
//   - 单个文件原样加入（即使扩展名不常见，也按内容探测）；
//   - 结果**排序**，保证同一批输入在不同机器上得到相同的处理顺序。
//
// 为什么接受目录：众测数据的自然形态就是"一个目录里很多批次"。
func DiscoverSources(paths []string) ([]Source, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no input paths given")
	}

	byPath := make(map[string]Source)
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}

		if !info.IsDir() {
			byPath[path] = classifySource(path)
			continue
		}

		// 目录：递归收集。
		walkErr := filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !looksLikeJSONL(p) {
				return nil
			}
			byPath[p] = classifySource(p)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("scan directory %s: %w", path, walkErr)
		}
	}

	if len(byPath) == 0 {
		return nil, fmt.Errorf("no JSONL input files found in the given paths")
	}

	out := make([]Source, 0, len(byPath))
	for _, source := range byPath {
		out = append(out, source)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// looksLikeJSONL 报告文件名是否像 JSONL 数据。
func looksLikeJSONL(path string) bool {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".jsonl"),
		strings.HasSuffix(lower, ".jsonl.gz"),
		strings.HasSuffix(lower, ".ndjson"),
		strings.HasSuffix(lower, ".jsonl.zst"):
		return true
	case strings.HasSuffix(lower, ".gz"):
		// 只认 .jsonl.gz 之类的名字；普通的 .gz 可能是别的东西。
		return strings.Contains(lower, "jsonl")
	default:
		return false
	}
}

// classifySource 按文件名判断是否需要解压。
//
// 只看文件名是个折中：真正的判定应当看魔数，但那需要先读文件头。
// 这里两种都要处理——文件名的判断留给"目录扫描"，
// 而**单个显式给出的文件**会在 LoadSources 里按魔数兜底。
func classifySource(path string) Source {
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		return Source{Path: path, Kind: SourceGzip}
	}
	return Source{Path: path, Kind: SourcePlain}
}

// LoadSources 读取并聚合所有输入文件。
//
// 单个文件读取失败会**记录并继续**：众测数据里混着几个坏文件
// 是常态，因为一个坏文件就放弃整批数据没有道理。
// 失败的文件会出现在返回的 error 列表里，由调用方如实报出。
func (c *Collector) LoadSources(sources []Source) []SourceError {
	failures := make([]SourceError, 0)

	for _, source := range sources {
		reader, closeFn, err := source.Open()
		if err != nil {
			failures = append(failures, SourceError{Path: source.Path, Err: err})
			continue
		}

		c.Load.Sources++
		loadErr := Load(reader, &c.Load, func(row Row) error {
			c.Add(row)
			return nil
		})

		if closeErr := closeFn(); closeErr != nil && loadErr == nil {
			loadErr = closeErr
		}
		if loadErr != nil {
			failures = append(failures, SourceError{Path: source.Path, Err: loadErr})
		}
	}

	return failures
}

// SourceError 是读取某个文件时的失败。
type SourceError struct {
	Path string
	Err  error
}

// Error 实现 error。
func (e SourceError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

// Package csvstore 把测量结果**实时**追加到 CSV 文件。
//
// 为什么不用数据库：本工具的实际用法是"跑一轮、看结果"，
// 而 SQLite 带来的是会话、迁移、幂等去重、恢复逻辑——一整套
// 使用者并不需要的东西。CSV 可以直接看、直接拖进表格，
// 而且**崩溃/断电时已经写下去的行仍然在**。
//
// == 三个关键约定 ==
//
//  1. **每行写完立即刷盘**（Flush，不是 fsync）：中途 Ctrl+C、
//     进程被杀、机器断电，已完成的测量都不会丢。代价是每次
//     写一个小 syscall，对本工具的量级（每秒几十行）毫无影响。
//  2. **表头只写一次**：文件已存在且非空时不再写表头，
//     否则追加模式下表头会出现在文件中间。
//  3. **不做会话、不做恢复**：每次启动就是一个新文件，
//     中断就是中断——已经写下去的行就是结果，没有"续测"这回事。
package csvstore

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Header 是 CSV 的列顺序。
//
// 顺序按"看表格的人怎么读"排：先认出是哪条目标、什么时候测的，
// 再看结果，最后是线路细节。新增列一律**追加在末尾**——
// 列名与列序是契约，改了就破坏别人已经做好的表。
var Header = []string{
	"timestamp_utc",
	"target",
	"ip",
	"port",
	"success",
	"latency_ms",
	"error_type",
	"error_message",
	"hop_count",
	"as_path",
	"hops",
	"client_version",
	"cca2",
}

// Row 是一条待写入的结果。
//
// 它同时承载"探测"与"跟踪"两类信息：一个目标先有探测结果，
// 跟踪完成后再补一行带线路信息的记录。这样同一个 CSV 里既能看到
// "通不通、多快"，也能看到"走的是哪条线路"。
type Row struct {
	// Timestamp 是这次测量的时间。
	Timestamp time.Time

	// Target 是 "IP:Port"。
	Target string

	// IP / Port 是拆开的形式（便于表格排序筛选）。
	IP   string
	Port int

	// Success 表示这次测量是否成功。
	Success bool

	// LatencyMS 是延迟；<=0 表示**没有测到**（超时/失败）。
	//
	// 0 与"没测到"必须能区分，因此未测到时写空字符串而不是 0。
	LatencyMS float64

	// ErrorType / ErrorMessage 是失败信息（成功时为空）。
	//
	// ErrorMessage 里可能含 IP 与路径，写入前会被清洗
	// （见 SanitizeErrorMessage）。
	ErrorType    string
	ErrorMessage string

	// HopCount 是路径跳数（没有跟踪时为 0）。
	HopCount int

	// ASPath 是线路串，例如 "163 > CN2 > Cloudflare"。
	ASPath string

	// Hops 是逐跳明细，例如 "1:10.0.0.1;2:203.0.113.4:163"。
	Hops string

	// ClientVersion 是产生这条数据的程序版本。
	ClientVersion string

	// CCA2 是目标的国家两位码（来自目标列表的 location.cca2）。
	//
	// 放进 CSV 而不是事后去目标列表里查：CSV 是这个项目**唯一**的
	// 数据源，界面要按国家筛选/分组时不能依赖"目标列表此刻是否还在、
	// 内容是否已经变了"。目标会随上游数据变动，落盘的国家不会。
	//
	// 追加在末尾：列名与列序是契约，插在中间会打乱别人已经做好的表。
	CCA2 string
}

// Store 是一个 CSV 追加写入器。
//
// 并发安全：探测与跟踪会从多个 goroutine 写入。
type Store struct {
	mu     sync.Mutex
	file   *os.File
	writer *csv.Writer
	path   string
	rows   int
}

// Options 是创建 Store 的参数。
type Options struct {
	// Path 是 CSV 文件路径。
	Path string

	// Append 为真时追加到已有文件（不重复写表头）。
	Append bool
}

// Open 打开（或创建）CSV 文件。
func Open(opts Options) (*Store, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, fmt.Errorf("csvstore: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("csvstore: create directory: %w", err)
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if opts.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("csvstore: open %s: %w", path, err)
	}

	store := &Store{file: file, writer: csv.NewWriter(file), path: path}

	// 已有内容的文件在追加模式下不写表头，否则表头会夹在数据中间。
	needsHeader := true
	if opts.Append {
		if info, statErr := file.Stat(); statErr == nil && info.Size() > 0 {
			needsHeader = false
		}
	}

	if needsHeader {
		if err := store.writer.Write(Header); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("csvstore: write header: %w", err)
		}
		store.writer.Flush()
		if err := store.writer.Error(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("csvstore: flush header: %w", err)
		}
	}

	return store, nil
}

// Path 返回文件路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Rows 返回本次已写入的数据行数（不含表头）。
func (s *Store) Rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// Append 写入一行并**立即刷盘**。
//
// 用 Flush 而不是 Sync：Flush 把数据交给操作系统（进程被杀也不会丢），
// 而 Sync 会真的等磁盘——每秒几十次 Sync 会明显拖慢测量，
// 而"操作系统崩溃"不是本工具需要防的场景。
func (s *Store) Append(row Row) error {
	if s == nil {
		return fmt.Errorf("csvstore: nil store")
	}

	record := toRecord(row)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writer.Write(record); err != nil {
		return fmt.Errorf("csvstore: write row: %w", err)
	}
	// 每行都 Flush，这是"中断不丢已完成结果"的实现方式。
	s.writer.Flush()
	if err := s.writer.Error(); err != nil {
		return fmt.Errorf("csvstore: flush row: %w", err)
	}
	s.rows++
	return nil
}

// Close 关闭文件（幂等）。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	s.writer.Flush()
	flushErr := s.writer.Error()

	file := s.file
	s.file = nil

	closeErr := file.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// toRecord 把一行转成字符串切片。
func toRecord(row Row) []string {
	timestamp := row.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	return []string{
		timestamp.UTC().Format(time.RFC3339),
		row.Target,
		row.IP,
		strconv.Itoa(row.Port),
		strconv.FormatBool(row.Success),
		formatLatency(row.LatencyMS),
		row.ErrorType,
		SanitizeErrorMessage(row.ErrorMessage),
		hopCountOrEmpty(row.HopCount),
		row.ASPath,
		row.Hops,
		row.ClientVersion,
		row.CCA2,
	}
}

// formatLatency 格式化延迟。
//
// 未测到（<=0）写**空字符串**：表格里 0ms 与"没测到"是两回事，
// 混在一起会让平均延迟、分位数全部失真。
func formatLatency(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return strconv.FormatFloat(ms, 'f', 3, 64)
}

// hopCountOrEmpty 在未跟踪时留空。
func hopCountOrEmpty(count int) string {
	if count <= 0 {
		return ""
	}
	return strconv.Itoa(count)
}

// SanitizeErrorMessage 清洗错误信息，让它可以安全地进结果文件。
//
// 做两件事：
//
//  1. 去掉**本机文件路径**。Go 的错误里偶尔会带上可执行文件路径
//     （例如找不到 NextTrace 时），那是这台机器的信息，不该进结果。
//  2. 截断过长的信息（某些系统错误会把整行命令都带进来）。
//
// 刻意**不**去掉目标地址：`dial tcp 1.2.3.4:443: timeout` 里的
// 地址正是"哪个目标失败了"，去掉了反而看不懂。
func SanitizeErrorMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return ""
	}

	trimmed = stripLocalPaths(trimmed)

	// 300 字符足够表达任何真实错误，更长说明是噪声。
	const maxLen = 300
	if len(trimmed) > maxLen {
		// 按**字节**截断可能切断一个 UTF-8 字符，因此回退到
		// 最后一个完整的 rune 边界。
		cut := maxLen
		for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
			cut--
		}
		trimmed = trimmed[:cut] + "…"
	}
	return trimmed
}

// stripLocalPaths 把常见的本机路径替换成占位符。
//
// 覆盖 Windows 与 Unix 两类写法：
//
//	C:\Users\alice\tools\nexttrace.exe  -> <path>
//	/home/alice/tools/nexttrace          -> <path>
//
// 只处理"看起来像绝对路径"的片段，避免误伤普通文字。
func stripLocalPaths(message string) string {
	var b strings.Builder
	b.Grow(len(message))

	i := 0
	for i < len(message) {
		if start, length, ok := matchPath(message, i); ok {
			b.WriteString("<path>")
			i = start + length
			continue
		}
		b.WriteByte(message[i])
		i++
	}
	return b.String()
}

// matchPath 判断 s[i:] 是否以一个绝对路径开头。
//
// 返回该路径的长度。识别规则故意保守：
//
//	Windows：<盘符>:\ 或 <盘符>:/
//	Unix：   / 后面跟至少两段（避免把 "dial tcp 1.2.3.4:443" 里的
//	        单个斜杠、或 "i/o timeout" 里的斜杠当成路径）
func matchPath(s string, i int) (start, length int, ok bool) {
	// Windows 盘符形式。
	if i+2 < len(s) && isDriveLetter(s[i]) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/') {
		end := i + 3
		for end < len(s) && s[end] != ' ' && s[end] != '"' && s[end] != '\'' && s[end] != '\n' {
			end++
		}
		return i, end - i, end > i+3
	}

	// Unix 形式：以 '/' 开头，且至少包含两段。
	if s[i] == '/' && (i == 0 || s[i-1] == ' ' || s[i-1] == '"' || s[i-1] == '\'') {
		end := i + 1
		slashes := 0
		for end < len(s) {
			c := s[end]
			if c == ' ' || c == '"' || c == '\'' || c == '\n' {
				break
			}
			if c == '/' {
				slashes++
			}
			end++
		}
		// 至少要有一个内部斜杠（两段）才算路径，
		// 否则 "i/o" 这种会被误伤。
		if slashes >= 1 && end-i > 3 {
			return i, end - i, true
		}
	}
	return 0, 0, false
}

// isDriveLetter 判断是否为 Windows 盘符。
func isDriveLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

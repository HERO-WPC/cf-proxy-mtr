// Package applog 提供程序的日志设施。
//
// 存在的直接原因：本项目现在有一个**脱离命令行启动**的入口
// （图形界面）。没有终端就没有 stderr，而"双击启动后什么都没发生"
// 是最难排查的一类故障——用户看不到任何错误，也无法告诉你他看到了什么。
//
// 因此约定是：
//
//  1. 每条日志**同时**写到控制台（如果存在）与日志文件；
//  2. 日志文件路径在启动时就被打印/记录出来，用户能找到它；
//  3. 日志文件有大小上限与轮转，不会无限增长吃满磁盘。
//
// 与"控制台输出"的分工（这一点很重要，否则两套东西会打架）：
//
//	applog  给**开发者与排查**看：时间戳、级别、结构化细节
//	fmt.*   给**命令行的正常使用者**看：扫描汇总、进度、错误提示
//
// 不把 CLI 的每一条输出都塞进 applog：那会让日志文件里
// 混着大量"给人看的表格"，反而查不到真正重要的信息。
package applog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 默认参数。
const (
	// DefaultDir 是日志目录（相对项目根）。
	DefaultDir = "data/logs"

	// DefaultFileName 是日志文件名。
	DefaultFileName = "cf-route-tester.log"

	// DefaultMaxBytes 是单个日志文件的大小上限。
	//
	// 5 MiB 足够记录很多次扫描的关键事件，又不会让用户
	// 在磁盘上留一个几百兆的文件。
	DefaultMaxBytes = 5 << 20

	// DefaultMaxBackups 是保留的历史日志文件数。
	DefaultMaxBackups = 3
)

// Options 是日志配置。
type Options struct {
	// Dir 是日志目录。为空表示**只写控制台，不写文件**
	// （例如用户显式要求 --log-file="" 时）。
	Dir string

	// FileName 是日志文件名。
	FileName string

	// Console 为真时同时写到 Console 输出流。
	Console io.Writer

	// Level 是日志级别（默认 Info）。
	Level slog.Level

	// MaxBytes / MaxBackups 控制轮转。
	MaxBytes   int64
	MaxBackups int

	// AlsoStderr 为真时把日志同时写一份到 os.Stderr。
	//
	// 这是给"从终端启动 GUI"用的：用户在终端里能立刻看到
	// 启动过程与错误，而不必去翻日志文件。
	AlsoStderr bool
}

// Logger 是日志器。
type Logger struct {
	*slog.Logger

	// path 是实际写入的日志文件路径；为空表示没有写文件。
	path string

	// closer 关闭底层文件。
	closer io.Closer
}

// Path 返回日志文件路径（可能为空）。
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// 下面四个方法是**零值安全**的包装。
//
// 为什么不直接用内嵌的 slog.Logger：`var l *Logger` 或
// `&Logger{}` 上的 l.Info(...) 会解引用空的内嵌指针而 panic。
// 日志调用遍布各处，其中任何一处拿到未初始化的日志器就崩掉整程序，
// 而这个代价明显不值得——日志本来就是辅助设施。
//
// 因此：nil 接收者、nil 内嵌器都退化为"什么都不做"。

// Debug 记录一条调试日志。
func (l *Logger) Debug(msg string, args ...any) { l.log(slog.LevelDebug, msg, args...) }

// Info 记录一条信息日志。
func (l *Logger) Info(msg string, args ...any) { l.log(slog.LevelInfo, msg, args...) }

// Warn 记录一条警告日志。
func (l *Logger) Warn(msg string, args ...any) { l.log(slog.LevelWarn, msg, args...) }

// Error 记录一条错误日志。
func (l *Logger) Error(msg string, args ...any) { l.log(slog.LevelError, msg, args...) }

// log 是四个级别方法的公共实现。
func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l == nil || l.Logger == nil {
		return
	}
	l.Logger.Log(context.Background(), level, msg, args...)
}

// Close 关闭日志文件。
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	closer := l.closer
	l.closer = nil
	return closer.Close()
}

// New 创建日志器。
//
// 即使创建日志文件失败也**不返回错误**，而是退化成只写控制台，
// 并把失败原因写进控制台。理由：日志是辅助设施，
// 它挂了不该让主程序起不来。
func New(opts Options) *Logger {
	if opts.FileName == "" {
		opts.FileName = DefaultFileName
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxBackups <= 0 {
		opts.MaxBackups = DefaultMaxBackups
	}

	// 组装输出目标。
	writers := make([]io.Writer, 0, 3)
	if opts.Console != nil {
		writers = append(writers, opts.Console)
	}
	if opts.AlsoStderr {
		writers = append(writers, os.Stderr)
	}

	logger := &Logger{}

	if strings.TrimSpace(opts.Dir) != "" {
		writer, path, err := openRotating(opts.Dir, opts.FileName, opts.MaxBytes, opts.MaxBackups)
		if err != nil {
			// 退化成只写控制台，但把原因说出来。
			if opts.Console != nil {
				fmt.Fprintf(opts.Console, "warning: cannot open log file in %s: %v\n", opts.Dir, err)
			}
			if opts.AlsoStderr {
				fmt.Fprintf(os.Stderr, "warning: cannot open log file in %s: %v\n", opts.Dir, err)
			}
		} else {
			logger.path = path
			logger.closer = writer.(io.Closer)
			// 日志文件放在最后：控制台输出优先（人能立刻看到）。
			writers = append(writers, writer)
		}
	}

	var out io.Writer
	switch len(writers) {
	case 0:
		out = io.Discard
	case 1:
		out = writers[0]
	default:
		out = io.MultiWriter(writers...)
	}

	handler := slog.NewTextHandler(out, &slog.HandlerOptions{
		Level: opts.Level,
		// 时间戳用本地时间：用户排查时对照的是自己的钟表。
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				if t, ok := attr.Value.Any().(time.Time); ok {
					attr.Value = slog.StringValue(t.Local().Format("2006-01-02 15:04:05.000"))
				}
			}
			return attr
		},
	})

	logger.Logger = slog.New(handler)
	return logger
}

// Discard 返回一个什么都不做的日志器（测试用）。
func Discard() *Logger {
	return &Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// ---------------------------------------------------------------------------
// 轮转
// ---------------------------------------------------------------------------

// rotatingFile 是一个带大小上限的日志文件。
//
// 自己实现而不引第三方库：需求很简单（超了就改名、只留 N 个备份），
// 而项目的依赖面已经被刻意压到只剩 SQLite 驱动。
type rotatingFile struct {
	mu         sync.Mutex
	dir        string
	name       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

// openRotating 打开（或创建）日志文件，必要时先轮转。
func openRotating(dir, name string, maxBytes int64, maxBackups int) (io.Writer, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create log directory: %w", err)
	}

	path := filepath.Join(dir, name)
	rotator := &rotatingFile{
		dir:        dir,
		name:       name,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
	}

	// 启动时就检查一次大小：上次运行留下的文件可能已经超过上限。
	//
	// 注意这里**不能**在 rotateLocked 之后无条件再调一次 openLocked：
	// rotateLocked 自己会打开文件，再打开一次会把前一个句柄覆盖掉，
	// 而那个句柄永远不会被关闭——文件于是被本进程占住，
	// 轮转与删除都会失败。这个 bug 实测复现过（文件在进程退出前
	// 无法删除、Windows 上报"being used by another process"）。
	if info, err := os.Stat(path); err == nil && info.Size() >= maxBytes {
		if err := rotator.rotateLocked(); err != nil {
			return nil, "", err
		}
		// rotateLocked 成功后文件已经打开，直接返回。
		return rotator, path, nil
	}

	if err := rotator.openLocked(); err != nil {
		return nil, "", err
	}
	return rotator, path, nil
}

// openLocked 以追加方式打开当前日志文件。
func (r *rotatingFile) openLocked() error {
	file, err := os.OpenFile(filepath.Join(r.dir, r.name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	r.file = file
	r.size = info.Size()
	return nil
}

// Write 实现 io.Writer，并在超限时轮转。
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return 0, fmt.Errorf("applog: log file is closed")
	}

	// 先判断"写完是否会超"：把这一条完整写进当前文件再轮转，
	// 可以保证日志行不会被拦腰截断（截断的日志比没有更难看懂）。
	if r.size+int64(len(p)) > r.maxBytes {
		// 轮转失败不阻止本次写入：继续往当前文件追加，
		// 否则用户会突然丢失日志。失败原因由 rotateLocked 负责记录。
		_ = r.rotateLocked()
	}

	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// Close 关闭文件。
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeLocked()
}

// closeLocked 关闭并清空当前句柄。
//
// 必须把 r.file 置空：否则调用方会以为文件仍然打开，
// 而重复 Close 的语义也会变得不确定。
func (r *rotatingFile) closeLocked() error {
	if r.file == nil {
		return nil
	}
	file := r.file
	r.file = nil
	return file.Close()
}

// rotateLocked 把当前文件改名为备份，并新建一个空文件。
//
// 命名从 .1 开始，数字越大越旧：cf-route-tester.log.1 是最近一次轮转。
//
// 这里的容错是**踩过坑之后加的**：早先的实现无条件先关闭当前文件、
// 再打开同名文件。若改名失败（Windows 上其它句柄占着旧文件时会发生），
// 就会同时泄漏一个旧句柄**并**让新句柄指向同一个文件——
// 结果是文件不再受大小上限约束（实测 100 字节上限下涨到 5.8 KB），
// 而且进程退出后文件仍被占用、无法删除。
//
// 因此改名失败时改为"原地截断"，保证始终只有一个句柄、大小仍然有界。
func (r *rotatingFile) rotateLocked() error {
	base := filepath.Join(r.dir, r.name)

	if err := r.renameCurrentLocked(base); err != nil {
		// 改名不可行：原地截断，仍然守住大小上限。
		if r.file == nil {
			if openErr := r.openLocked(); openErr != nil {
				return openErr
			}
		}
		if truncErr := r.file.Truncate(0); truncErr != nil {
			return fmt.Errorf("applog: rotate failed (%v) and truncate failed: %w", err, truncErr)
		}
		if _, seekErr := r.file.Seek(0, io.SeekStart); seekErr != nil {
			return fmt.Errorf("applog: rotate failed (%v) and seek failed: %w", err, seekErr)
		}
		r.size = 0
		return nil
	}

	if err := r.openLocked(); err != nil {
		return err
	}

	// 顺手清理超出保留数量的备份。
	r.pruneBackupsLocked(base)
	return nil
}

// renameCurrentLocked 关闭当前文件并把它改名为备份。
func (r *rotatingFile) renameCurrentLocked(base string) error {
	// 先关句柄：Windows 上占着句柄的文件无法改名。
	// 注意这里**不**用 closeLocked 的"吞掉错误"行为——
	// 关不掉就没法改名，应当走原地截断的兜底路径。
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			r.file = nil
			return fmt.Errorf("close before rotate: %w", err)
		}
		r.file = nil
	}

	// 丢掉最旧的备份，然后整体后移一位。
	_ = os.Remove(fmt.Sprintf("%s.%d", base, r.maxBackups))
	for i := r.maxBackups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", base, i), fmt.Sprintf("%s.%d", base, i+1))
	}
	if err := os.Rename(base, base+".1"); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(base), err)
	}
	return nil
}

// pruneBackupsLocked 删除超出保留数量的备份。
//
// 正常路径下 renameCurrentLocked 已经把最旧的挤掉了；
// 这里再兜一次，防止因为改名为备份失败而积累出多余文件。
func (r *rotatingFile) pruneBackupsLocked(base string) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	prefix := r.name + "."
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		index := 0
		if _, err := fmt.Sscanf(strings.TrimPrefix(name, prefix), "%d", &index); err != nil {
			continue
		}
		if index > r.maxBackups {
			_ = os.Remove(filepath.Join(r.dir, name))
		}
	}
}

// LogFilePath 返回某个目录下的日志文件路径（供 CLI 提前告知用户）。
func LogFilePath(dir, name string) string {
	if name == "" {
		name = DefaultFileName
	}
	return filepath.Join(dir, name)
}

// Package storage 负责本地 SQLite 持久化。
//
// 设计约束（对应需求第 14、15、17、18、40 条）：
//
//   - 历史数据只追加，绝不覆盖：measurements / traces 是时间序列，
//     同一个目标的多次测量必须同时保留。只有维度表（targets / collectors /
//     scan_sessions）才使用 UPSERT。
//   - 数据库结构不直接绑定外部原始 JSON：写入的是 model / probe 的
//     内部统一结构，NextTrace 的原始 JSON 只作为诊断信息存放在附列里。
//   - 不保存任何隐私信息：没有公网 IP、MAC、内网 IP、主机名字段。
//     保存的只有匿名 collector_id 与粗粒度的地区 / 运营商。
//   - 表结构变更一律通过 migrations.go 中的版本化迁移完成，
//     不依赖运行时"探测列是否存在"。
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// 纯 Go 的 SQLite 驱动（modernc.org/sqlite）。
	//
	// 选择它的理由是硬性的：项目要求 Windows 双击 exe 可用、
	// 并且要交叉编译 linux / darwin / windows × amd64 / arm64。
	// mattn/go-sqlite3 需要 CGO，会同时破坏这两点。
	//
	// 代价：二进制体积明显增大（驱动内含 SQLite 的翻译版本）。
	// 这是为了"单文件、无外部依赖"付出的必要成本。
	_ "modernc.org/sqlite"
)

// 默认配置。
const (
	// DefaultPath 是本地数据库的默认路径。
	DefaultPath = "data/results.db"

	// DefaultBusyTimeout 是等待数据库锁的最长时间。
	//
	// 5 秒足够覆盖"另一个进程正在写"的常见情况（例如用户同时开着
	// 两个终端），又不会让网络测量的进度被一次锁等待拖太久。
	DefaultBusyTimeout = 5 * time.Second

	// driverName 是 database/sql 使用的驱动名。
	driverName = "sqlite"
)

// Config 是数据库配置。
type Config struct {
	// Path 是数据库文件路径。":memory:" 表示内存数据库（测试用）。
	Path string

	// BusyTimeout 是等待写锁的超时。
	BusyTimeout time.Duration

	// ReadOnly 以只读模式打开（用于 query / export 不修改数据）。
	ReadOnly bool
}

// DefaultConfig 返回默认数据库配置。
func DefaultConfig() Config {
	return Config{
		Path:        DefaultPath,
		BusyTimeout: DefaultBusyTimeout,
	}
}

// Store 是对本地 SQLite 数据库的封装。
//
// 并发安全：Store 本身只持有 *sql.DB（database/sql 是并发安全的），
// 因此可以被多个 goroutine 共享。批量写入额外提供了 Writer，
// 见 writer.go——那是为了把大量 INSERT 合并进少数事务。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开（必要时创建）数据库，并完成迁移。
//
// 目录不存在时自动创建；迁移是幂等的，重复调用不会重复执行。
// 返回的 Store 必须在用完后 Close。
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		cfg.Path = DefaultPath
	}
	if cfg.BusyTimeout <= 0 {
		cfg.BusyTimeout = DefaultBusyTimeout
	}

	if !isMemoryPath(cfg.Path) {
		dir := filepath.Dir(cfg.Path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database dir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open(driverName, dsn(cfg))
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", cfg.Path, err)
	}

	// SQLite 的写入者只能有一个：把连接数限制住，
	// 避免 database/sql 在池里创建多个写连接互相抢锁、
	// 把 busy timeout 全部消耗在内部竞争上。
	//
	// 读取由同一个连接串行化，对本项目（单进程 CLI）是合适的取舍：
	// 我们宁可让读取稍微排队，也不要写失败。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	store := &Store{db: db, path: cfg.Path}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect sqlite %q: %w", cfg.Path, err)
	}

	if err := store.applyPragmas(ctx, cfg); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 只读模式不允许迁移（会立刻失败），也通常不是创建数据库的场景。
	if !cfg.ReadOnly {
		if err := store.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	return store, nil
}

// dsn 构造 modernc.org/sqlite 的连接串。
//
// 注意 busy_timeout 是用整数拼接进 DSN 的：SQLite 的 PRAGMA 不接受
// 绑定参数，而这里用的是我们自己求出的 int64 毫秒数，不存在注入风险。
//
// 路径有两种形态，必须分开处理：
//
//	":memory:"                        纯内存库
//	"file:name?mode=memory&cache=shared"  已经带查询参数的 URI（测试用）
//	"data/results.db"                 普通文件路径
//
// 早期实现无条件在后面拼 "?..."，导致第二种形态变成
// "file:x?mode=memory&cache=shared?_pragma=..."，
// 参数被整体吃进 mode 的值里而报 "no such cache mode"。
// 因此这里必须先判断是否已经有查询串。
func dsn(cfg Config) string {
	busyMS := cfg.BusyTimeout.Milliseconds()
	if busyMS <= 0 {
		busyMS = DefaultBusyTimeout.Milliseconds()
	}

	params := url.Values{}
	// WAL：写入不阻塞读取，且崩溃后恢复更可靠。
	// 内存库不支持 WAL，但设置它也不会报错（SQLite 会保持 memory 模式），
	// 因此这里不再按库类型分支，避免两套路径产生行为差异。
	params.Set("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyMS, 10)+")")
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "synchronous(NORMAL)")
	if cfg.ReadOnly {
		params.Set("mode", "ro")
	}

	path, existingQuery := splitPathAndQuery(cfg.Path)
	merged := url.Values{}
	for k, vs := range existingQuery {
		for _, v := range vs {
			merged.Add(k, v)
		}
	}
	for k, vs := range params {
		for _, v := range vs {
			// 调用方显式给出的参数优先于我们的默认值。
			if merged.Has(k) {
				continue
			}
			merged.Add(k, v)
		}
	}
	return path + "?" + merged.Encode()
}

// splitPathAndQuery 把 "file:name?mode=memory" 拆成路径与已有查询参数。
func splitPathAndQuery(raw string) (string, url.Values) {
	path, query, found := strings.Cut(raw, "?")
	if !found {
		path = raw
	}
	// 用 file: URI 保证路径里的空格、中文、反斜杠都被正确处理；
	// 已经是 file: URI 或内存库时保持原样。
	if !strings.HasPrefix(path, "file:") && !isMemoryPath(path) {
		path = "file:" + filepath.ToSlash(path)
	}

	values := url.Values{}
	if found && query != "" {
		if parsed, err := url.ParseQuery(query); err == nil {
			values = parsed
		}
	}
	return path, values
}

// isMemoryPath 报告路径是否是内存数据库。
func isMemoryPath(path string) bool {
	return path == ":memory:" || strings.Contains(path, "mode=memory")
}

// applyPragmas 再次显式设置关键 PRAGMA。
//
// DSN 里已经带了这些设置，这里再设一次的原因：
// DSN 参数由驱动在建立每条连接时应用，行为随驱动版本可能变化；
// 显式执行一次可以让"数据库处于什么模式"这件事在代码里可见，
// 也便于测试直接断言。
func (s *Store) applyPragmas(ctx context.Context, cfg Config) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	}
	if !isMemoryPath(cfg.Path) {
		// WAL 对内存数据库无意义。
		pragmas = append(pragmas, "PRAGMA journal_mode = WAL")
	}

	for _, stmt := range pragmas {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply %q: %w", stmt, err)
		}
	}
	return nil
}

// Close 关闭数据库连接。
//
// WAL 模式下会把尚未合并的日志截断，避免留下 -wal / -shm 文件。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	// 尽力清理 WAL：失败不影响正确性（下次打开会继续恢复）。
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

// DB 暴露底层连接，供高级查询与后续阶段使用。
func (s *Store) DB() *sql.DB { return s.db }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// ---------------------------------------------------------------------------
// 迁移
// ---------------------------------------------------------------------------

// Migrate 应用所有尚未执行的迁移。
//
// 幂等：已应用的版本会被跳过。每个迁移在**单独事务**里执行，
// 因此中途失败不会留下半套表结构——下一个迁移不会在残缺的
// schema 上继续跑。
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			comment    TEXT NOT NULL DEFAULT '',
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersions 返回已应用的迁移版本集合。
func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// applyMigration 在事务里执行单个迁移。
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.Version, err)
	}
	defer func() {
		// 事务已提交时 Rollback 是无害的（返回 ErrTxDone），
		// 因此这里无条件调用，确保失败路径不会泄漏事务。
		_ = tx.Rollback()
	}()

	for i, stmt := range m.Statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d statement %d failed: %w", m.Version, i+1, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, comment, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Comment, time.Now().UTC().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.Version, err)
	}
	return nil
}

// SchemaVersion 返回当前数据库已应用的最大迁移版本。
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// ExpectedSchemaVersion 返回代码期望的数据库结构版本。
//
// 用于诊断"数据库比程序旧/新"的情况：数据库版本低于它说明
// 需要迁移（Migrate 会自动处理），高于它说明程序太旧。
func ExpectedSchemaVersion() int {
	max := 0
	for _, m := range migrations {
		if m.Version > max {
			max = m.Version
		}
	}
	return max
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

// Stats 是数据库的整体状态，用于 `db stats`。
type Stats struct {
	// Path 是数据库文件路径。
	Path string

	// SchemaVersion 是已应用的迁移版本。
	SchemaVersion int

	// ExpectedVersion 是代码期望的版本。
	ExpectedVersion int

	// SizeBytes 是数据库文件大小（含 WAL 时为三个文件之和）。
	SizeBytes int64

	// Tables 是各表的行数。
	Tables map[string]int64

	// FirstMeasurement 是最早一条测量的时间（零值表示没有数据）。
	FirstMeasurement time.Time

	// LastMeasurement 是最新一条测量的时间（零值表示没有数据）。
	LastMeasurement time.Time

	// DistinctTargets 是至少有一条测量的目标数。
	DistinctTargets int64

	// DistinctCollectors 是有测量的采集者数。
	DistinctCollectors int64
}

// CollectStats 汇总数据库状态。
func (s *Store) CollectStats(ctx context.Context) (Stats, error) {
	out := Stats{
		Path:            s.path,
		ExpectedVersion: ExpectedSchemaVersion(),
		Tables:          make(map[string]int64),
	}

	v, err := s.SchemaVersion(ctx)
	if err != nil {
		return out, err
	}
	out.SchemaVersion = v

	for _, table := range knownTables {
		var n int64
		// 表名来自本包常量，不是外部输入，因此直接拼接是安全的。
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			return out, fmt.Errorf("count %s: %w", table, err)
		}
		out.Tables[table] = n
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(measured_at), MAX(measured_at) FROM measurements`).Scan(
		&nullTime{target: &out.FirstMeasurement},
		&nullTime{target: &out.LastMeasurement},
	); err != nil {
		return out, fmt.Errorf("measurement time range: %w", err)
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT target_id) FROM measurements`).Scan(&out.DistinctTargets); err != nil {
		return out, fmt.Errorf("distinct targets: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT collector_id) FROM measurements`).Scan(&out.DistinctCollectors); err != nil {
		return out, fmt.Errorf("distinct collectors: %w", err)
	}

	out.SizeBytes = fileSize(s.path)
	return out, nil
}

// knownTables 是 CollectStats 会统计的表（顺序即输出顺序）。
var knownTables = []string{
	"targets",
	"collectors",
	"scan_sessions",
	"measurements",
	"traces",
}

// fileSize 返回数据库文件大小，尽量把 WAL / SHM 也算进去。
//
// 不返回 error：大小只是展示信息，取不到就报 0。
func fileSize(path string) int64 {
	if isMemoryPath(path) {
		return 0
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// nullTime 把 NULL 时间戳扫描成零值 time.Time。
//
// SQLite 里 MIN()/MAX() 在空表上返回 NULL，直接用 time.Time 扫描会报错。
type nullTime struct{ target *time.Time }

// Scan 实现 sql.Scanner。
func (n *nullTime) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*n.target = time.Time{}
		return nil
	case int64:
		*n.target = time.UnixMilli(v).UTC()
		return nil
	case time.Time:
		*n.target = v.UTC()
		return nil
	case []byte:
		ms, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return fmt.Errorf("scan time %q: %w", v, err)
		}
		*n.target = time.UnixMilli(ms).UTC()
		return nil
	case string:
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("scan time %q: %w", v, err)
		}
		*n.target = time.UnixMilli(ms).UTC()
		return nil
	default:
		return fmt.Errorf("scan time: unsupported type %T", value)
	}
}

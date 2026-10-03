package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
)

// newDBCommand 构造 `db` 命令组。
//
// 它本身不是叶子命令，而是 `db stats` / `db migrate` / `db vacuum`
// 的分发点。之所以用子命令而不是三个平级命令：
// 这三个操作都属于"维护本地数据库"，放在一起更容易被发现。
func newDBCommand() Command {
	return Command{
		Name:    "db",
		Summary: "本地 SQLite 数据库维护（stats / migrate / vacuum）",
		Usage:   ClientName + " db <stats|migrate|vacuum> [flags]",
		Run:     runDB,
	}
}

// dbParams 是 db 子命令共用的参数。
type dbParams struct {
	path    string
	timeout durationFlag
}

// runDB 分发 db 子命令。
func runDB(env *Env, args []string) error {
	if len(args) == 0 {
		return usageError("db requires a subcommand: stats | migrate | vacuum")
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "stats":
		return runDBStats(env, rest)
	case "migrate":
		return runDBMigrate(env, rest)
	case "vacuum":
		return runDBVacuum(env, rest)
	default:
		return usageError("unknown db subcommand: %s (want stats | migrate | vacuum)", sub)
	}
}

// dbFlagSet 构造 db 子命令的 FlagSet。
func dbFlagSet(name string, p *dbParams) *flag.FlagSet {
	fs := newFlagSet("db " + name)
	fs.StringVar(&p.path, "db", storage.DefaultPath, "SQLite 数据库路径")
	fs.Var(&p.timeout, "timeout", "整体操作超时（默认 60s）")
	return fs
}

// openStore 按参数打开数据库。
func openStore(ctx context.Context, p dbParams) (*storage.Store, error) {
	cfg := storage.DefaultConfig()
	cfg.Path = p.path
	if p.timeout.set {
		cfg.BusyTimeout = p.timeout.d
	}
	return storage.Open(ctx, cfg)
}

// dbContext 返回带超时与中断处理的 context。
//
// 数据库操作可能因为另一个进程持锁而等待，因此必须有超时，
// 否则用户看到的就是"命令卡住不动"。
func dbContext(p dbParams) (context.Context, context.CancelFunc) {
	timeout := 60 * time.Second
	if p.timeout.set {
		timeout = p.timeout.d
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	cancel := func() {
		stop()
	}
	timer := time.AfterFunc(timeout, stop)
	return ctx, func() {
		timer.Stop()
		cancel()
	}
}

// ---------------------------------------------------------------------------
// db stats
// ---------------------------------------------------------------------------

func runDBStats(env *Env, args []string) error {
	var p dbParams
	fs := dbFlagSet("stats", &p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := dbContext(p)
	defer cancel()

	store, err := openStore(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	stats, err := store.CollectStats(ctx)
	if err != nil {
		return err
	}

	printDBStats(env.Stdout, stats)
	return nil
}

// printDBStats 输出数据库状态。
func printDBStats(w io.Writer, s storage.Stats) {
	fmt.Fprintf(w, "database:       %s\n", s.Path)
	fmt.Fprintf(w, "size:           %s\n", humanBytes(s.SizeBytes))
	fmt.Fprintf(w, "schema_version: %d", s.SchemaVersion)
	if s.SchemaVersion != s.ExpectedVersion {
		fmt.Fprintf(w, "  (this build expects %d: run '%s db migrate')",
			s.ExpectedVersion, ClientName)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "\nrows:\n")
	for _, table := range []string{"targets", "collectors", "scan_sessions", "measurements", "traces"} {
		fmt.Fprintf(w, "  %-16s %d\n", table+":", s.Tables[table])
	}

	fmt.Fprintf(w, "\nmeasurement coverage:\n")
	fmt.Fprintf(w, "  targets with samples:    %d\n", s.DistinctTargets)
	fmt.Fprintf(w, "  collectors with samples: %d\n", s.DistinctCollectors)
	if s.FirstMeasurement.IsZero() {
		fmt.Fprintf(w, "  time range:              (no measurements yet)\n")
		return
	}
	fmt.Fprintf(w, "  first:  %s\n", formatTime(s.FirstMeasurement))
	fmt.Fprintf(w, "  last:   %s\n", formatTime(s.LastMeasurement))
	span := s.LastMeasurement.Sub(s.FirstMeasurement)
	fmt.Fprintf(w, "  span:   %s\n", span.Round(time.Second))
}

// humanBytes 把字节数格式化成人类可读形式。
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "0 B"
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------------
// db migrate
// ---------------------------------------------------------------------------

func runDBMigrate(env *Env, args []string) error {
	var p dbParams
	fs := dbFlagSet("migrate", &p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := dbContext(p)
	defer cancel()

	// Open 内部会执行迁移；这里再显式调用一次只是为了把
	// "已应用到哪个版本"报给用户。
	store, err := openStore(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(ctx); err != nil {
		return err
	}

	version, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "database:       %s\n", store.Path())
	fmt.Fprintf(env.Stdout, "schema_version: %d (up to date)\n", version)
	return nil
}

// ---------------------------------------------------------------------------
// db vacuum
// ---------------------------------------------------------------------------

func runDBVacuum(env *Env, args []string) error {
	var p dbParams
	fs := dbFlagSet("vacuum", &p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := dbContext(p)
	defer cancel()

	// 检查文件是否存在：对不存在的数据库执行 vacuum 会创建一个
	// 空库，那不是用户想要的。
	if _, err := os.Stat(p.path); err != nil {
		return fmt.Errorf("database %q does not exist: %w", p.path, err)
	}

	store, err := openStore(ctx, p)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// 先把 WAL 合并进主文件，再做碎片整理。
	// 这样"整理前"的大小是稳定值，而不是刚刚写入的一堆 WAL。
	if _, err := store.DB().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	before := mainFileSize(p.path)

	// VACUUM 会先把整理后的库写进临时空间，因此**过程中**文件会先变大，
	// 不能拿"刚执行完"的瞬时大小当成结果——那样报告出来的是
	// "变大"，会误导用户。所以这里再 checkpoint 一次并只统计主文件。
	if _, err := store.DB().ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum (磁盘空间不足时可能失败): %w", err)
	}
	if _, err := store.DB().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint wal after vacuum: %w", err)
	}
	after := mainFileSize(p.path)

	fmt.Fprintf(env.Stdout, "database: %s\n", p.path)
	fmt.Fprintf(env.Stdout, "size:     %s -> %s\n", humanBytes(before), humanBytes(after))
	if after < before {
		fmt.Fprintf(env.Stdout, "reclaimed: %s\n", humanBytes(before-after))
	} else {
		fmt.Fprintf(env.Stdout, "reclaimed: 0 B\n")
	}
	return nil
}

// mainFileSize 只返回主数据库文件的大小（不含 WAL / SHM）。
//
// vacuum 关心的是"主文件能缩到多小"；把 WAL 算进来会让结果
// 随"刚写了多少数据"波动，反而看不出整理效果。
// （WAL 的清理由 checkpoint 负责，日志里也会体现。）
func mainFileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// ---------------------------------------------------------------------------
// 采集者身份的辅助（probe --db 也使用）
// ---------------------------------------------------------------------------

// resolveIdentity 读取（必要时创建）本地匿名标识。
func resolveIdentity(path string, profileOverrides map[string]string) (*identity.Local, error) {
	local, err := identity.Load(path)
	if err != nil {
		return nil, err
	}

	// 命令行给出的画像覆盖本地文件中的值（只覆盖非空项）。
	merged := local.Profile
	if v := strings.TrimSpace(profileOverrides["country"]); v != "" {
		merged.Country = v
	}
	if v := strings.TrimSpace(profileOverrides["province"]); v != "" {
		merged.Province = v
	}
	if v := strings.TrimSpace(profileOverrides["city"]); v != "" {
		merged.City = v
	}
	if v := strings.TrimSpace(profileOverrides["isp"]); v != "" {
		merged.ISP = v
	}
	if v := strings.TrimSpace(profileOverrides["asn"]); v != "" {
		merged.ASN = v
	}
	if v := strings.TrimSpace(profileOverrides["ip-version"]); v != "" {
		merged.IPVersion = v
	}

	if merged != local.Profile {
		// 先归一化，再校验，最后落盘。
		//
		// 顺序很重要：用户手写 "--country cn" 是完全合理的输入，
		// 若先校验就会因为"必须是大写"而被拒绝——那是在为难用户，
		// 而不是在保护数据。归一化后再校验，非法值（例如 3 位
		// 国家代码）依然被挡在文件之外。
		profile := merged.ToModel()
		profile.Normalize()
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("%w: collector profile: %v", storage.ErrInvalidInput, err)
		}

		local.Profile = identity.FromModel(profile)
		if err := identity.Save(path, local); err != nil {
			return nil, err
		}
	}

	return local, nil
}

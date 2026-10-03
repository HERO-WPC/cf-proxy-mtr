package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/export"
	"github.com/cf-route-tester/cf-route-tester/internal/identity"
	"github.com/cf-route-tester/cf-route-tester/internal/storage"
	"github.com/cf-route-tester/cf-route-tester/internal/version"
)

// exportParams 是 `cf-route-tester export` 的参数。
type exportParams struct {
	db           string
	identityPath string

	// kind 决定导出测量、跟踪还是两者。
	kind string

	// session 为空时导出该采集者的全部数据（跨会话）。
	session string

	// out 为空时按 SuggestFilename 生成到当前目录；"-" 表示标准输出。
	out string

	format string

	since string
	until string

	limit int

	// listSessions 只列出会话供选择，不导出。
	listSessions bool

	jsonOut bool
	quiet   bool

	// dryRun 只统计将要导出多少行与过滤掉多少，不写文件。
	dryRun bool
}

// 导出种类。
const (
	kindMeasurements = "measurements"
	kindTraces       = "traces"
	kindAll          = "all"
)

// newExportCommand 构造 export 子命令。
func newExportCommand() Command {
	return Command{
		Name:    "export",
		Summary: "把本地数据导出为可公开的 JSONL（自动应用隐私过滤）",
		Usage:   ClientName + " export [flags]",
		Run:     runExport,
		Flags: func(w io.Writer) {
			var p exportParams
			fs := exportFlagSet(&p)
			fs.SetOutput(w)
			fs.PrintDefaults()

			fmt.Fprintf(w, "\nFormats:\n")
			for _, format := range export.AllFormats() {
				status := "available"
				if reason := format.UnavailableReason(); reason != "" {
					status = "NOT available"
				}
				fmt.Fprintf(w, "  %-10s %s\n", format, status)
			}
			fmt.Fprintf(w, "\n  zstd 暂不可用：Go 1.26 标准库未公开 zstd 编码器，\n")
			fmt.Fprintf(w, "  引入第三方实现会明显增加二进制体积，因此先用 gzip。\n")

			fmt.Fprintf(w, "\nPrivacy:\n")
			fmt.Fprintf(w, "  导出物是**公开数据**，因此这里会自动做隐私过滤：\n")
			fmt.Fprintf(w, "    - 目标是内网 / 保留地址的行被丢弃（那是本机测试数据）；\n")
			fmt.Fprintf(w, "    - 路径上的内网地址被替换成 %s / %s（保留跳的位置）；\n",
				"private-v4", "private-v6")
			fmt.Fprintf(w, "    - 错误信息里的 IP、文件路径、用户名被替换成占位符。\n")
			fmt.Fprintf(w, "  过滤结果会在汇总里如实报出，绝不静默丢弃。\n")

			fmt.Fprintf(w, "\nExamples:\n")
			fmt.Fprintf(w, "  %s export --list-sessions                 # 先看有哪些会话\n", ClientName)
			fmt.Fprintf(w, "  %s export --session <id> --dry-run        # 看会导出多少行\n", ClientName)
			fmt.Fprintf(w, "  %s export --session <id> --out out.jsonl.gz\n", ClientName)
			fmt.Fprintf(w, "  %s export --format jsonl --out - > all.jsonl\n", ClientName)
		},
	}
}

// exportFlagSet 构造 export 的 FlagSet。
func exportFlagSet(p *exportParams) *flag.FlagSet {
	fs := newFlagSet("export")

	fs.StringVar(&p.db, "db", "", "本地数据库路径（必填）")
	fs.StringVar(&p.identityPath, "identity", identity.DefaultPath, "本地匿名标识文件路径")

	fs.StringVar(&p.kind, "kind", kindAll, "导出内容：measurements / traces / all")
	fs.StringVar(&p.session, "session", "", "只导出指定会话（默认导出该采集者的全部数据）")
	fs.StringVar(&p.format, "format", string(export.FormatJSONLGz),
		"输出格式："+export.JoinFormats(export.SupportedFormats()))
	fs.StringVar(&p.out, "out", "", "输出路径（默认自动命名；'-' 表示标准输出）")

	fs.StringVar(&p.since, "since", "", "起始时间（RFC3339 或 2006-01-02）")
	fs.StringVar(&p.until, "until", "", "截止时间（RFC3339 或 2006-01-02）")
	fs.IntVar(&p.limit, "limit", 0, "最多导出每种数据的行数（0 表示不限）")

	fs.BoolVar(&p.listSessions, "list-sessions", false, "只列出数据库里的会话，不导出")
	fs.BoolVar(&p.dryRun, "dry-run", false, "只统计将导出与过滤的行数，不写文件")
	fs.BoolVar(&p.jsonOut, "json", false, "以 JSON 输出汇总统计")
	fs.BoolVar(&p.quiet, "quiet", false, "只输出最终结果")

	return fs
}

// runExport 实现 `cf-route-tester export`。
func runExport(env *Env, args []string) error {
	var p exportParams
	fs := exportFlagSet(&p)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if strings.TrimSpace(p.db) == "" {
		return usageError("--db is required: export reads from the local database")
	}
	if p.limit < 0 {
		return usageError("--limit must not be negative")
	}

	kind, err := normalizeExportKind(p.kind)
	if err != nil {
		return usageError("%v", err)
	}
	format, err := export.NormalizeFormat(p.format)
	if err != nil {
		return usageError("%v", err)
	}
	// 格式不可用时给出原因，而不是笼统的"不支持"。
	if reason := format.UnavailableReason(); reason != "" {
		return usageError("%s", reason)
	}

	since, err := parseExportTime(p.since)
	if err != nil {
		return usageError("--since: %v", err)
	}
	// --until 用"结束时刻"语义：纯日期扩展成当天最后一刻，
	// 否则 "--since 2026-10-03 --until 2026-10-03" 会得到空结果。
	until, err := parseExportTimeEnd(p.until)
	if err != nil {
		return usageError("--until: %v", err)
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return usageError("--until (%s) is before --since (%s)", until, since)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := storage.Open(ctx, storage.Config{Path: p.db})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintln(env.Stderr, "Error: closing database: "+cerr.Error())
		}
	}()

	// 采集者：导出物里必须带匿名 ID，否则数据无法归属到节点。
	local, err := identity.Load(p.identityPath)
	if err != nil {
		return err
	}
	collectorPK, err := store.UpsertCollector(ctx, local.CollectorID, local.Profile.ToModel(), version.Version)
	if err != nil {
		return err
	}

	// --list-sessions：先让用户看到可选项。
	if p.listSessions {
		return printExportSessions(env, ctx, store, collectorPK, p)
	}

	// 落库前先把会话定下来：导出报告里要显示它。
	sessionID := strings.TrimSpace(p.session)

	filter := storage.ExportFilter{
		CollectorPK: collectorPK,
		SessionID:   sessionID,
		Since:       since,
		Until:       until,
		Limit:       p.limit,
	}

	// --dry-run：只统计，不写文件。
	if p.dryRun {
		stats, err := collectExportStats(ctx, store, filter, kind)
		if err != nil {
			return err
		}
		return reportExport(env, stats, exportReport{
			SessionID: sessionID,
			Kind:      kind,
			Format:    format,
			Database:  p.db,
			DryRun:    true,
			NoData:    stats.RowsExported() == 0,
		})
	}

	// 决定输出目标。
	writer, closeOutput, outPath, err := openExportOutput(env, format, p.out, sessionID, kind)
	if err != nil {
		return err
	}

	encoder, err := export.NewEncoder(format, writer)
	if err != nil {
		_ = closeOutput()
		return err
	}

	stats, writeErr := writeExport(ctx, encoder, store, filter, kind, env, p)

	// 收尾顺序：先关编码器（刷出 gzip 尾部与缓冲），再关文件。
	encodeErr := encoder.Close()
	closeErr := closeOutput()

	if writeErr != nil {
		return writeErr
	}
	if encodeErr != nil {
		return encodeErr
	}
	if closeErr != nil {
		return closeErr
	}

	report := exportReport{
		SessionID: sessionID,
		Kind:      kind,
		Format:    format,
		Output:    outPath,
		Database:  p.db,
		NoData:    stats.RowsExported() == 0,
	}
	if outPath != "-" {
		if info, err := os.Stat(outPath); err == nil {
			report.Bytes = info.Size()
		}
	}
	return reportExport(env, stats, report)
}

// normalizeExportKind 校验导出种类。
func normalizeExportKind(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", kindAll, "both":
		return kindAll, nil
	case kindMeasurements, "measurement", "probe":
		return kindMeasurements, nil
	case kindTraces, "trace":
		return kindTraces, nil
	default:
		return "", fmt.Errorf("unknown --kind %q (want %s, %s or %s)",
			raw, kindMeasurements, kindTraces, kindAll)
	}
}

// parseExportTime 解析时间参数，允许只写日期。
//
// 只写日期（"2026-10-03"）时：
//   - --since 取当天 00:00:00 UTC；
//   - --until 取当天 23:59:59.999999999 UTC。
//
// 这个区别很重要：如果 --until 也取 00:00:00，用户写
// "--since 2026-10-03 --until 2026-10-03" 会得到空结果，
// 而那显然不是他想表达的"就这一天"。
func parseExportTime(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}

	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %q (want RFC3339 like 2026-10-03T12:00:00Z, or a date like 2026-10-03)", raw)
}

// parseExportTimeEnd 解析 --until，把纯日期扩展成当天结束。
func parseExportTimeEnd(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}
	// 纯日期（10 个字符）扩展成当天最后一刻。
	if len(trimmed) == len("2006-01-02") {
		if parsed, err := time.Parse("2006-01-02", trimmed); err == nil {
			return parsed.Add(24*time.Hour - time.Nanosecond).UTC(), nil
		}
	}
	return parseExportTime(trimmed)
}

// openExportOutput 准备输出目标。
func openExportOutput(env *Env, format export.Format, out, sessionID, kind string) (io.Writer, func() error, string, error) {
	if out == "-" {
		// 标准输出：不关闭，也不报"文件大小"。
		return env.Stdout, func() error { return nil }, "-", nil
	}

	path := strings.TrimSpace(out)
	if path == "" {
		// 自动命名：放到当前目录，名字里带会话 ID。
		name := export.SuggestFilename(format, sessionID, kind)
		path = name
	}

	// 已存在时**不静默覆盖**：导出物是用户可能已经上传过的东西，
	// 悄悄覆盖会让人分不清"哪一份被上传了"。
	if _, err := os.Stat(path); err == nil {
		return nil, nil, "", fmt.Errorf(
			"output file %q already exists; remove it or choose another path with --out", path)
	} else if !os.IsNotExist(err) {
		return nil, nil, "", fmt.Errorf("check output path %q: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, "", fmt.Errorf("create output directory %q: %w", dir, err)
		}
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create output file %q: %w", path, err)
	}

	closeFn := func() error {
		if err := file.Close(); err != nil {
			return fmt.Errorf("close output file %q: %w", path, err)
		}
		return nil
	}
	return file, closeFn, path, nil
}

// writeExport 执行导出，返回过滤统计。
func writeExport(
	ctx context.Context,
	encoder *export.Encoder,
	store *storage.Store,
	filter storage.ExportFilter,
	kind string,
	env *Env,
	p exportParams,
) (export.FilterStats, error) {
	var stats export.FilterStats

	// 进度：整库导出可能跑很久，没有反馈会让人以为卡死。
	progress := newExportProgress(env.Stderr, p.quiet)

	if kind == kindMeasurements || kind == kindAll {
		written, err := streamMeasurementRows(ctx, encoder, store, filter, &stats)
		if err != nil {
			return stats, err
		}
		progress.step("measurements", written, stats.RowsSkipped())
	}

	if kind == kindTraces || kind == kindAll {
		written, err := streamTraceRows(ctx, encoder, store, filter, &stats)
		if err != nil {
			return stats, err
		}
		progress.step("traces", written, stats.RowsSkipped())
	}

	return stats, nil
}

// exportProgress 输出导出进度。
//
// 一阶段一行的形式（而不是 \r 覆盖）：导出只有两三个阶段，
// 每个阶段的最终数字都有意义，覆盖掉反而看不到。
type exportProgress struct {
	w     io.Writer
	quiet bool
}

// newExportProgress 创建进度输出。
func newExportProgress(w io.Writer, quiet bool) *exportProgress {
	return &exportProgress{w: w, quiet: quiet}
}

// step 报告一个阶段完成。
func (p *exportProgress) step(label string, written, skipped int) {
	if p == nil || p.quiet {
		return
	}
	if skipped > 0 {
		fmt.Fprintf(p.w, "%s: %d exported, %d skipped by privacy filter\n", label, written, skipped)
		return
	}
	fmt.Fprintf(p.w, "%s: %d exported\n", label, written)
}

// streamMeasurementRows 流式转换并写出测量。
func streamMeasurementRows(
	ctx context.Context,
	encoder *export.Encoder,
	store *storage.Store,
	filter storage.ExportFilter,
	stats *export.FilterStats,
) (int, error) {
	written := 0
	_, err := store.StreamMeasurements(ctx, filter, func(item storage.ExportMeasurement) error {
		row, rowStats, ok := export.MeasurementRow(item, version.Version)
		stats.Add(rowStats)
		if !ok {
			return nil
		}
		if err := encoder.Writer.Write(row); err != nil {
			return err
		}
		written++
		return nil
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// streamTraceRows 流式转换并写出跟踪。
func streamTraceRows(
	ctx context.Context,
	encoder *export.Encoder,
	store *storage.Store,
	filter storage.ExportFilter,
	stats *export.FilterStats,
) (int, error) {
	written := 0
	_, err := store.StreamTraces(ctx, filter, func(item storage.ExportTrace) error {
		row, rowStats, ok := export.TraceRow(item, version.Version)
		stats.Add(rowStats)
		if !ok {
			return nil
		}
		if err := encoder.Writer.Write(row); err != nil {
			return err
		}
		written++
		return nil
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// collectExportStats 只统计而不写出（--dry-run）。
//
// 复用同一套转换逻辑：如果 dry-run 用另一套计数，它给出的数字
// 就没有意义了（"报告 100 行，实际导出 80 行"）。
func collectExportStats(ctx context.Context, store *storage.Store, filter storage.ExportFilter, kind string) (export.FilterStats, error) {
	var stats export.FilterStats
	sink := &discardWriter{}

	encoder, err := export.NewEncoder(export.FormatJSONL, sink)
	if err != nil {
		return stats, err
	}
	if kind == kindMeasurements || kind == kindAll {
		if _, err := streamMeasurementRows(ctx, encoder, store, filter, &stats); err != nil {
			return stats, err
		}
	}
	if kind == kindTraces || kind == kindAll {
		if _, err := streamTraceRows(ctx, encoder, store, filter, &stats); err != nil {
			return stats, err
		}
	}
	return stats, encoder.Close()
}

// discardWriter 丢弃所有写入。
type discardWriter struct{}

// Write 实现 io.Writer。
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

// exportReport 是导出汇总里与统计无关的部分。
type exportReport struct {
	SessionID string
	Kind      string
	Format    export.Format
	Output    string
	Database  string
	DryRun    bool
	NoData    bool
	Bytes     int64
}

// reportExport 输出导出汇总。
func reportExport(env *Env, stats export.FilterStats, report exportReport) error {
	if report.DryRun || report.Output == "-" {
		// 标准输出被数据占用时，汇总只能进 stderr。
	}

	w := env.Stdout
	if report.Output == "-" {
		w = env.Stderr
	}

	if report.SessionID != "" {
		fmt.Fprintf(w, "session:     %s\n", report.SessionID)
	} else {
		fmt.Fprintf(w, "session:     (all sessions of this collector)\n")
	}
	fmt.Fprintf(w, "kind:        %s\n", report.Kind)
	fmt.Fprintf(w, "database:    %s\n", report.Database)

	if report.DryRun {
		fmt.Fprintf(w, "mode:        dry run (nothing written)\n")
	} else {
		fmt.Fprintf(w, "output:      %s\n", report.Output)
		fmt.Fprintf(w, "format:      %s\n", report.Format)
	}

	fmt.Fprintf(w, "\nrows in scope:\n")
	if report.Kind == kindMeasurements || report.Kind == kindAll {
		fmt.Fprintf(w, "  measurements: %d read, %d exported\n",
			stats.MeasurementsTotal, stats.MeasurementsExported)
	}
	if report.Kind == kindTraces || report.Kind == kindAll {
		fmt.Fprintf(w, "  traces:       %d read, %d exported\n",
			stats.TracesTotal, stats.TracesExported)
	}
	fmt.Fprintf(w, "  total exported: %d\n", stats.RowsExported())

	if report.Bytes > 0 {
		fmt.Fprintf(w, "  output size:    %s\n", humanBytes(report.Bytes))
	} else if !report.DryRun && report.Output == "-" {
		fmt.Fprintf(w, "  output size:    (streamed to stdout)\n")
	}

	// 隐私过滤必须被明确报出：静默丢弃数据的导出是不可信的。
	fmt.Fprintf(w, "\nprivacy filtering:\n")
	fmt.Fprintf(w, "  %s\n", stats.PrivacyNote())
	if stats.SanitizedMessages > 0 {
		fmt.Fprintf(w, "  %d error message(s) had IPs/paths/usernames replaced\n", stats.SanitizedMessages)
	}

	if report.NoData {
		fmt.Fprintf(w, "\nwarning: nothing to export for this selection.\n")
		fmt.Fprintf(w, "         Check the session id with '%s export --list-sessions',\n", ClientName)
		fmt.Fprintf(w, "         or drop --session to export everything for this collector.\n")
	}

	return nil
}

// printExportSessions 列出会话供用户选择。
func printExportSessions(env *Env, ctx context.Context, store *storage.Store, collectorPK int64, p exportParams) error {
	sessions, err := store.ListSessions(ctx, collectorPK)
	if err != nil {
		return err
	}

	if p.jsonOut {
		return writeSessionsJSON(env.Stdout, sessions)
	}

	if len(sessions) == 0 {
		fmt.Fprintf(env.Stdout, "no sessions found for this collector in %s\n", p.db)
		fmt.Fprintf(env.Stdout, "run '%s scan' first to create one.\n", ClientName)
		return nil
	}

	fmt.Fprintf(env.Stdout, "sessions in %s:\n\n", p.db)
	fmt.Fprintf(env.Stdout, "  %-28s %-20s %8s %8s %s\n",
		"SESSION", "STARTED (UTC)", "MEASURED", "TRACED", "STATE")
	for _, session := range sessions {
		state := "open"
		if session.Finished() {
			state = "finished"
		}
		fmt.Fprintf(env.Stdout, "  %-28s %-20s %8d %8d %s\n",
			session.ID,
			session.StartedAt.Format("2006-01-02 15:04:05"),
			session.MeasuredTargets,
			session.TracedTargets,
			state)
	}

	fmt.Fprintf(env.Stdout, "\nnext: %s export --session <id> --out <file>\n", ClientName)
	return nil
}

// writeSessionsJSON 以 JSON 输出会话列表。
func writeSessionsJSON(w io.Writer, sessions []storage.SessionSummary) error {
	type sessionJSON struct {
		ID              string `json:"id"`
		StartedAt       string `json:"started_at"`
		FinishedAt      string `json:"finished_at,omitempty"`
		Finished        bool   `json:"finished"`
		TargetCount     int    `json:"target_count"`
		CompletedCount  int    `json:"completed_count"`
		MeasuredTargets int    `json:"measured_targets"`
		TracedTargets   int    `json:"traced_targets"`
		CollectorID     string `json:"collector_id"`
	}

	out := make([]sessionJSON, 0, len(sessions))
	for _, session := range sessions {
		entry := sessionJSON{
			ID:              session.ID,
			StartedAt:       session.StartedAt.Format(time.RFC3339),
			Finished:        session.Finished(),
			TargetCount:     session.TargetCount,
			CompletedCount:  session.CompletedCount,
			MeasuredTargets: session.MeasuredTargets,
			TracedTargets:   session.TracedTargets,
			CollectorID:     session.CollectorAID,
		}
		if session.Finished() {
			entry.FinishedAt = session.FinishedAt.Format(time.RFC3339)
		}
		out = append(out, entry)
	}

	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(blob))
	return err
}

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SaveMeasurements 批量写入测量结果。
//
// 语义（需求第 39、40 条）：
//
//   - **只追加**，绝不 UPDATE 既有行。同一个目标在不同时间的测量
//     必须同时保留，否则"什么时候变差"就无法回答；
//   - 失败结果同样写入：失败也是线路信息；
//   - 重复写入是幂等的：dedup_key 上的 UNIQUE 约束会让第二次导入
//     变成空操作，因此"同一个 batch 被上传/导入两次"不会让统计翻倍。
//
// 返回实际写入的行数与因重复而跳过的行数。
//
// 整个批次在一个事务里：要么全部写入，要么全不写入。
// 这样"写到一半被 Ctrl+C"不会留下半批数据，
// 断点续测只需重新提交这一批。
func (s *Store) SaveMeasurements(ctx context.Context, measurements []Measurement) (saved int, skipped int, err error) {
	if len(measurements) == 0 {
		return 0, 0, nil
	}

	// 先整体校验：把错误定位到具体目标，而不是让 SQL 报一个
	// 与数据无关的约束错误。
	for i, m := range measurements {
		if err := m.Validate(); err != nil {
			return 0, 0, fmt.Errorf("measurement #%d: %w", i, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin measurement insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO measurements (
			target_id, collector_id, session_id,
			measured_at, success, latency_us, error_type, error_message,
			schema_version, client_version, dedup_key, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(dedup_key) DO NOTHING
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("prepare measurement insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	createdAt := time.Now().UTC().UnixMilli()

	for i, m := range measurements {
		res, err := stmt.ExecContext(ctx,
			m.TargetID, m.CollectorID, nullableString(m.SessionID),
			m.Timestamp.UTC().UnixMilli(), boolToInt(m.Success), m.LatencyUS(),
			string(m.ErrorType), m.ErrorMessage,
			normalizeSchemaVersion(m.SchemaVersion), orDefault(m.ClientVersion, clientVersion),
			measurementDedupKey(m), createdAt,
		)
		if err != nil {
			// 外键失败通常意味着"目标还没入库"或"采集者不存在"，
			// 这类错误必须报出来，否则会出现"测量写了但目标不存在"
			// 这种后续无法分析的数据。
			if isForeignKeyError(err) {
				return 0, 0, fmt.Errorf("measurement #%d for target %q: %w (目标或采集者尚未入库)",
					i, m.TargetID, err)
			}
			return 0, 0, fmt.Errorf("insert measurement #%d for %q: %w", i, m.TargetID, err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			// 驱动不支持 RowsAffected 时保守认为写入了。
			saved++
			continue
		}
		if n == 0 {
			skipped++
			continue
		}
		saved++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit measurement insert: %w", err)
	}
	return saved, skipped, nil
}

// SaveTraces 批量写入线路跟踪结果。
//
// 与 SaveMeasurements 完全同构：只追加、幂等、整批一个事务。
// Phase 4 只建立通道；真正的调用来自 Phase 7。
func (s *Store) SaveTraces(ctx context.Context, traces []Trace) (saved int, skipped int, err error) {
	if len(traces) == 0 {
		return 0, 0, nil
	}

	for i, t := range traces {
		if err := t.Validate(); err != nil {
			return 0, 0, fmt.Errorf("trace #%d: %w", i, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin trace insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO traces (
			target_id, collector_id, session_id,
			traced_at, engine, engine_version, mode, protocol, port,
			success, duration_us, hop_count, trace_json, raw_json,
			error_type, error_message, local_filtered,
			schema_version, client_version, dedup_key, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(dedup_key) DO NOTHING
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("prepare trace insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	createdAt := time.Now().UTC().UnixMilli()

	for i, t := range traces {
		res, err := stmt.ExecContext(ctx,
			t.TargetID, t.CollectorID, nullableString(t.SessionID),
			t.Timestamp.UTC().UnixMilli(), t.Engine, t.EngineVersion, t.Mode, t.Protocol, t.Port,
			boolToInt(t.Success), t.DurationUS(), t.HopCount,
			t.TraceJSON, t.RawJSON, t.ErrorType, t.ErrorMessage, boolToInt(t.LocalFiltered),
			normalizeSchemaVersion(t.SchemaVersion), orDefault(t.ClientVersion, clientVersion),
			traceDedupKey(t), createdAt,
		)
		if err != nil {
			if isForeignKeyError(err) {
				return 0, 0, fmt.Errorf("trace #%d for target %q: %w (目标或采集者尚未入库)",
					i, t.TargetID, err)
			}
			return 0, 0, fmt.Errorf("insert trace #%d for %q: %w", i, t.TargetID, err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			saved++
			continue
		}
		if n == 0 {
			skipped++
			continue
		}
		saved++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit trace insert: %w", err)
	}
	return saved, skipped, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// boolToInt 把 bool 存成 0/1（SQLite 没有布尔类型）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullableString 把空字符串转成 NULL。
//
// 用于 session_id：它可为空（不属于任何会话的临时测量），
// 而外键列上的空字符串会违反约束，必须写 NULL。
func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// orDefault 在值为空时返回兜底值。
func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// normalizeSchemaVersion 保证 schema_version 至少为 1。
//
// 0 会被解读成"未知版本"，而未知版本在分析时无法区分字段含义，
// 因此宁可落成当前版本（调用方若真的不知道版本，那也是本程序产生的）。
func normalizeSchemaVersion(v int) int {
	if v <= 0 {
		return schemaVersionForMeasurements
	}
	return v
}

// isForeignKeyError 判断错误是否是外键约束失败。
//
// 不依赖具体的错误码：modernc.org/sqlite 把 SQLite 的错误文本
// 原样带出来，因此匹配文本是跨驱动版本最稳的做法。
func isForeignKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "FOREIGN KEY") || strings.Contains(msg, "CONSTRAINT FAILED")
}

// ErrNoRows 是 ErrNotFound 在计数场景下的别名，便于调用方书写。
var ErrNoRows = sql.ErrNoRows

// CountMeasurements 返回 measurements 表的行数。
func (s *Store) CountMeasurements(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM measurements`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count measurements: %w", err)
	}
	return n, nil
}

// CountTraces 返回 traces 表的行数。
func (s *Store) CountTraces(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traces`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count traces: %w", err)
	}
	return n, nil
}

// errors 包的引用保留：Validate 里用 errors.New 构造错误。
var _ = errors.New

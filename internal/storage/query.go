package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
)

// MeasurementView 是从数据库读回的一条测量。
type MeasurementView struct {
	ID          int64
	TargetID    string
	CollectorPK int64
	SessionID   string

	Timestamp time.Time

	Success      bool
	LatencyMS    float64
	ErrorType    probe.ErrorType
	ErrorMessage string

	SchemaVersion int
	ClientVersion string
}

// TraceView 是从数据库读回的一条 trace（不含完整 JSON，避免误用）。
type TraceView struct {
	ID            int64
	TargetID      string
	CollectorPK   int64
	SessionID     string
	Timestamp     time.Time
	Engine        string
	EngineVersion string
	Mode          string
	Protocol      string
	Port          int
	Success       bool
	DurationMS    float64
	HopCount      int
	ErrorType     string
	ErrorMessage  string
	LocalFiltered bool
}

// CollectorView 是从数据库读回的采集者。
type CollectorView struct {
	PK            int64
	CollectorID   string
	Country       string
	Province      string
	City          string
	ISP           string
	ASN           string
	IPVersion     string
	ClientVersion string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// MeasurementQuery 是测量查询的过滤条件。
//
// 零值表示"不过滤"。这是给后续阶段（导出、聚合、查询）准备的
// 基础能力；Phase 4 只需要它足够表达"取某目标某时间段的样本"。
type MeasurementQuery struct {
	// TargetID 精确匹配目标。
	TargetID string

	// CollectorPK 精确匹配采集者。
	CollectorPK int64

	// SessionID 精确匹配会话。
	SessionID string

	// Since / Until 是时间窗口（闭区间）；零值表示不限。
	Since time.Time
	Until time.Time

	// SuccessOnly / FailedOnly 只取成功或只取失败。
	// 两个同时为真时返回错误，避免调用方拿到空结果却不知道原因。
	SuccessOnly bool
	FailedOnly  bool

	// Limit 限制返回行数（<=0 表示不限）。
	Limit int

	// OrderDesc 为真时按时间倒序（默认正序）。
	OrderDesc bool
}

// QueryMeasurements 按条件读取测量记录。
//
// 注意：这是**读取**接口，不会修改任何数据。
// 数据库里的测量是时间序列，因此这里默认按时间正序返回，
// 便于调用方直接观察"延迟随时间如何变化"。
func (s *Store) QueryMeasurements(ctx context.Context, q MeasurementQuery) ([]MeasurementView, error) {
	if q.SuccessOnly && q.FailedOnly {
		return nil, fmt.Errorf("%w: SuccessOnly and FailedOnly are mutually exclusive", ErrInvalidInput)
	}

	where, args := q.buildWhere()

	order := "ASC"
	if q.OrderDesc {
		order = "DESC"
	}

	sqlText := `
		SELECT id, target_id, collector_id, session_id,
		       measured_at, success, latency_us, error_type, error_message,
		       schema_version, client_version
		FROM measurements` + where + ` ORDER BY measured_at ` + order

	if q.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query measurements: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MeasurementView
	for rows.Next() {
		var (
			view       MeasurementView
			sessionID  sql.NullString
			measuredAt int64
			success    int
			latencyUS  int64
			errType    string
		)
		if err := rows.Scan(
			&view.ID, &view.TargetID, &view.CollectorPK, &sessionID,
			&measuredAt, &success, &latencyUS, &errType, &view.ErrorMessage,
			&view.SchemaVersion, &view.ClientVersion,
		); err != nil {
			return nil, fmt.Errorf("scan measurement: %w", err)
		}

		if sessionID.Valid {
			view.SessionID = sessionID.String
		}
		view.Timestamp = time.UnixMilli(measuredAt).UTC()
		view.Success = success == 1
		view.LatencyMS = microsecondsToMilliseconds(latencyUS)
		view.ErrorType = probe.ErrorType(errType)

		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate measurements: %w", err)
	}
	return out, nil
}

// CountMeasurementsByQuery 返回符合条件的数据条数。
//
// 统计类查询（成功率、样本数）需要它，而且比把全部行读回来再数
// 要省内存。
func (s *Store) CountMeasurementsByQuery(ctx context.Context, q MeasurementQuery) (int64, error) {
	where, args := q.buildWhere()
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM measurements`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count measurements: %w", err)
	}
	return n, nil
}

// buildWhere 把查询条件编译成 SQL 片段与参数。
//
// 参数一律通过占位符传递，字符串不拼接进 SQL——
// 即使将来 TargetID 来自命令行输入，也不会形成注入。
func (q MeasurementQuery) buildWhere() (string, []any) {
	var (
		clauses []string
		args    []any
	)

	if q.TargetID != "" {
		clauses = append(clauses, "target_id = ?")
		args = append(args, q.TargetID)
	}
	if q.CollectorPK > 0 {
		clauses = append(clauses, "collector_id = ?")
		args = append(args, q.CollectorPK)
	}
	if q.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if !q.Since.IsZero() {
		clauses = append(clauses, "measured_at >= ?")
		args = append(args, q.Since.UTC().UnixMilli())
	}
	if !q.Until.IsZero() {
		clauses = append(clauses, "measured_at <= ?")
		args = append(args, q.Until.UTC().UnixMilli())
	}
	if q.SuccessOnly {
		clauses = append(clauses, "success = 1")
	}
	if q.FailedOnly {
		clauses = append(clauses, "success = 0")
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + joinClauses(clauses), args
}

// joinClauses 用 " AND " 连接条件。
func joinClauses(clauses []string) string {
	out := ""
	for i, c := range clauses {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}

// QueryTraces 按条件读取 trace 摘要（不含 trace_json / raw_json）。
func (s *Store) QueryTraces(ctx context.Context, targetID string, limit int) ([]TraceView, error) {
	sqlText := `
		SELECT id, target_id, collector_id, session_id, traced_at,
		       engine, engine_version, mode, protocol, port,
		       success, duration_us, hop_count, error_type, error_message, local_filtered
		FROM traces`
	var (
		where string
		args  []any
	)
	if targetID != "" {
		where = " WHERE target_id = ?"
		args = append(args, targetID)
	}
	sqlText += where + " ORDER BY traced_at DESC"
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TraceView
	for rows.Next() {
		var (
			view       TraceView
			sessionID  sql.NullString
			tracedAt   int64
			success    int
			durationUS int64
			filtered   int
		)
		if err := rows.Scan(
			&view.ID, &view.TargetID, &view.CollectorPK, &sessionID, &tracedAt,
			&view.Engine, &view.EngineVersion, &view.Mode, &view.Protocol, &view.Port,
			&success, &durationUS, &view.HopCount, &view.ErrorType, &view.ErrorMessage, &filtered,
		); err != nil {
			return nil, fmt.Errorf("scan trace: %w", err)
		}

		if sessionID.Valid {
			view.SessionID = sessionID.String
		}
		view.Timestamp = time.UnixMilli(tracedAt).UTC()
		view.Success = success == 1
		view.DurationMS = microsecondsToMilliseconds(durationUS)
		view.LocalFiltered = filtered == 1

		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traces: %w", err)
	}
	return out, nil
}

// LoadTraces 读取指定目标的 trace 记录，并附带完整的 trace_json。
//
// 故意与 QueryTraces 分开：完整 JSON 可能很大，
// 只有确实需要路径数据的调用方（例如后续的路径分析）才应读取它。
func (s *Store) LoadTraceJSON(ctx context.Context, traceID int64) (string, string, error) {
	var traceJSON, rawJSON sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT trace_json, raw_json FROM traces WHERE id = ?`, traceID).Scan(&traceJSON, &rawJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("%w: trace %d", ErrNotFound, traceID)
	case err != nil:
		return "", "", fmt.Errorf("load trace %d: %w", traceID, err)
	}
	return traceJSON.String, rawJSON.String, nil
}

// LoadCollectors 读取全部采集者。
func (s *Store) LoadCollectors(ctx context.Context) ([]CollectorView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, collector_id, country, province, city, isp, asn, ip_version,
		       client_version, created_at, updated_at
		FROM collectors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query collectors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CollectorView
	for rows.Next() {
		var (
			view      CollectorView
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(
			&view.PK, &view.CollectorID, &view.Country, &view.Province, &view.City,
			&view.ISP, &view.ASN, &view.IPVersion, &view.ClientVersion,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan collector: %w", err)
		}
		view.CreatedAt = time.UnixMilli(createdAt).UTC()
		view.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collectors: %w", err)
	}
	return out, nil
}

// GroupProfile 返回采集者的分组画像（用于聚合分组）。
func (v CollectorView) GroupProfile() string {
	return v.Country + "/" + v.Province + "/" + v.City + "/" + v.ISP + "/" + v.ASN
}

// PendingTargets 返回在指定 (采集者, 会话) 下**还没有测量结果**的目标。
//
// 这是断点续测的判据（需求第 20 条），必须是三元组而不是"数据库里有没有
// 这个目标的历史结果"——否则同一个采集者永远无法重新测量全部目标，
// 而"每天重测一次"正是项目的数据来源。
//
// 判定依据是 measurements 里是否存在 (target_id, collector_id, session_id)
// 的行，与 success / 失败分类无关：**测过就是测过**，
// 包括"超时""连接被拒"这些失败结果。失败也是线路信息，
// 重跑时不该被当成"还没测"。
//
// targets 为空时返回空切片（而不是报错）：调用方可能只是没有目标。
func (s *Store) PendingTargets(ctx context.Context, targets []model.Target, collectorPK int64, sessionID string) ([]model.Target, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	if collectorPK <= 0 {
		return nil, fmt.Errorf("%w: pending query needs a collector", ErrInvalidInput)
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("%w: pending query needs a session id", ErrInvalidInput)
	}

	// 一次性把本会话已完成的目标读进内存，再与给定列表比对。
	//
	// 为什么不用 "WHERE id NOT IN (子查询)" 直接查 targets：
	// 目标列表来自 all.json，可能包含数据库里还没有的新目标；
	// 而且我们需要保持**源顺序**，用 SQL 排序反而会丢掉它。
	// 逐个 EXISTS 查询在 1.5 万个目标上会产生 1.5 万次查询，太慢。
	done, err := s.completedTargetIDs(ctx, collectorPK, sessionID)
	if err != nil {
		return nil, err
	}

	pending := make([]model.Target, 0, len(targets))
	for _, target := range targets {
		if _, ok := done[target.ID]; ok {
			continue
		}
		pending = append(pending, target)
	}
	return pending, nil
}

// completedTargetIDs 返回某会话中已有测量的目标集合。
func (s *Store) completedTargetIDs(ctx context.Context, collectorPK int64, sessionID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT target_id FROM measurements
		WHERE collector_id = ? AND session_id = ?
	`, collectorPK, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query completed targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan completed target: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate completed targets: %w", err)
	}
	return out, nil
}

// SessionProgress 统计某会话的完成情况。
type SessionProgress struct {
	// Measured 是该会话已有测量的目标数。
	Measured int64

	// Success / Failed 是其中成功与失败的目标数。
	Success int64
	Failed  int64
}

// LoadSessionProgress 读取会话的实际进度。
//
// 与 scan_sessions.completed_count 的区别：那个字段是程序写入的
// "我记得我做了多少"，这里是**从测量数据反推**出来的事实。
// 断点续测以事实为准更安全：即使计数因为崩溃而没更新，
// 也不会重复测量或漏测。
func (s *Store) LoadSessionProgress(ctx context.Context, collectorPK int64, sessionID string) (SessionProgress, error) {
	var out SessionProgress
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT target_id),
			COUNT(DISTINCT CASE WHEN success = 1 THEN target_id END),
			COUNT(DISTINCT CASE WHEN success = 0 THEN target_id END)
		FROM measurements
		WHERE collector_id = ? AND session_id = ?
	`, collectorPK, sessionID).Scan(&out.Measured, &out.Success, &out.Failed); err != nil {
		return out, fmt.Errorf("load session progress: %w", err)
	}
	return out, nil
}

// UpdateSessionProgress 更新会话的完成计数（不改动 finished_at）。
//
// 断点续测需要在扫描过程中周期性记录进度，这样即使进程被杀，
// 用户下次 `db stats` / 查询会话也能看到"上次跑到哪了"。
// 真正的完成状态由 FinishSession 标记（设置 finished_at）。
func (s *Store) UpdateSessionProgress(ctx context.Context, sessionID string, completedCount int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE scan_sessions SET completed_count = ? WHERE id = ?
	`, completedCount, sessionID)
	if err != nil {
		return fmt.Errorf("update session progress %q: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return nil
}

// LatestOpenSession 返回某采集者最近一个**尚未结束**的会话。
//
// 用于 `scan --resume` 不指定会话 ID 时找到"上次没跑完的那次扫描"。
// 找不到时返回 ErrNotFound。
func (s *Store) LatestOpenSession(ctx context.Context, collectorPK int64) (SessionState, error) {
	var (
		out        SessionState
		startedAt  int64
		finishedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, collector_id, started_at, finished_at,
		       target_count, completed_count, client_version
		FROM scan_sessions
		WHERE collector_id = ? AND finished_at IS NULL
		ORDER BY started_at DESC
		LIMIT 1
	`, collectorPK).Scan(&out.ID, &out.CollectorPK, &startedAt, &finishedAt,
		&out.TargetCount, &out.CompletedCount, &out.ClientVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionState{}, fmt.Errorf("%w: no unfinished scan session for this collector", ErrNotFound)
	case err != nil:
		return SessionState{}, fmt.Errorf("load latest open session: %w", err)
	}

	out.StartedAt = time.UnixMilli(startedAt).UTC()
	if finishedAt.Valid {
		out.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	return out, nil
}

// microsecondsToMilliseconds 把库里的整数微秒换算成毫秒。
func microsecondsToMilliseconds(us int64) float64 {
	return float64(us) / 1000
}

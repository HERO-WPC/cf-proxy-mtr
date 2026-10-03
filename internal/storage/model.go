package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
)

// ErrNotFound 表示查询的目标 / 采集者 / 会话不存在。
var ErrNotFound = errors.New("storage: not found")

// Measurement 是一条待写入的 TCP 测量结果。
//
// 它与 probe.ProbeResult 的关系：ProbeResult 是"一次探测发生了什么"，
// Measurement 是"这条事实如何落库"。两者分开的理由：
//
//   - 数据库需要额外维度（采集者、会话、数据格式版本），
//     这些不是探测本身产生的；
//   - probe 包不应该知道数据库的任何概念。
//
// Timestamp 是**测量发生的时间**（探测开始时刻），不是入库时间。
// 两者可能相差很久（批量导入历史数据），因此必须分开记录。
type Measurement struct {
	TargetID    string
	CollectorID int64
	SessionID   string // 空字符串表示不关联会话（NULL）

	Timestamp time.Time

	Success      bool
	LatencyMS    float64
	ErrorType    probe.ErrorType
	ErrorMessage string

	SchemaVersion int
	ClientVersion string
}

// LatencyUS 把毫秒换算成微秒（四舍五入）。
//
// 用整数微秒入库而不是浮点毫秒：避免浮点表示误差，
// 也让"同一测量在不同工具下比较"有确定的基准。
func (m Measurement) LatencyUS() int64 {
	if m.LatencyMS <= 0 {
		return 0
	}
	return int64(m.LatencyMS*1000 + 0.5)
}

// Validate 检查该测量是否可以入库。
//
// 数据库层还有 CHECK 约束兜底，但在这里先检查能给出
// "哪个目标、哪里不对"这种可操作的错误信息。
func (m Measurement) Validate() error {
	if m.TargetID == "" {
		return errors.New("storage: measurement without target id")
	}
	if m.CollectorID <= 0 {
		return fmt.Errorf("storage: measurement for %s without collector", m.TargetID)
	}
	if m.Timestamp.IsZero() {
		return fmt.Errorf("storage: measurement for %s without timestamp", m.TargetID)
	}
	if !m.Success {
		if m.ErrorType == probe.ErrorTypeNone {
			return fmt.Errorf("storage: failed measurement for %s has no error type", m.TargetID)
		}
		if !m.ErrorType.Valid() {
			return fmt.Errorf("storage: measurement for %s has unknown error type %q",
				m.TargetID, m.ErrorType)
		}
	} else if m.ErrorType != probe.ErrorTypeNone {
		return fmt.Errorf("storage: successful measurement for %s carries error type %q",
			m.TargetID, m.ErrorType)
	}
	return nil
}

// NewMeasurement 从探测结果构造测量记录。
func NewMeasurement(collectorID int64, sessionID string, r probe.ProbeResult) Measurement {
	return Measurement{
		TargetID:      r.TargetID,
		CollectorID:   collectorID,
		SessionID:     sessionID,
		Timestamp:     r.Timestamp.UTC(),
		Success:       r.Success,
		LatencyMS:     r.LatencyMS,
		ErrorType:     r.ErrorType,
		ErrorMessage:  r.ErrorMessage,
		SchemaVersion: schemaVersionForMeasurements,
		ClientVersion: clientVersion,
	}
}

// Trace 是一条待写入的线路跟踪结果。
//
// Phase 4 只建立表结构与写入通道；真正的调用来自 Phase 7（NextTrace）
// 与 Phase 8（两级测量）。这里刻意不 import internal/trace，
// 因为那个包在 Phase 7 才会出现——需要它时只需在这里补一个转换函数。
type Trace struct {
	TargetID    string
	CollectorID int64
	SessionID   string

	Timestamp time.Time

	Engine        string
	EngineVersion string
	Mode          string
	Protocol      string
	Port          int

	Success      bool
	DurationMS   float64
	HopCount     int
	TraceJSON    string // 归一化后的内部 Trace（JSON）
	RawJSON      string // 清洗后的原始输出，仅用于诊断
	ErrorType    string
	ErrorMessage string

	// LocalFiltered 表示已应用隐私过滤（本地地址已被移除或标记）。
	LocalFiltered bool

	SchemaVersion int
	ClientVersion string
}

// DurationUS 把毫秒换算成微秒。
func (t Trace) DurationUS() int64 {
	if t.DurationMS <= 0 {
		return 0
	}
	return int64(t.DurationMS*1000 + 0.5)
}

// Validate 检查该 trace 是否可以入库。
func (t Trace) Validate() error {
	if t.TargetID == "" {
		return errors.New("storage: trace without target id")
	}
	if t.CollectorID <= 0 {
		return fmt.Errorf("storage: trace for %s without collector", t.TargetID)
	}
	if t.Timestamp.IsZero() {
		return fmt.Errorf("storage: trace for %s without timestamp", t.TargetID)
	}
	if !model.ValidPort(t.Port) {
		return fmt.Errorf("storage: trace for %s has invalid port %d", t.TargetID, t.Port)
	}
	if !t.Success && t.ErrorType == "" {
		return fmt.Errorf("storage: failed trace for %s has no error type", t.TargetID)
	}
	if t.Success && t.ErrorType != "" {
		return fmt.Errorf("storage: successful trace for %s carries error type %q",
			t.TargetID, t.ErrorType)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 目标
// ---------------------------------------------------------------------------

// TargetView 是从数据库读回的单个目标。
type TargetView struct {
	Target model.Target

	// FirstSeen / LastSeen 是该目标出现在数据源里的时间范围。
	FirstSeen time.Time
	LastSeen  time.Time
}

// UpsertTargets 批量写入 / 更新目标元数据。
//
// 这是**维度表**，因此允许 UPSERT：目标的地理信息可能随上游更新而变化，
// 我们希望保留最新的（同时 first_seen 保持首次出现的时间）。
//
// 与 measurements 的区别：measurements 只追加，
// 因为那些是"发生过的事实"；targets 是"目标当前的样子"。
//
// 返回新增的目标数量（已存在而被更新的不计入）。
func (s *Store) UpsertTargets(ctx context.Context, targets []model.Target, at time.Time) (int, error) {
	if len(targets) == 0 {
		return 0, nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ms := at.UTC().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin target upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO targets (
			id, ip, port, ip_version,
			source_country, source_cca2, source_region, source_city,
			source_latitude, source_longitude, source_country_en,
			colo_iata, colo_cca2, colo_region, colo_city,
			colo_latitude, colo_longitude,
			first_seen, last_seen
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			ip                = excluded.ip,
			port              = excluded.port,
			ip_version        = excluded.ip_version,
			source_country    = excluded.source_country,
			source_cca2       = excluded.source_cca2,
			source_region     = excluded.source_region,
			source_city       = excluded.source_city,
			source_latitude   = excluded.source_latitude,
			source_longitude  = excluded.source_longitude,
			source_country_en = excluded.source_country_en,
			colo_iata         = excluded.colo_iata,
			colo_cca2         = excluded.colo_cca2,
			colo_region       = excluded.colo_region,
			colo_city         = excluded.colo_city,
			colo_latitude     = excluded.colo_latitude,
			colo_longitude    = excluded.colo_longitude,
			-- first_seen 保持首次出现的时间；last_seen 前进。
			-- 注意这里不能用 excluded.first_seen，否则"首次见到"会被覆盖。
			last_seen         = excluded.last_seen
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare target upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	inserted := 0
	for _, t := range targets {
		// 先把目标归一化，避免把非规范形式写进库
		// （例如带空白的国家代码），否则后续按国家分组会出现重复分组。
		target := t
		target.Normalize()

		if !model.ValidPort(target.Port) {
			return inserted, fmt.Errorf("%w: target %q has invalid port %d",
				ErrInvalidInput, target.ID, target.Port)
		}

		// 必须先判断"是否已存在"，再 UPSERT：
		// UPSERT 之后无论新增还是更新，行都存在，判断不出新增数。
		existed, err := s.targetExists(ctx, tx, target.ID)
		if err != nil {
			return inserted, err
		}

		if _, err := stmt.ExecContext(ctx,
			target.ID, target.IP, target.Port, string(target.IPVersion),
			target.Location.Country, target.Location.CCA2, target.Location.Region, target.Location.City,
			nullFloat(target.Location.Latitude, target.Location.HasCoordinates),
			nullFloat(target.Location.Longitude, target.Location.HasCoordinates),
			target.Location.CountryEN,
			target.Colo.IATA, target.Colo.CCA2, target.Colo.Region, target.Colo.City,
			nullFloat(target.Colo.Latitude, target.Colo.HasCoordinates),
			nullFloat(target.Colo.Longitude, target.Colo.HasCoordinates),
			ms, ms,
		); err != nil {
			return inserted, fmt.Errorf("upsert target %q: %w", target.ID, err)
		}

		if !existed {
			inserted++
		}
	}

	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("commit target upsert: %w", err)
	}
	return inserted, nil
}

// targetExists 在事务内判断目标是否已存在。
func (s *Store) targetExists(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM targets WHERE id = ?`, id).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check target %q: %w", id, err)
	default:
		return true, nil
	}
}

// ErrInvalidInput 表示调用方传入了不合法的数据。
var ErrInvalidInput = errors.New("storage: invalid input")

// CountTargets 返回 targets 表的行数。
func (s *Store) CountTargets(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count targets: %w", err)
	}
	return n, nil
}

// LoadTargets 读回全部目标（按 id 升序）。
//
// 主要用于测试与"数据库里现在有哪些目标"的诊断输出；
// 全量扫描使用的目标列表来自 all.json 缓存，而不是数据库。
func (s *Store) LoadTargets(ctx context.Context) ([]TargetView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, ip, port, ip_version,
		       source_country, source_cca2, source_region, source_city,
		       source_latitude, source_longitude, source_country_en,
		       colo_iata, colo_cca2, colo_region, colo_city,
		       colo_latitude, colo_longitude,
		       first_seen, last_seen
		FROM targets
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TargetView
	for rows.Next() {
		var (
			view      TargetView
			ipVersion string
			lat, lon  sql.NullFloat64
			colLat    sql.NullFloat64
			colLon    sql.NullFloat64
			firstSeen int64
			lastSeen  int64
		)

		if err := rows.Scan(
			&view.Target.ID, &view.Target.IP, &view.Target.Port, &ipVersion,
			&view.Target.Location.Country, &view.Target.Location.CCA2,
			&view.Target.Location.Region, &view.Target.Location.City,
			&lat, &lon, &view.Target.Location.CountryEN,
			&view.Target.Colo.IATA, &view.Target.Colo.CCA2,
			&view.Target.Colo.Region, &view.Target.Colo.City,
			&colLat, &colLon,
			&firstSeen, &lastSeen,
		); err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}

		view.Target.IPVersion = model.IPVersion(ipVersion)
		if lat.Valid && lon.Valid {
			view.Target.Location.Latitude = lat.Float64
			view.Target.Location.Longitude = lon.Float64
			view.Target.Location.HasCoordinates = true
		}
		if colLat.Valid && colLon.Valid {
			view.Target.Colo.Latitude = colLat.Float64
			view.Target.Colo.Longitude = colLon.Float64
			view.Target.Colo.HasCoordinates = true
		}
		view.FirstSeen = time.UnixMilli(firstSeen).UTC()
		view.LastSeen = time.UnixMilli(lastSeen).UTC()

		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate targets: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 采集者与会话
// ---------------------------------------------------------------------------

// UpsertCollector 写入或更新一个采集者，返回其内部主键。
//
// collector_id 是唯一键；同一采集者再次运行时只更新资料
// （地区 / 运营商可能被用户改正），created_at 保持不变。
func (s *Store) UpsertCollector(ctx context.Context, collectorID string, profile model.CollectorProfile, clientVersion string) (int64, error) {
	collectorID = strings.TrimSpace(collectorID)
	if collectorID == "" {
		return 0, fmt.Errorf("%w: empty collector id", ErrInvalidInput)
	}

	// 归一化后再入库，避免 "CN" 与 "cn" 变成两个分组。
	profile.Normalize()
	if err := profile.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	now := time.Now().UTC().UnixMilli()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO collectors (
			collector_id, country, province, city, isp, asn, ip_version,
			client_version, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(collector_id) DO UPDATE SET
			country        = excluded.country,
			province       = excluded.province,
			city           = excluded.city,
			isp            = excluded.isp,
			asn            = excluded.asn,
			ip_version     = excluded.ip_version,
			client_version = excluded.client_version,
			updated_at     = excluded.updated_at
	`, collectorID, profile.Country, profile.Province, profile.City, profile.ISP,
		profile.ASN, string(profile.IPVersion), clientVersion, now, now,
	); err != nil {
		return 0, fmt.Errorf("upsert collector %q: %w", collectorID, err)
	}

	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM collectors WHERE collector_id = ?`, collectorID).Scan(&id); err != nil {
		return 0, fmt.Errorf("read collector %q: %w", collectorID, err)
	}
	return id, nil
}

// DefineSession 创建（或更新）一次测量会话，返回会话 ID。
//
// 会话是"这次扫描"的载体；断点续测时重复调用同一个 sessionID
// 不会创建多条记录，而是更新计数。
func (s *Store) DefineSession(ctx context.Context, sessionID string, collectorPK int64, targetCount int, clientVersion string, startedAt time.Time) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: empty session id", ErrInvalidInput)
	}
	if collectorPK <= 0 {
		return fmt.Errorf("%w: session %q without collector", ErrInvalidInput, sessionID)
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scan_sessions (
			id, collector_id, started_at, target_count, completed_count, client_version, created_at
		) VALUES (?,?,?,?,0,?,?)
		ON CONFLICT(id) DO UPDATE SET
			target_count   = excluded.target_count,
			client_version = excluded.client_version
	`, sessionID, collectorPK, startedAt.UTC().UnixMilli(), targetCount, clientVersion,
		time.Now().UTC().UnixMilli(),
	); err != nil {
		return fmt.Errorf("define session %q: %w", sessionID, err)
	}
	return nil
}

// FinishSession 标记会话结束。
//
// finished_at 为空表示未结束（可续测）；置上之后该会话视为完成。
func (s *Store) FinishSession(ctx context.Context, sessionID string, completedCount int, finishedAt time.Time) error {
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE scan_sessions
		SET finished_at = ?, completed_count = ?
		WHERE id = ?
	`, finishedAt.UTC().UnixMilli(), completedCount, sessionID)
	if err != nil {
		return fmt.Errorf("finish session %q: %w", sessionID, err)
	}
	// 目标会话不存在时明确报错：静默失败会让"断点续测"失去意义。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return nil
}

// SessionState 是会话的当前状态（用于断点续测）。
type SessionState struct {
	ID             string
	CollectorPK    int64
	StartedAt      time.Time
	FinishedAt     time.Time
	TargetCount    int
	CompletedCount int
	ClientVersion  string
}

// Finished 报告会话是否已结束。
func (s SessionState) Finished() bool { return !s.FinishedAt.IsZero() }

// LoadSession 读取会话状态。
func (s *Store) LoadSession(ctx context.Context, sessionID string) (SessionState, error) {
	var (
		out        SessionState
		startedAt  int64
		finishedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, collector_id, started_at, finished_at,
		       target_count, completed_count, client_version
		FROM scan_sessions WHERE id = ?
	`, sessionID).Scan(&out.ID, &out.CollectorPK, &startedAt, &finishedAt,
		&out.TargetCount, &out.CompletedCount, &out.ClientVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SessionState{}, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	case err != nil:
		return SessionState{}, fmt.Errorf("load session %q: %w", sessionID, err)
	}

	out.StartedAt = time.UnixMilli(startedAt).UTC()
	if finishedAt.Valid {
		out.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// nullFloat 在 has 为假时返回 NULL，避免把"没有坐标"写成 0,0。
//
// 这一点很重要：0,0 是几内亚湾的合法坐标，
// 用 0 表示"未知"会让后续分析把一批目标误判到同一个点上。
func nullFloat(v float64, has bool) any {
	if !has {
		return nil
	}
	return v
}

// dedupKey 计算一条记录的去重键。
//
// 组成：(collector, session, target, timestamp)。
// 同一个 batch 被重复导入两次时，这四元组完全相同，
// 因此 UNIQUE 约束会让第二次导入变成空操作，统计不会翻倍。
//
// 使用 SHA-256 而不是直接拼接：四元组拼起来可能很长，
// 而且 session_id 里含 "-"、target_id 里含 ":"，直接拼接有歧义风险
// （例如 ("a:b", "c") 与 ("a", "b:c") 会拼成同一个字符串）。
// 哈希前先用长度前缀消除歧义。
func dedupKey(collectorPK int64, sessionID, targetID string, at time.Time) string {
	h := sha256.New()
	writeLenPrefixed(h, strconv.FormatInt(collectorPK, 10))
	writeLenPrefixed(h, sessionID)
	writeLenPrefixed(h, targetID)
	writeLenPrefixed(h, strconv.FormatInt(at.UTC().UnixMilli(), 10))
	return hex.EncodeToString(h.Sum(nil))
}

// stringWriter 是 sha256 需要的最小写入接口。
type stringWriter interface{ Write([]byte) (int, error) }

// writeLenPrefixed 写入"长度:内容"形式，消除拼接歧义。
func writeLenPrefixed(w stringWriter, s string) {
	_, _ = w.Write([]byte(strconv.Itoa(len(s))))
	_, _ = w.Write([]byte{':'})
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{'|'})
}

// measurementDedupKey 计算测量的去重键。
func measurementDedupKey(m Measurement) string {
	return dedupKey(m.CollectorID, m.SessionID, m.TargetID, m.Timestamp)
}

// traceDedupKey 计算 trace 的去重键。
func traceDedupKey(t Trace) string {
	return dedupKey(t.CollectorID, t.SessionID, t.TargetID, t.Timestamp)
}

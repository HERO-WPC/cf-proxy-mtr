package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 本文件提供**导出专用**的流式读取。
//
// 为什么不复用 QueryMeasurements：
//
//  1. 导出必须带上目标元数据（目标 IP、端口、上游地区/colo、坐标），
//     否则无法做隐私过滤，也无法生成自包含的公开数据；
//  2. 导出要能流式处理整库（几十万行），不能先把全部结果读进内存；
//  3. 导出需要按"会话"过滤，而会话是每次采集的边界——
//     Phase 10 上传的是**一次会话**的产物，不是整个历史库。

// ExportFilter 是导出读取的过滤条件。
type ExportFilter struct {
	// CollectorPK > 0 时只导出该采集者的数据。
	//
	// 上传场景下这是必须的：一个节点的库不该混入别人的数据
	// （虽然本地库通常只有一个采集者，但显式限定更安全）。
	CollectorPK int64

	// SessionID 非空时只导出该会话的数据。
	//
	// 这是上传的天然单位：一次扫描 = 一个可独立核对的批次。
	SessionID string

	// Since / Until 限定时间窗口（零值表示不限）。
	Since time.Time
	Until time.Time

	// Limit > 0 时最多导出这么多行（用于抽样检查）。
	Limit int
}

// where 构造过滤条件与参数。
//
// 单独抽出来是为了让 measurements 与 traces 的过滤条件**完全一致**：
// 两份手写的 SQL 迟早会漂移，出现"测量过滤了会话、跟踪没过滤"这种
// 静默错误。
func (f ExportFilter) where(alias string) (string, []any) {
	clauses := make([]string, 0, 5)
	args := make([]any, 0, 5)

	column := func(name string) string {
		if alias == "" {
			return name
		}
		return alias + "." + name
	}

	if f.CollectorPK > 0 {
		clauses = append(clauses, column("collector_id")+" = ?")
		args = append(args, f.CollectorPK)
	}
	if strings.TrimSpace(f.SessionID) != "" {
		clauses = append(clauses, column("session_id")+" = ?")
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, column("ts")+" >= ?")
		args = append(args, f.Since.UTC().UnixMilli())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, column("ts")+" <= ?")
		args = append(args, f.Until.UTC().UnixMilli())
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ---------------------------------------------------------------------------
// 测量导出行
// ---------------------------------------------------------------------------

// ExportMeasurement 是一条可导出的测量（含目标元数据）。
//
// 字段刻意与数据库列同名同义：转换与隐私过滤在 export 包里做，
// storage 只负责"把事实取出来"。
type ExportMeasurement struct {
	ID           int64
	TargetID     string
	CollectorPK  int64
	CollectorAID string
	SessionID    string
	Timestamp    time.Time

	Success      bool
	LatencyMS    float64
	ErrorType    string
	ErrorMessage string

	SchemaVersion int
	ClientVersion string

	// 目标元数据（来自 targets 表）。
	//
	// 注意这里**没有** ASN / ISP：all.json 的目标元数据里不存在这两个字段
	// （它们只属于 Cloudflare 的 colo 信息，而 colo 也不含 ASN）。
	// 公开数据里"目标属于哪个 AS"要靠聚合阶段从 IP 前缀推算，
	// 不在本层凭空造一个字段。
	IP        string
	Port      int
	Country   string
	CCA2      string
	Region    string
	City      string
	CountryEN string

	ColoIATA string
	ColoCCA2 string
	ColoCity string

	// 坐标：指针，nil 表示"没有坐标"。
	//
	// 数据库里用 NULL 表达缺失（0,0 是几内亚湾的合法坐标，
	// 不能用 0 表示"未知"）。
	Latitude  *float64
	Longitude *float64

	// ColoLatitude / ColoLongitude 是 Cloudflare 接入点的坐标。
	ColoLatitude  *float64
	ColoLongitude *float64

	// Collector 画像（公开数据需要它来做地区/运营商分组）。
	CollectorCountry   string
	CollectorProvince  string
	CollectorCity      string
	CollectorISP       string
	CollectorASN       string
	CollectorIPVersion string
}

// StreamMeasurements 流式读取测量，逐行回调。
//
// 回调返回非 nil 错误时立即停止并把该错误返回给调用方——
// 这样导出层可以在磁盘写满时中止读取，而不是继续空转。
func (s *Store) StreamMeasurements(ctx context.Context, filter ExportFilter, fn func(ExportMeasurement) error) (int, error) {
	if fn == nil {
		return 0, fmt.Errorf("%w: stream requires a callback", ErrInvalidInput)
	}

	// 用子查询把测量时间戳命名成 ts，好让 where() 里的写法统一。
	query := `
		SELECT
			m.id, m.target_id, m.collector_id, COALESCE(c.collector_id, ''), COALESCE(m.session_id, ''),
			m.measured_at, m.success, m.latency_us, m.error_type, m.error_message,
			m.schema_version, m.client_version,
			t.ip, t.port, t.source_country, t.source_cca2, t.source_region, t.source_city,
			t.source_country_en, t.colo_iata, t.colo_cca2, t.colo_city,
			t.source_latitude, t.source_longitude, t.colo_latitude, t.colo_longitude,
			c.country, c.province, c.city, c.isp, c.asn, c.ip_version
		FROM measurements m
		JOIN targets t ON t.id = m.target_id
		JOIN collectors c ON c.id = m.collector_id
	`
	whereSQL, args := filter.measurementWhere()

	query += whereSQL
	query += " ORDER BY m.measured_at ASC, m.id ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("stream measurements: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var (
			item           ExportMeasurement
			measuredAt     int64
			latencyUS      int64
			lat, lng       sql.NullFloat64
			colLat, colLng sql.NullFloat64
		)
		if err := rows.Scan(
			&item.ID, &item.TargetID, &item.CollectorPK, &item.CollectorAID, &item.SessionID,
			&measuredAt, &item.Success, &latencyUS, &item.ErrorType, &item.ErrorMessage,
			&item.SchemaVersion, &item.ClientVersion,
			&item.IP, &item.Port, &item.Country, &item.CCA2, &item.Region, &item.City,
			&item.CountryEN, &item.ColoIATA, &item.ColoCCA2, &item.ColoCity,
			&lat, &lng, &colLat, &colLng,
			&item.CollectorCountry, &item.CollectorProvince, &item.CollectorCity,
			&item.CollectorISP, &item.CollectorASN, &item.CollectorIPVersion,
		); err != nil {
			return count, fmt.Errorf("scan measurement row: %w", err)
		}

		item.Timestamp = time.UnixMilli(measuredAt).UTC()
		item.LatencyMS = float64(latencyUS) / 1000
		item.Latitude, item.Longitude = optionalCoordinate(lat, lng)
		item.ColoLatitude, item.ColoLongitude = optionalCoordinate(colLat, colLng)

		if err := fn(item); err != nil {
			return count, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("iterate measurements: %w", err)
	}
	return count, nil
}

// measurementWhere 构造测量导出的过滤条件。
//
// 单独写而不是复用 where()：这里的时间列叫 measured_at、
// 采集者列在 JOIN 后有两个（m.collector_id 与 c.id），
// 必须显式限定，否则 SQL 会报 ambiguous column。
func (f ExportFilter) measurementWhere() (string, []any) {
	clauses := make([]string, 0, 5)
	args := make([]any, 0, 5)

	if f.CollectorPK > 0 {
		clauses = append(clauses, "m.collector_id = ?")
		args = append(args, f.CollectorPK)
	}
	if strings.TrimSpace(f.SessionID) != "" {
		clauses = append(clauses, "m.session_id = ?")
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "m.measured_at >= ?")
		args = append(args, f.Since.UTC().UnixMilli())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "m.measured_at <= ?")
		args = append(args, f.Until.UTC().UnixMilli())
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ---------------------------------------------------------------------------
// 跟踪导出行
// ---------------------------------------------------------------------------

// ExportTrace 是一条可导出的跟踪（含目标元数据与完整跳列表）。
type ExportTrace struct {
	ID           int64
	TargetID     string
	CollectorPK  int64
	CollectorAID string
	SessionID    string
	Timestamp    time.Time

	Engine        string
	EngineVersion string
	Mode          string
	Protocol      string
	Port          int

	Success      bool
	DurationMS   float64
	HopCount     int
	TraceJSON    string
	ErrorType    string
	ErrorMessage string

	SchemaVersion int
	ClientVersion string

	// 目标元数据（来自 targets 表）。同样没有 ASN / ISP（见上）。
	IP        string
	Country   string
	CCA2      string
	Region    string
	City      string
	CountryEN string

	ColoIATA string
	ColoCCA2 string
	ColoCity string

	Latitude  *float64
	Longitude *float64

	ColoLatitude  *float64
	ColoLongitude *float64

	CollectorCountry   string
	CollectorProvince  string
	CollectorCity      string
	CollectorISP       string
	CollectorASN       string
	CollectorIPVersion string
}

// StreamTraces 流式读取跟踪结果。
//
// 注意**不读 raw_json**：原始引擎输出只用于本地诊断，
// 体积大（实测单条 24 KB）且含未经脱敏的中间信息，
// 公开数据里没有它的位置。这是刻意的省略，不是遗漏。
func (s *Store) StreamTraces(ctx context.Context, filter ExportFilter, fn func(ExportTrace) error) (int, error) {
	if fn == nil {
		return 0, fmt.Errorf("%w: stream requires a callback", ErrInvalidInput)
	}

	query := `
		SELECT
			r.id, r.target_id, r.collector_id, COALESCE(c.collector_id, ''), COALESCE(r.session_id, ''),
			r.traced_at, r.engine, r.engine_version, r.mode, r.protocol, r.port,
			r.success, r.duration_us, r.hop_count, r.trace_json, r.error_type, r.error_message,
			r.schema_version, r.client_version,
			t.ip, t.source_country, t.source_cca2, t.source_region, t.source_city,
			t.source_country_en, t.colo_iata, t.colo_cca2, t.colo_city,
			t.source_latitude, t.source_longitude, t.colo_latitude, t.colo_longitude,
			c.country, c.province, c.city, c.isp, c.asn, c.ip_version
		FROM traces r
		JOIN targets t ON t.id = r.target_id
		JOIN collectors c ON c.id = r.collector_id
	`
	whereSQL, args := filter.traceWhere()
	query += whereSQL
	query += " ORDER BY r.traced_at ASC, r.id ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("stream traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	count := 0
	for rows.Next() {
		var (
			item           ExportTrace
			tracedAt       int64
			durationU      int64
			lat, lng       sql.NullFloat64
			colLat, colLng sql.NullFloat64
		)
		if err := rows.Scan(
			&item.ID, &item.TargetID, &item.CollectorPK, &item.CollectorAID, &item.SessionID,
			&tracedAt, &item.Engine, &item.EngineVersion, &item.Mode, &item.Protocol, &item.Port,
			&item.Success, &durationU, &item.HopCount, &item.TraceJSON, &item.ErrorType, &item.ErrorMessage,
			&item.SchemaVersion, &item.ClientVersion,
			&item.IP, &item.Country, &item.CCA2, &item.Region, &item.City,
			&item.CountryEN, &item.ColoIATA, &item.ColoCCA2, &item.ColoCity,
			&lat, &lng, &colLat, &colLng,
			&item.CollectorCountry, &item.CollectorProvince, &item.CollectorCity,
			&item.CollectorISP, &item.CollectorASN, &item.CollectorIPVersion,
		); err != nil {
			return count, fmt.Errorf("scan trace row: %w", err)
		}

		item.Timestamp = time.UnixMilli(tracedAt).UTC()
		item.DurationMS = float64(durationU) / 1000
		item.Latitude, item.Longitude = optionalCoordinate(lat, lng)
		item.ColoLatitude, item.ColoLongitude = optionalCoordinate(colLat, colLng)

		if err := fn(item); err != nil {
			return count, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("iterate traces: %w", err)
	}
	return count, nil
}

// traceWhere 构造跟踪导出的过滤条件。
func (f ExportFilter) traceWhere() (string, []any) {
	clauses := make([]string, 0, 5)
	args := make([]any, 0, 5)

	if f.CollectorPK > 0 {
		clauses = append(clauses, "r.collector_id = ?")
		args = append(args, f.CollectorPK)
	}
	if strings.TrimSpace(f.SessionID) != "" {
		clauses = append(clauses, "r.session_id = ?")
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "r.traced_at >= ?")
		args = append(args, f.Since.UTC().UnixMilli())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "r.traced_at <= ?")
		args = append(args, f.Until.UTC().UnixMilli())
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// optionalCoordinate 把两个可空坐标列转成指针。
//
// 只有两个都有效才返回非 nil：单边坐标是无意义的数据，
// 保留它会让下游以为"这个目标有坐标"。
func optionalCoordinate(lat, lng sql.NullFloat64) (*float64, *float64) {
	if !lat.Valid || !lng.Valid {
		return nil, nil
	}
	latitude, longitude := lat.Float64, lng.Float64
	return &latitude, &longitude
}

// ListSessions 返回数据库里已有的会话（供导出选择 --session）。
//
// 按开始时间倒序：用户最可能想导出的是最近一次扫描。
func (s *Store) ListSessions(ctx context.Context, collectorPK int64) ([]SessionSummary, error) {
	query := `
		SELECT
			s.id, s.collector_id, COALESCE(c.collector_id, ''),
			s.started_at, s.finished_at, s.target_count, s.completed_count,
			(SELECT COUNT(DISTINCT m.target_id) FROM measurements m
			  WHERE m.session_id = s.id AND m.collector_id = s.collector_id),
			(SELECT COUNT(DISTINCT r.target_id) FROM traces r
			  WHERE r.session_id = s.id AND r.collector_id = s.collector_id)
		FROM scan_sessions s
		LEFT JOIN collectors c ON c.id = s.collector_id
	`
	args := []any{}
	if collectorPK > 0 {
		query += " WHERE s.collector_id = ?"
		args = append(args, collectorPK)
	}
	query += " ORDER BY s.started_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SessionSummary, 0, 16)
	for rows.Next() {
		var (
			item      SessionSummary
			startedAt int64
			finished  sql.NullInt64
		)
		if err := rows.Scan(
			&item.ID, &item.CollectorPK, &item.CollectorAID,
			&startedAt, &finished, &item.TargetCount, &item.CompletedCount,
			&item.MeasuredTargets, &item.TracedTargets,
		); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		item.StartedAt = time.UnixMilli(startedAt).UTC()
		if finished.Valid {
			item.FinishedAt = time.UnixMilli(finished.Int64).UTC()
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return out, nil
}

// SessionSummary 是一个会话的概览（供导出与查询展示）。
type SessionSummary struct {
	ID           string
	CollectorPK  int64
	CollectorAID string

	StartedAt  time.Time
	FinishedAt time.Time

	TargetCount    int
	CompletedCount int

	// MeasuredTargets / TracedTargets 是从**数据事实**统计出来的数量，
	// 而不是会话行里记录的计数器。
	MeasuredTargets int
	TracedTargets   int
}

// Finished 报告会话是否已结束。
func (s SessionSummary) Finished() bool { return !s.FinishedAt.IsZero() }

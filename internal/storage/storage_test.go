package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
	"github.com/cf-route-tester/cf-route-tester/internal/probe"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// memoryCounter 让每个内存数据库拥有独立的连接串。
//
// 用 ":memory:" 时驱动会为每个连接创建**独立**的库，
// 连接池拿到不同连接就会看到不同的表，测试会莫名其妙地失败。
// 加上 cache=shared 与唯一名字后，同一个库在整个测试进程内共享。
var memoryCounter int64

func newTestStore(t *testing.T) *Store {
	t.Helper()

	name := fmt.Sprintf("file:testdb%d?mode=memory&cache=shared", atomic.AddInt64(&memoryCounter, 1))
	store, err := Open(context.Background(), Config{Path: name})
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func newFileStore(t *testing.T) *Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "results.db")
	store, err := Open(context.Background(), Config{Path: path})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// mustCollector 写入一个采集者并返回其主键。
func mustCollector(t *testing.T, s *Store, collectorID string, profile model.CollectorProfile) int64 {
	t.Helper()
	pk, err := s.UpsertCollector(context.Background(), collectorID, profile, "0.1.0")
	if err != nil {
		t.Fatalf("UpsertCollector: %v", err)
	}
	return pk
}

// chengduTarget 返回一个字段完整的目标。
func testTarget(ip string, port int) model.Target {
	target, err := model.NewTargetFromStrings(ip, port)
	if err != nil {
		panic(err)
	}
	target.Location = model.Location{
		Country:        "US",
		CCA2:           "US",
		Region:         "Illinois",
		City:           "Chicago",
		Latitude:       41.85003,
		Longitude:      -87.65005,
		CountryEN:      "United States",
		HasCoordinates: true,
	}
	target.Colo = model.ColoInfo{
		IATA:           "ORD",
		CCA2:           "US",
		Region:         "North America",
		City:           "Chicago",
		Latitude:       41.9786,
		Longitude:      -87.9048,
		HasCoordinates: true,
	}
	return target
}

func testProfile() model.CollectorProfile {
	return model.CollectorProfile{
		Country:   "CN",
		Province:  "Zhejiang",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "AS9808",
		IPVersion: model.IPVersionIPv4,
	}
}

// mustSession 声明一个测量会话并返回其 ID。
//
// 测量可以不带会话（session_id 为 NULL），但只要带了，
// 就必须指向真实存在的会话——这是数据库层的外键约束在保证
// "断点续测所用的会话确实存在"，否则 (target, collector, session)
// 三元组的判据就失去意义。
func mustSession(t *testing.T, s *Store, sessionID string, collectorPK int64, targetCount int) string {
	t.Helper()
	if err := s.DefineSession(context.Background(), sessionID, collectorPK, targetCount, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatalf("DefineSession(%s): %v", sessionID, err)
	}
	return sessionID
}

// okMeasurement 构造一条成功的测量。
func okMeasurement(targetID string, collectorPK int64, sessionID string, at time.Time, latencyMS float64) Measurement {
	return Measurement{
		TargetID:    targetID,
		CollectorID: collectorPK,
		SessionID:   sessionID,
		Timestamp:   at,
		Success:     true,
		LatencyMS:   latencyMS,
	}
}

// failMeasurement 构造一条失败的测量。
func failMeasurement(targetID string, collectorPK int64, sessionID string, at time.Time, kind probe.ErrorType) Measurement {
	return Measurement{
		TargetID:     targetID,
		CollectorID:  collectorPK,
		SessionID:    sessionID,
		Timestamp:    at,
		Success:      false,
		ErrorType:    kind,
		ErrorMessage: "connect: " + string(kind),
	}
}

// ---------------------------------------------------------------------------
// 打开与迁移
// ---------------------------------------------------------------------------

func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	v, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != ExpectedSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, want %d", v, ExpectedSchemaVersion())
	}

	// 重复迁移必须无副作用（幂等）。
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}

	var applied int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(migrations) {
		t.Errorf("applied migrations = %d, want %d", applied, len(migrations))
	}

	// 重新打开同一个文件也必须成功（结构已存在）。
	path := store.Path()
	store2, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store2.Close() }()

	if v, err := store2.SchemaVersion(ctx); err != nil || v != ExpectedSchemaVersion() {
		t.Errorf("reopened SchemaVersion = %d (err=%v), want %d", v, err, ExpectedSchemaVersion())
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "results.db")
	store, err := Open(context.Background(), Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.DB().Exec(`SELECT 1`); err != nil {
		t.Fatalf("database unusable: %v", err)
	}
}

func TestOpenUsesWALAndForeignKeys(t *testing.T) {
	store := newFileStore(t)
	ctx := context.Background()

	var journalMode string
	if err := store.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var fk int
	if err := store.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

// TestSchemaHasNoPrivacyColumns 是一条隐私约束测试（需求第 14 条）。
//
// 它直接检查数据库实际创建出来的列名，而不是检查建表语句文本：
// 只要有人将来新增了携带隐私信息的列，这里就会失败。
func TestSchemaHasNoPrivacyColumns(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	forbidden := []string{
		"public_ip", "publicip", "local_ip", "localip", "mac", "mac_address",
		"hostname", "host_name", "device_id", "deviceid", "machine_id",
		"serial", "serial_number", "uuid", "hwid", "imei", "imsi",
		"latitude_precise", "home_address", "address", "geohash",
	}

	for _, table := range knownTables {
		rows, err := store.DB().QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		var columns []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				t.Fatalf("scan column: %v", err)
			}
			columns = append(columns, name)
		}
		_ = rows.Close()

		if len(columns) == 0 {
			t.Errorf("table %q has no columns (missing?)", table)
		}
		for _, col := range columns {
			lower := strings.ToLower(col)
			for _, bad := range forbidden {
				if lower == bad {
					t.Errorf("table %q has column %q, which must never be persisted", table, col)
				}
			}
		}
	}
}

// TestMeasurementTableIsAppendOnlyByDesign 检查关键列与约束存在。
//
// 这里断言的是"设计意图在数据库层被强制"：
// success 与 error_type 的一致性由 CHECK 约束保证，
// 去重由 UNIQUE 索引保证，而不是只靠调用方自觉。
func TestMeasurementTableConstraints(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 失败但没有 error_type 必须被数据库拒绝。
	collectorPK := mustCollector(t, store, "c-test", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatalf("UpsertTargets: %v", err)
	}

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO measurements (
			target_id, collector_id, measured_at, success, latency_us,
			error_type, dedup_key, created_at
		) VALUES ('1.2.3.4:443', ?, ?, 0, 0, '', 'x', 0)`, collectorPK, time.Now().UnixMilli())
	if err == nil {
		t.Fatal("database accepted a failed measurement without error_type; CHECK constraint is missing")
	}

	// 端口越界必须被拒绝。
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO targets (id, ip, port, first_seen, last_seen)
		VALUES ('1.2.3.4:0', '1.2.3.4', 0, 0, 0)`)
	if err == nil {
		t.Fatal("database accepted port 0; CHECK constraint is missing")
	}
}

// ---------------------------------------------------------------------------
// 目标
// ---------------------------------------------------------------------------

func TestUpsertTargetsInsertAndUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	targets := []model.Target{
		testTarget("1.2.3.4", 443),
		testTarget("1.2.3.4", 8443),
		testTarget("5.6.7.8", 2053),
	}

	first := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	inserted, err := store.UpsertTargets(ctx, targets, first)
	if err != nil {
		t.Fatalf("UpsertTargets: %v", err)
	}
	if inserted != 3 {
		t.Errorf("inserted = %d, want 3", inserted)
	}

	// 再写一次：全部已存在，inserted 必须为 0。
	second := first.Add(2 * time.Hour)
	inserted, err = store.UpsertTargets(ctx, targets, second)
	if err != nil {
		t.Fatalf("second UpsertTargets: %v", err)
	}
	if inserted != 0 {
		t.Errorf("inserted = %d, want 0 on re-upsert", inserted)
	}

	views, err := store.LoadTargets(ctx)
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if len(views) != 3 {
		t.Fatalf("targets = %d, want 3", len(views))
	}

	// first_seen 必须保持首次时间，last_seen 必须前进。
	for _, v := range views {
		if !v.FirstSeen.Equal(first) {
			t.Errorf("%s FirstSeen = %s, want %s (must not be overwritten)",
				v.Target.ID, v.FirstSeen, first)
		}
		if !v.LastSeen.Equal(second) {
			t.Errorf("%s LastSeen = %s, want %s", v.Target.ID, v.LastSeen, second)
		}
	}
}

// TestUpsertTargetsUpdatesMetadata 验证维度表允许 UPSERT 更新地理位置。
func TestUpsertTargetsUpdatesMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	original := testTarget("1.2.3.4", 443)
	if _, err := store.UpsertTargets(ctx, []model.Target{original}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// 上游改正了城市信息。
	updated := original
	updated.Location.City = "Springfield"
	if _, err := store.UpsertTargets(ctx, []model.Target{updated}, time.Now()); err != nil {
		t.Fatal(err)
	}

	views, err := store.LoadTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("targets = %d, want 1 (upsert must not duplicate)", len(views))
	}
	if views[0].Target.Location.City != "Springfield" {
		t.Errorf("City = %q, want Springfield (targets is a dimension table and should update)",
			views[0].Target.Location.City)
	}
}

// TestUpsertTargetsKeepsMissingCoordinatesNull 验证"没有坐标"写成 NULL 而不是 0。
func TestUpsertTargetsKeepsMissingCoordinatesNull(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	noCoords := testTarget("9.9.9.9", 443)
	noCoords.Location = model.Location{Country: "JP", CCA2: "JP", City: "Tokyo"}
	noCoords.Colo = model.ColoInfo{}

	if _, err := store.UpsertTargets(ctx, []model.Target{noCoords}, time.Now()); err != nil {
		t.Fatal(err)
	}

	views, err := store.LoadTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if views[0].Target.Location.HasCoordinates {
		t.Error("HasCoordinates = true, want false for a target without coordinates")
	}
	// 0,0 是合法坐标（几内亚湾），绝不能因为"存了 0"而被当成有坐标。
	var lat, lon *float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT source_latitude, source_longitude FROM targets WHERE id = ?`,
		noCoords.ID).Scan(&lat, &lon); err != nil {
		t.Fatal(err)
	}
	if lat != nil || lon != nil {
		t.Errorf("coordinates = %v/%v, want NULL for unknown location", lat, lon)
	}
}

func TestUpsertTargetsRejectsInvalidPort(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	broken := model.Target{ID: "1.2.3.4:99999", IP: "1.2.3.4", Port: 99999}
	_, err := store.UpsertTargets(ctx, []model.Target{broken}, time.Now())
	if err == nil {
		t.Fatal("UpsertTargets accepted an invalid port")
	}
	if !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("error = %v, want a message about the invalid port", err)
	}
}

func TestUpsertTargetsNormalizesBeforeStoring(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 带空白与错误大小写的输入必须先被归一化，
	// 否则按国家分组时会出现 "cn" 与 "CN" 两个组。
	messy := testTarget("1.2.3.4", 443)
	messy.Location.Country = " us "
	messy.Location.CCA2 = "us"
	messy.Location.City = " Chicago "
	messy.Colo.IATA = " ord "
	messy.Colo.CCA2 = "us"

	if _, err := store.UpsertTargets(ctx, []model.Target{messy}, time.Now()); err != nil {
		t.Fatal(err)
	}

	views, err := store.LoadTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := views[0].Target
	if got.Location.Country != "US" || got.Location.CCA2 != "US" {
		t.Errorf("country codes = %q/%q, want US/US", got.Location.Country, got.Location.CCA2)
	}
	if got.Location.City != "Chicago" {
		t.Errorf("city = %q, want Chicago", got.Location.City)
	}
	if got.Colo.IATA != "ORD" || got.Colo.CCA2 != "US" {
		t.Errorf("colo = %+v, want ORD/US", got.Colo)
	}
}

// ---------------------------------------------------------------------------
// 采集者与会话
// ---------------------------------------------------------------------------

func TestUpsertCollectorIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pk1 := mustCollector(t, store, "c-0123456789abcdef0123456789abcdef", testProfile())

	// 同一 collector_id 再来一次：必须返回同一个主键，不产生新行。
	pk2 := mustCollector(t, store, "c-0123456789abcdef0123456789abcdef", testProfile())
	if pk1 != pk2 {
		t.Errorf("primary keys differ: %d vs %d (collector_id must be unique)", pk1, pk2)
	}

	collectors, err := store.LoadCollectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(collectors) != 1 {
		t.Fatalf("collectors = %d, want 1", len(collectors))
	}
	if collectors[0].Country != "CN" || collectors[0].ASN != "AS9808" {
		t.Errorf("collector = %+v, want CN/AS9808", collectors[0])
	}
}

func TestUpsertCollectorNormalizesAndValidates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 归一化：country 小写、asn 缺前缀都必须被修正后再入库。
	profile := model.CollectorProfile{
		Country:   "cn",
		Province:  "Zhejiang",
		City:      "Hangzhou",
		ISP:       "China Mobile",
		ASN:       "9808",
		IPVersion: "IPv4",
	}
	pk := mustCollector(t, store, "c-normalize-test", profile)

	collectors, err := store.LoadCollectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := collectors[0]
	if got.PK != pk {
		t.Errorf("PK = %d, want %d", got.PK, pk)
	}
	if got.Country != "CN" || got.ASN != "AS9808" || got.IPVersion != "ipv4" {
		t.Errorf("collector = %+v, want normalized CN/AS9808/ipv4", got)
	}

	// 非法输入必须被拒绝（而不是悄悄写进去）。
	for name, bad := range map[string]model.CollectorProfile{
		"bad country":    {Country: "CHN"},
		"bad asn":        {ASN: "ASCMCC"},
		"bad ip_version": {IPVersion: "ipv5"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.UpsertCollector(ctx, "c-bad-"+name, bad, "0.1.0"); err == nil {
				t.Error("UpsertCollector accepted an invalid profile")
			}
		})
	}

	if _, err := store.UpsertCollector(ctx, "  ", testProfile(), "0.1.0"); err == nil {
		t.Error("UpsertCollector accepted an empty collector id")
	}
}

func TestSessionLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-session-test", testProfile())
	started := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	if err := store.DefineSession(ctx, "20260101T000000Z-00000000", collectorPK, 14635, "0.1.0", started); err != nil {
		t.Fatalf("DefineSession: %v", err)
	}

	// 重复定义（断点续测会这么做）不能报错，也不能丢计数。
	if err := store.DefineSession(ctx, "20260101T000000Z-00000000", collectorPK, 14635, "0.1.0", started); err != nil {
		t.Fatalf("second DefineSession: %v", err)
	}

	session, err := store.LoadSession(ctx, "20260101T000000Z-00000000")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if session.Finished() {
		t.Error("session reports as finished before FinishSession")
	}
	if session.TargetCount != 14635 {
		t.Errorf("TargetCount = %d, want 14635", session.TargetCount)
	}
	if !session.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %s, want %s", session.StartedAt, started)
	}

	finished := started.Add(30 * time.Minute)
	if err := store.FinishSession(ctx, "20260101T000000Z-00000000", 14635, finished); err != nil {
		t.Fatalf("FinishSession: %v", err)
	}

	session, err = store.LoadSession(ctx, "20260101T000000Z-00000000")
	if err != nil {
		t.Fatal(err)
	}
	if !session.Finished() {
		t.Error("session not finished after FinishSession")
	}
	if session.CompletedCount != 14635 {
		t.Errorf("CompletedCount = %d, want 14635", session.CompletedCount)
	}

	// 不存在的会话必须明确报错，而不是静默成功。
	if _, err := store.LoadSession(ctx, "nope"); err == nil {
		t.Error("LoadSession succeeded for a missing session")
	}
	if err := store.FinishSession(ctx, "nope", 1, finished); err == nil {
		t.Error("FinishSession succeeded for a missing session")
	}
}

// ---------------------------------------------------------------------------
// 测量：只追加、幂等、时间序列
// ---------------------------------------------------------------------------

// TestMeasurementsAreAppendOnly 是需求第 40 条的核心测试。
//
// 同一个目标在不同时间的测量必须**同时存在**，
// 否则"什么时候变差"永远无法回答。
func TestMeasurementsAreAppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-append-only", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	morning := okMeasurement("1.2.3.4:443", collectorPK, "", base, 42.3)
	evening := okMeasurement("1.2.3.4:443", collectorPK, "", base.Add(10*time.Hour), 86.1)

	saved, skipped, err := store.SaveMeasurements(ctx, []Measurement{morning, evening})
	if err != nil {
		t.Fatalf("SaveMeasurements: %v", err)
	}
	if saved != 2 || skipped != 0 {
		t.Fatalf("saved/skipped = %d/%d, want 2/0", saved, skipped)
	}

	rows, err := store.QueryMeasurements(ctx, MeasurementQuery{TargetID: "1.2.3.4:443"})
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("measurements = %d, want 2 (history must not be overwritten)", len(rows))
	}
	// 默认按时间正序，便于观察变化趋势。
	if rows[0].LatencyMS != 42.3 || rows[1].LatencyMS != 86.1 {
		t.Errorf("latencies = %v/%v, want 42.3/86.1", rows[0].LatencyMS, rows[1].LatencyMS)
	}
	if !rows[0].Timestamp.Before(rows[1].Timestamp) {
		t.Errorf("timestamps not ordered: %s then %s", rows[0].Timestamp, rows[1].Timestamp)
	}
}

// TestMeasurementsAreIdempotent 验证"同一个 batch 重复导入"不会让统计翻倍。
func TestMeasurementsAreIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-idempotent", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustSession(t, store, "s1", collectorPK, 1)

	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	batch := []Measurement{
		okMeasurement("1.2.3.4:443", collectorPK, "s1", at, 42),
		failMeasurement("1.2.3.4:443", collectorPK, "s1", at.Add(time.Minute), probe.ErrorTypeTimeout),
	}

	saved, skipped, err := store.SaveMeasurements(ctx, batch)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if saved != 2 || skipped != 0 {
		t.Fatalf("first save = %d/%d, want 2/0", saved, skipped)
	}

	// 完全相同的一批再来一次（模拟重复上传）。
	saved, skipped, err = store.SaveMeasurements(ctx, batch)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if saved != 0 || skipped != 2 {
		t.Errorf("second save = %d/%d, want 0/2 (duplicates must be skipped)", saved, skipped)
	}

	total, err := store.CountMeasurements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("measurements = %d, want 2 (statistics must not double)", total)
	}
}

// TestMeasurementDedupDistinguishesRealDifferences 验证去重键不会"过度去重"。
func TestMeasurementDedupDistinguishesRealDifferences(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorA := mustCollector(t, store, "c-collector-a", testProfile())
	collectorB := mustCollector(t, store, "c-collector-b", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{
		testTarget("1.2.3.4", 443),
		testTarget("1.2.3.5", 443),
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustSession(t, store, "s1", collectorA, 2)
	mustSession(t, store, "s1", collectorB, 2)
	mustSession(t, store, "s2", collectorA, 2)

	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	// 这些都不是重复：不同采集者、不同目标、不同时间、不同会话。
	distinct := []Measurement{
		okMeasurement("1.2.3.4:443", collectorA, "s1", at, 40),
		okMeasurement("1.2.3.4:443", collectorB, "s1", at, 41),                  // 不同采集者
		okMeasurement("1.2.3.5:443", collectorA, "s1", at, 42),                  // 不同目标
		okMeasurement("1.2.3.4:443", collectorA, "s1", at.Add(time.Second), 43), // 不同时间
		okMeasurement("1.2.3.4:443", collectorA, "s2", at, 44),                  // 不同会话
	}

	saved, skipped, err := store.SaveMeasurements(ctx, distinct)
	if err != nil {
		t.Fatalf("SaveMeasurements: %v", err)
	}
	if saved != len(distinct) || skipped != 0 {
		t.Errorf("saved/skipped = %d/%d, want %d/0 (all rows are genuinely distinct)",
			saved, skipped, len(distinct))
	}
}

// TestFailureResultsAreStored 验证失败也会入库（需求第 39 条）。
func TestFailureResultsAreStored(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-failures", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	_, _, err := store.SaveMeasurements(ctx, []Measurement{
		failMeasurement("1.2.3.4:443", collectorPK, "", at, probe.ErrorTypeTimeout),
		failMeasurement("1.2.3.4:443", collectorPK, "", at.Add(time.Minute), probe.ErrorTypeConnectionRefused),
		failMeasurement("1.2.3.4:443", collectorPK, "", at.Add(2*time.Minute), probe.ErrorTypeCanceled),
	})
	if err != nil {
		t.Fatalf("SaveMeasurements: %v", err)
	}

	rows, err := store.QueryMeasurements(ctx, MeasurementQuery{TargetID: "1.2.3.4:443"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("measurements = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.Success {
			t.Errorf("failed measurement stored as success: %+v", r)
		}
		if r.ErrorType == probe.ErrorTypeNone {
			t.Errorf("failed measurement without error type: %+v", r)
		}
		if r.ErrorMessage == "" {
			t.Errorf("failed measurement without message: %+v", r)
		}
	}
	// 分类必须原样保留，否则"哪一种失败"就无法聚合。
	if rows[0].ErrorType != probe.ErrorTypeTimeout {
		t.Errorf("ErrorType = %q, want %q", rows[0].ErrorType, probe.ErrorTypeTimeout)
	}
}

func TestSaveMeasurementsValidatesBeforeWriting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-validate", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC()
	cases := map[string]Measurement{
		"no target":            {CollectorID: collectorPK, Timestamp: at, Success: true},
		"no collector":         {TargetID: "1.2.3.4:443", Timestamp: at, Success: true},
		"no timestamp":         {TargetID: "1.2.3.4:443", CollectorID: collectorPK, Success: true},
		"failed without type":  {TargetID: "1.2.3.4:443", CollectorID: collectorPK, Timestamp: at},
		"unknown error type":   {TargetID: "1.2.3.4:443", CollectorID: collectorPK, Timestamp: at, ErrorType: "weird"},
		"success with errtype": {TargetID: "1.2.3.4:443", CollectorID: collectorPK, Timestamp: at, Success: true, ErrorType: probe.ErrorTypeTimeout},
	}

	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := store.SaveMeasurements(ctx, []Measurement{m}); err == nil {
				t.Error("SaveMeasurements accepted an invalid measurement")
			}
		})
	}

	// 整批校验失败时不能写入任何一行。
	good := okMeasurement("1.2.3.4:443", collectorPK, "", at, 10)
	bad := Measurement{TargetID: "", CollectorID: collectorPK, Timestamp: at}
	if _, _, err := store.SaveMeasurements(ctx, []Measurement{good, bad}); err == nil {
		t.Fatal("batch with an invalid row was accepted")
	}
	total, err := store.CountMeasurements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("measurements = %d, want 0 (a failed batch must write nothing)", total)
	}
}

// TestSaveMeasurementsRequiresTargetToExist 验证外键约束在起作用。
//
// 目标还没入库就写测量，会产生"后续无法分析"的孤儿数据，
// 因此必须在写入时失败，而不是默默接受。
func TestSaveMeasurementsRequiresTargetToExist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-fk", testProfile())

	_, _, err := store.SaveMeasurements(ctx, []Measurement{
		okMeasurement("9.9.9.9:443", collectorPK, "", time.Now(), 10),
	})
	if err == nil {
		t.Fatal("SaveMeasurements accepted a measurement for an unknown target")
	}
	if !strings.Contains(err.Error(), "目标或采集者尚未入库") {
		t.Errorf("error = %v, want an actionable message", err)
	}
}

// TestSaveMeasurementsCascadeDelete 验证外键的 ON DELETE CASCADE 生效。
func TestSaveMeasurementsCascadeDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-cascade", testProfile())
	target := testTarget("1.2.3.4", 443)
	if _, err := store.UpsertTargets(ctx, []model.Target{target}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SaveMeasurements(ctx, []Measurement{
		okMeasurement(target.ID, collectorPK, "", time.Now(), 10),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DB().ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, target.ID); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	// 测量必须随之消失：留着的孤儿测量会让"样本数"永远对不上目标。
	total, err := store.CountMeasurements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("measurements = %d, want 0 after the target was deleted", total)
	}
}

// ---------------------------------------------------------------------------
// Trace
// ---------------------------------------------------------------------------

func TestSaveTraces(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-traces", testProfile())
	target := testTarget("1.2.3.4", 2053)
	if _, err := store.UpsertTargets(ctx, []model.Target{target}, time.Now()); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	mustSession(t, store, "s1", collectorPK, 1)

	trace := Trace{
		TargetID:      target.ID,
		CollectorID:   collectorPK,
		SessionID:     "s1",
		Timestamp:     at,
		Engine:        "nexttrace",
		EngineVersion: "1.3.0",
		Mode:          "traceroute",
		Protocol:      "tcp",
		Port:          2053,
		Success:       true,
		DurationMS:    1234.5,
		HopCount:      12,
		TraceJSON:     `{"hops":[{"ttl":1}]}`,
		RawJSON:       `{"raw":true}`,
		LocalFiltered: true,
	}

	saved, skipped, err := store.SaveTraces(ctx, []Trace{trace})
	if err != nil {
		t.Fatalf("SaveTraces: %v", err)
	}
	if saved != 1 || skipped != 0 {
		t.Fatalf("saved/skipped = %d/%d, want 1/0", saved, skipped)
	}

	// 重复写入必须幂等。
	saved, skipped, err = store.SaveTraces(ctx, []Trace{trace})
	if err != nil {
		t.Fatal(err)
	}
	if saved != 0 || skipped != 1 {
		t.Errorf("second save = %d/%d, want 0/1", saved, skipped)
	}

	views, err := store.QueryTraces(ctx, target.ID, 10)
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("traces = %d, want 1", len(views))
	}
	got := views[0]
	if got.Port != 2053 {
		t.Errorf("Port = %d, want 2053 (the measured port, not 443)", got.Port)
	}
	if got.Engine != "nexttrace" || got.HopCount != 12 {
		t.Errorf("trace = %+v, want nexttrace with 12 hops", got)
	}
	if !got.LocalFiltered {
		t.Error("LocalFiltered = false, want true")
	}

	// 完整 JSON 与原始 JSON 单独读取。
	traceJSON, rawJSON, err := store.LoadTraceJSON(ctx, got.ID)
	if err != nil {
		t.Fatalf("LoadTraceJSON: %v", err)
	}
	if !strings.Contains(traceJSON, "hops") || rawJSON == "" {
		t.Errorf("traceJSON/rawJSON = %q/%q, want both stored", traceJSON, rawJSON)
	}
}

func TestSaveTracesValidates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-trace-validate", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{testTarget("1.2.3.4", 443)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	at := time.Now().UTC()
	base := Trace{
		TargetID: "1.2.3.4:443", CollectorID: collectorPK, Timestamp: at,
		Port: 443, Success: true,
	}

	cases := map[string]func(*Trace){
		"bad port":            func(tr *Trace) { tr.Port = 0 },
		"failed without type": func(tr *Trace) { tr.Success = false },
		"success with type":   func(tr *Trace) { tr.ErrorType = "timeout" },
		"no timestamp":        func(tr *Trace) { tr.Timestamp = time.Time{} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			trace := base
			mutate(&trace)
			if _, _, err := store.SaveTraces(ctx, []Trace{trace}); err == nil {
				t.Error("SaveTraces accepted an invalid trace")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

func TestQueryMeasurementsFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorA := mustCollector(t, store, "c-query-a", testProfile())
	collectorB := mustCollector(t, store, "c-query-b", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{
		testTarget("1.2.3.4", 443),
		testTarget("5.6.7.8", 443),
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	mustSession(t, store, "s1", collectorA, 4)
	mustSession(t, store, "s2", collectorB, 1)

	base := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	var batch []Measurement
	// 目标 A：采集者 A 成功两次、失败一次
	batch = append(batch,
		okMeasurement("1.2.3.4:443", collectorA, "s1", base, 10),
		okMeasurement("1.2.3.4:443", collectorA, "s1", base.Add(time.Hour), 20),
		failMeasurement("1.2.3.4:443", collectorA, "s1", base.Add(2*time.Hour), probe.ErrorTypeTimeout),
	)
	// 目标 B：采集者 B 成功一次
	batch = append(batch, okMeasurement("5.6.7.8:443", collectorB, "s2", base.Add(3*time.Hour), 30))

	if _, _, err := store.SaveMeasurements(ctx, batch); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query MeasurementQuery
		want  int
	}{
		{"all", MeasurementQuery{}, 4},
		{"by target", MeasurementQuery{TargetID: "1.2.3.4:443"}, 3},
		{"by collector", MeasurementQuery{CollectorPK: collectorB}, 1},
		{"by session", MeasurementQuery{SessionID: "s1"}, 3},
		{"success only", MeasurementQuery{SuccessOnly: true}, 3},
		{"failed only", MeasurementQuery{FailedOnly: true}, 1},
		{"time window", MeasurementQuery{Since: base.Add(30 * time.Minute), Until: base.Add(90 * time.Minute)}, 1},
		{"limit", MeasurementQuery{Limit: 2}, 2},
		{"combined", MeasurementQuery{TargetID: "1.2.3.4:443", SuccessOnly: true}, 2},
		{"no match", MeasurementQuery{TargetID: "9.9.9.9:443"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.QueryMeasurements(ctx, tc.query)
			if err != nil {
				t.Fatalf("QueryMeasurements: %v", err)
			}
			if len(rows) != tc.want {
				t.Errorf("rows = %d, want %d", len(rows), tc.want)
			}

			// Count 必须与查询结果一致。
			n, err := store.CountMeasurementsByQuery(ctx, tc.query)
			if err != nil {
				t.Fatalf("CountMeasurementsByQuery: %v", err)
			}
			if tc.query.Limit > 0 {
				// 带 LIMIT 时 COUNT 不受影响，因此只检查 >= 结果数。
				if n < int64(len(rows)) {
					t.Errorf("count = %d < rows = %d", n, len(rows))
				}
				return
			}
			if n != int64(len(rows)) {
				t.Errorf("count = %d, want %d", n, len(rows))
			}
		})
	}

	// 互斥条件必须报错，而不是返回空结果让调用方困惑。
	if _, err := store.QueryMeasurements(ctx, MeasurementQuery{SuccessOnly: true, FailedOnly: true}); err == nil {
		t.Error("QueryMeasurements accepted mutually exclusive filters")
	}

	// 倒序。
	desc, err := store.QueryMeasurements(ctx, MeasurementQuery{OrderDesc: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(desc) != 1 || !desc[0].Timestamp.Equal(base.Add(3*time.Hour)) {
		t.Errorf("OrderDesc returned %+v, want the newest measurement", desc)
	}
}

// ---------------------------------------------------------------------------
// 统计
// ---------------------------------------------------------------------------

func TestCollectStats(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	empty, err := store.CollectStats(ctx)
	if err != nil {
		t.Fatalf("CollectStats on empty db: %v", err)
	}
	if empty.SchemaVersion != ExpectedSchemaVersion() {
		t.Errorf("SchemaVersion = %d, want %d", empty.SchemaVersion, ExpectedSchemaVersion())
	}
	for table, n := range empty.Tables {
		if n != 0 {
			t.Errorf("table %q has %d rows in a fresh database", table, n)
		}
	}
	if !empty.FirstMeasurement.IsZero() || !empty.LastMeasurement.IsZero() {
		t.Error("empty database should report zero time range")
	}

	collectorPK := mustCollector(t, store, "c-stats", testProfile())
	if _, err := store.UpsertTargets(ctx, []model.Target{
		testTarget("1.2.3.4", 443),
		testTarget("5.6.7.8", 443),
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	if _, _, err := store.SaveMeasurements(ctx, []Measurement{
		okMeasurement("1.2.3.4:443", collectorPK, "", base, 10),
		okMeasurement("1.2.3.4:443", collectorPK, "", base.Add(time.Hour), 20),
		failMeasurement("5.6.7.8:443", collectorPK, "", base.Add(2*time.Hour), probe.ErrorTypeTimeout),
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := store.CollectStats(ctx)
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	if stats.Tables["targets"] != 2 {
		t.Errorf("targets = %d, want 2", stats.Tables["targets"])
	}
	if stats.Tables["measurements"] != 3 {
		t.Errorf("measurements = %d, want 3", stats.Tables["measurements"])
	}
	if stats.Tables["collectors"] != 1 {
		t.Errorf("collectors = %d, want 1", stats.Tables["collectors"])
	}
	if !stats.FirstMeasurement.Equal(base) {
		t.Errorf("FirstMeasurement = %s, want %s", stats.FirstMeasurement, base)
	}
	if !stats.LastMeasurement.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("LastMeasurement = %s, want %s", stats.LastMeasurement, base.Add(2*time.Hour))
	}
	if stats.DistinctTargets != 2 {
		t.Errorf("DistinctTargets = %d, want 2", stats.DistinctTargets)
	}
	if stats.DistinctCollectors != 1 {
		t.Errorf("DistinctCollectors = %d, want 1", stats.DistinctCollectors)
	}
}

func TestCollectStatsReportsFileSize(t *testing.T) {
	store := newFileStore(t)

	stats, err := store.CollectStats(context.Background())
	if err != nil {
		t.Fatalf("CollectStats: %v", err)
	}
	// 文件数据库的库文件至少有一个 SQLite 头（100 字节）。
	if stats.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want > 0 for a file database", stats.SizeBytes)
	}
}

// ---------------------------------------------------------------------------
// 与 probe 的集成
// ---------------------------------------------------------------------------

// TestStoreProbeResults 验证 probe 的真实结果能完整落库并读回。
//
// 这是 Phase 3 与 Phase 4 之间的契约测试：
// probe 的结果结构变化、或 storage 的列变化，都会在这里暴露。
func TestStoreProbeResults(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	collectorPK := mustCollector(t, store, "c-probe-integration", testProfile())
	targets := []model.Target{
		testTarget("1.2.3.4", 443),
		testTarget("1.2.3.4", 2053),
		testTarget("2001:db8::1", 8443),
	}
	if _, err := store.UpsertTargets(ctx, targets, time.Now()); err != nil {
		t.Fatal(err)
	}

	mustSession(t, store, "session-1", collectorPK, len(targets))

	started := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	results := []probe.ProbeResult{
		{TargetID: "1.2.3.4:443", IP: "1.2.3.4", Port: 443, Success: true, LatencyMS: 42.3, Timestamp: started},
		{TargetID: "1.2.3.4:2053", IP: "1.2.3.4", Port: 2053, Success: false, LatencyMS: 3000, ErrorType: probe.ErrorTypeTimeout, ErrorMessage: "dial tcp: i/o timeout", Timestamp: started.Add(time.Second)},
		{TargetID: "[2001:db8::1]:8443", IP: "2001:db8::1", Port: 8443, Success: false, ErrorType: probe.ErrorTypeNetworkUnreachable, ErrorMessage: "network is unreachable", Timestamp: started.Add(2 * time.Second)},
	}

	measurements := make([]Measurement, 0, len(results))
	for _, r := range results {
		if err := r.Valid(); err != nil {
			t.Fatalf("probe result invalid: %v", err)
		}
		measurements = append(measurements, NewMeasurement(collectorPK, "session-1", r))
	}

	saved, skipped, err := store.SaveMeasurements(ctx, measurements)
	if err != nil {
		t.Fatalf("SaveMeasurements: %v", err)
	}
	if saved != 3 || skipped != 0 {
		t.Fatalf("saved/skipped = %d/%d, want 3/0", saved, skipped)
	}

	rows, err := store.QueryMeasurements(ctx, MeasurementQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("measurements = %d, want 3", len(rows))
	}

	// 延迟必须精确往返（微秒存储 -> 毫秒读出）。
	if rows[0].LatencyMS != 42.3 {
		t.Errorf("LatencyMS = %v, want 42.3", rows[0].LatencyMS)
	}
	// 失败分类必须原样保留。
	if rows[1].ErrorType != probe.ErrorTypeTimeout {
		t.Errorf("ErrorType = %q, want timeout", rows[1].ErrorType)
	}
	if rows[2].ErrorType != probe.ErrorTypeNetworkUnreachable {
		t.Errorf("ErrorType = %q, want network_unreachable", rows[2].ErrorType)
	}
	// IPv6 目标的 ID 必须保留方括号形式。
	if rows[2].TargetID != "[2001:db8::1]:8443" {
		t.Errorf("TargetID = %q, want [2001:db8::1]:8443", rows[2].TargetID)
	}
	// 版本信息必须落库，便于日后按版本区分样本。
	if rows[0].SchemaVersion != schemaVersionForMeasurements || rows[0].ClientVersion == "" {
		t.Errorf("versions = %d/%q, want %d and a non-empty client version",
			rows[0].SchemaVersion, rows[0].ClientVersion, schemaVersionForMeasurements)
	}
}

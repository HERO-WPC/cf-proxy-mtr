package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// TestExportStreamsExecute 确认导出查询真的能跑通（列名与 JOIN 正确）。
// 这些 SQL 有 30 多个列与 3 张表的 JOIN，写错列名只有在执行时才会暴露。
func TestExportStreamsExecute(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pk := mustCollector(t, store, "c-export-exec", model.CollectorProfile{Country: "CN", Province: "Zhejiang", City: "Hangzhou", ISP: "China Mobile", ASN: "AS9808"})
	sessionID := "20260101T000000Z-00000000"
	if err := store.DefineSession(ctx, sessionID, pk, 2, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	target, err := model.NewTargetFromStrings("45.63.67.144", 2053)
	if err != nil {
		t.Fatal(err)
	}
	target.Location = model.Location{
		Country: "US", CCA2: "US", Region: "California", City: "San Jose",
		Latitude: 37.3382, Longitude: -121.8863, HasCoordinates: true, CountryEN: "United States",
	}
	target.Colo = model.ColoInfo{
		IATA: "SJC", CCA2: "US", Region: "California", City: "San Jose",
		Latitude: 37.36, Longitude: -121.92, HasCoordinates: true,
	}
	if _, err := store.UpsertTargets(ctx, []model.Target{target}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.SaveMeasurements(ctx, []Measurement{{
		TargetID: target.ID, CollectorID: pk, SessionID: sessionID,
		Timestamp: time.Now().UTC(), Success: true, LatencyMS: 42.5,
	}}); err != nil {
		t.Fatal(err)
	}

	// 跟踪结果
	tr := Trace{
		TargetID: target.ID, CollectorID: pk, SessionID: sessionID,
		Timestamp: time.Now().UTC(), Engine: "nexttrace", EngineVersion: "1.7.3",
		Mode: "icmp", Protocol: "icmp", Port: 2053,
		Success: true, DurationMS: 1200, HopCount: 2,
		TraceJSON: `[{"TTL":1,"IP":"192.168.1.1"}]`,
	}
	if _, _, err := store.SaveTraces(ctx, []Trace{tr}); err != nil {
		t.Fatal(err)
	}

	// 测量导出
	filter := ExportFilter{CollectorPK: pk, SessionID: sessionID}
	var got ExportMeasurement
	n, err := store.StreamMeasurements(ctx, filter, func(m ExportMeasurement) error {
		got = m
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMeasurements: %v", err)
	}
	if n != 1 {
		t.Fatalf("measurements = %d, want 1", n)
	}
	if got.IP != "45.63.67.144" || got.Port != 2053 {
		t.Errorf("target meta = %s:%d", got.IP, got.Port)
	}
	if got.City != "San Jose" || got.Region != "California" || got.CCA2 != "US" {
		t.Errorf("location = %+v", got)
	}
	if got.Latitude == nil || got.Longitude == nil || *got.Latitude != 37.3382 {
		t.Errorf("coords = %v,%v", got.Latitude, got.Longitude)
	}
	if got.ColoIATA != "SJC" {
		t.Errorf("colo = %q", got.ColoIATA)
	}
	if got.CollectorAID != "c-export-exec" {
		t.Errorf("collector = %q", got.CollectorAID)
	}
	if got.LatencyMS != 42.5 {
		t.Errorf("latency = %v", got.LatencyMS)
	}

	// 跟踪导出
	var gott ExportTrace
	nt, err := store.StreamTraces(ctx, filter, func(x ExportTrace) error {
		gott = x
		return nil
	})
	if err != nil {
		t.Fatalf("StreamTraces: %v", err)
	}
	if nt != 1 {
		t.Fatalf("traces = %d, want 1", nt)
	}
	if gott.EngineVersion != "1.7.3" || gott.HopCount != 2 || gott.Port != 2053 {
		t.Errorf("trace = %+v", gott)
	}
	if gott.TraceJSON == "" {
		t.Error("trace_json is empty")
	}

	// 会话列表
	sessions, err := store.ListSessions(ctx, pk)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	s := sessions[0]
	fmt.Printf("session %s measured=%d traced=%d finished=%v\n", s.ID, s.MeasuredTargets, s.TracedTargets, s.Finished())
	if s.MeasuredTargets != 1 || s.TracedTargets != 1 {
		t.Errorf("session counts = %d/%d, want 1/1", s.MeasuredTargets, s.TracedTargets)
	}

	// 过滤条件必须真的生效
	n2, err := store.StreamMeasurements(ctx, ExportFilter{SessionID: "no-such-session"}, func(ExportMeasurement) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("session filter ignored: got %d rows", n2)
	}
}

// TestExportStreamsIncludePrivateTargets 验证**数据库层不过滤**私有目标。
//
// 这是一个刻意的分工：本地库要保留用户自己的测试数据（例如他用
// 本机监听验证链路时产生的目标），过滤发生在导出层。
// 如果 storage 层就把它们藏起来，用户在本地也看不到自己测过什么。
//
// 这条测试同时防止一种危险的误解：以为"storage 已经过滤过了"，
// 于是在导出层省掉过滤——那会直接把内网地址写进公开数据。
func TestExportStreamsIncludePrivateTargets(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pk := mustCollector(t, store, "c-private-target", model.CollectorProfile{Country: "CN"})
	sessionID := "20260101T000000Z-00000000"
	if err := store.DefineSession(ctx, sessionID, pk, 1, "0.1.0", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	private, err := model.NewTargetFromStrings("192.168.1.215", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTargets(ctx, []model.Target{private}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SaveMeasurements(ctx, []Measurement{{
		TargetID: private.ID, CollectorID: pk, SessionID: sessionID,
		Timestamp: time.Now().UTC(), Success: true, LatencyMS: 1.2,
	}}); err != nil {
		t.Fatal(err)
	}

	// storage 层必须**照常**返回这一行。
	var got ExportMeasurement
	count, err := store.StreamMeasurements(ctx, ExportFilter{SessionID: sessionID}, func(m ExportMeasurement) error {
		got = m
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMeasurements: %v", err)
	}
	if count != 1 {
		t.Fatalf("storage returned %d rows, want 1 (filtering is the export layer's job)", count)
	}
	if got.IP != "192.168.1.215" {
		t.Errorf("IP = %q, want the private address preserved in the local database", got.IP)
	}
}

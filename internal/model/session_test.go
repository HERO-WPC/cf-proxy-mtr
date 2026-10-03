package model

import (
	"strings"
	"testing"
	"time"
)

func TestNewSessionID(t *testing.T) {
	started := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	const n = 32
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := NewSessionID(started)
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if !IsSessionID(id) {
			t.Fatalf("generated id %q is not valid", id)
		}
		// 同一秒内连续生成也不能撞 ID：随机后缀就是为了这个。
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate session id generated in the same second: %q", id)
		}
		seen[id] = struct{}{}

		// 前缀必须是可读的 UTC 时间，便于按时间归档与人工识别。
		if !strings.HasPrefix(id, "20261003T100000Z-") {
			t.Fatalf("id %q does not carry the expected UTC prefix", id)
		}
	}
}

func TestSessionIDIsTimezoneStable(t *testing.T) {
	// 同一时刻在不同时区表示下必须生成相同的时间前缀，
	// 否则不同地区的采集者会产生无法按时间排序的 ID。
	utc := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	shanghai := utc.In(time.FixedZone("CST", 8*3600))

	idUTC, err := NewSessionID(utc)
	if err != nil {
		t.Fatal(err)
	}
	idLocal, err := NewSessionID(shanghai)
	if err != nil {
		t.Fatal(err)
	}

	prefixUTC := strings.Split(idUTC, "-")[0]
	prefixLocal := strings.Split(idLocal, "-")[0]
	if prefixUTC != prefixLocal {
		t.Errorf("time prefixes differ: %q vs %q (must be normalized to UTC)", prefixUTC, prefixLocal)
	}
}

func TestIsSessionID(t *testing.T) {
	valid := "20260101T000000Z-00000000"
	if !IsSessionID(valid) {
		t.Errorf("IsSessionID(%q) = false, want true", valid)
	}

	invalid := map[string]string{
		"empty":            "",
		"no separator":     "20261003T100000Z3f9a1c2d",
		"too many dashes":  "20260101T000000Z-00000000-extra",
		"bad time":         "20260101T000000Z-00000000", // 13 月
		"short time":       "20261003T1000Z-3f9a1c2d",
		"short suffix":     "20261003T100000Z-3f9a",
		"long suffix":      "20260101T000000Z-0000000000",
		"uppercase suffix": "20261003T100000Z-3F9A1C2D",
		"non hex suffix":   "20261003T100000Z-3f9a1c2g",
		"not a date":       "helloworld1234567-3f9a1c2d",
	}
	for name, in := range invalid {
		t.Run(name, func(t *testing.T) {
			if IsSessionID(in) {
				t.Errorf("IsSessionID(%q) = true, want false", in)
			}
		})
	}
}

func TestMeasurementSessionLifecycle(t *testing.T) {
	started := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	session := MeasurementSession{
		ID:            "20260101T000000Z-00000000",
		CollectorPK:   7,
		StartedAt:     started,
		TargetCount:   100,
		ClientVersion: "0.1.0",
	}

	if session.Finished() {
		t.Error("Finished() = true for a session without FinishedAt")
	}
	if got := session.Remaining(); got != 100 {
		t.Errorf("Remaining() = %d, want 100", got)
	}

	// 未结束时以 end 为截止。
	if got := session.Duration(started.Add(30 * time.Minute)); got != 30*time.Minute {
		t.Errorf("Duration() = %s, want 30m", got)
	}
	// end 早于 started 时返回 0，而不是负数。
	if got := session.Duration(started.Add(-time.Hour)); got != 0 {
		t.Errorf("Duration() = %s, want 0 for an end before the start", got)
	}

	session.CompletedCount = 40
	if got := session.Remaining(); got != 60 {
		t.Errorf("Remaining() = %d, want 60", got)
	}

	// 完成数超过计划数时不能返回负数。
	session.CompletedCount = 150
	if got := session.Remaining(); got != 0 {
		t.Errorf("Remaining() = %d, want 0 when over-completed", got)
	}

	session.FinishedAt = started.Add(time.Hour)
	if !session.Finished() {
		t.Error("Finished() = false after setting FinishedAt")
	}
	// 结束后以 FinishedAt 为准，忽略传入的 end。
	if got := session.Duration(started.Add(10 * time.Hour)); got != time.Hour {
		t.Errorf("Duration() = %s, want 1h", got)
	}

	// 零值会话的 Duration 必须是 0（而不是 panic 或巨大值）。
	if got := (MeasurementSession{}).Duration(started); got != 0 {
		t.Errorf("Duration() = %s, want 0 for a zero session", got)
	}
}

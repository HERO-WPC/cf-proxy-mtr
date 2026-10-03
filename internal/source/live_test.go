package source

import (
	"testing"

	"github.com/cf-route-tester/cf-route-tester/internal/model"
)

// TestLiveCacheAgainstModel 用真实抓取到的缓存（data/all.json）跑一遍模型不变式。
//
// 缓存文件不存在时自动跳过，因此在没有联网抓取过的机器上也不会失败。
func TestLiveCacheAgainstModel(t *testing.T) {
	meta, _, targets, _, err := ReadCache("../../data/all.json")
	if err != nil {
		t.Skipf("live cache not available: %v", err)
	}
	if len(targets) == 0 {
		t.Skip("live cache is empty")
	}

	t.Logf("live cache: %d targets, source=%s format=%s", len(targets), meta.URL, meta.Format)

	var (
		notCanonical      int
		invalidCount      int
		withTargetCoords  int
		withColoCoords    int
		withColo          int
		cc2Mismatch       int
		ipv6              int
		firstInvalid      error
		firstNonCanonical string
	)

	for _, target := range targets {
		if err := target.Validate(); err != nil {
			invalidCount++
			if firstInvalid == nil {
				firstInvalid = err
			}
		}
		if target.Normalize() {
			notCanonical++
			if firstNonCanonical == "" {
				firstNonCanonical = target.ID
			}
		}
		if target.Location.HasCoordinates {
			withTargetCoords++
		}
		if !target.Colo.IsZero() {
			withColo++
		}
		if target.Colo.HasCoordinates {
			withColoCoords++
		}
		if target.Location.Country != "" && target.Colo.CCA2 != "" &&
			target.Location.Country != target.Colo.CCA2 {
			cc2Mismatch++
		}
		if target.IPVersion == model.IPVersionIPv6 {
			ipv6++
		}
	}

	if invalidCount != 0 {
		t.Errorf("%d/%d targets fail Validate, first error: %v", invalidCount, len(targets), firstInvalid)
	}
	if notCanonical != 0 {
		t.Errorf("%d/%d targets are not canonical, example: %s", notCanonical, len(targets), firstNonCanonical)
	}

	// 统计信息：这些数字应该与 Phase 1 的实测观察一致
	// （每个 IP:port 都有 meta 位置；colo 有少量缺失；约 21% 的国家代码不一致）。
	t.Logf("target coords: %d, colo present: %d, colo coords: %d, country!=colo.cca2: %d, ipv6: %d",
		withTargetCoords, withColo, withColoCoords, cc2Mismatch, ipv6)

	if withColo == 0 {
		t.Error("no target has colo information; parsing may be broken")
	}
	if cc2Mismatch == 0 {
		t.Error("expected some targets where meta.country != colo.cca2 (the two are different concepts)")
	}

	// 每个目标都必须能生成唯一的键，且键与 ID 一致。
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		key := model.TargetID(target.IP, target.Port)
		if key != target.ID {
			t.Fatalf("ID mismatch: %q vs %q", target.ID, key)
		}
		if _, dup := seen[key]; dup {
			t.Fatalf("duplicate target in cache: %s", key)
		}
		seen[key] = struct{}{}
	}
}

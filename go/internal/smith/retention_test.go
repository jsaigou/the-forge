// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
)

func seedFinding(t *testing.T, s *Smith, severity Severity, ageDays int, invID *int64) {
	t.Helper()
	at := s.d.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if _, err := s.persistFindings(context.Background(), []Finding{{
		CheckID: "test_check", Severity: severity, Summary: "x",
	}}, "manual", at, invID); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
}

func TestPruneOnce_SeverityTieredCutoffs(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	setSetting(t, db, SettingRetention, `{"enabled":true,"ok_days":7,"info_hours":30,"warn_crit_days":180,"web_cache_days":30,"web_cache_max_rows":2000}`)

	seedFinding(t, s, SeverityOK, 10, nil)   // older than ok_days(7) -> pruned
	seedFinding(t, s, SeverityOK, 1, nil)    // newer -> kept
	seedFinding(t, s, SeverityInfo, 40, nil) // older than info_hours(30) -> pruned
	seedFinding(t, s, SeverityInfo, 1, nil)  // kept
	seedFinding(t, s, SeverityWarn, 10, nil) // well under warn_crit_days(180) -> kept
	seedFinding(t, s, SeverityCrit, 10, nil) // kept

	res, err := s.pruneOnce(context.Background())
	if err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	if res.DeletedFindings != 2 {
		t.Errorf("deleted findings = %d, want 2 (one stale ok, one stale info)", res.DeletedFindings)
	}
	remaining, err := s.ListFindings(context.Background(), time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	if len(remaining) != 4 {
		t.Errorf("remaining findings = %d, want 4", len(remaining))
	}
}

func TestPruneOnce_InvestigationAttachedNeverPruned(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	setSetting(t, db, SettingRetention, `{"enabled":true,"ok_days":1,"info_hours":1,"warn_crit_days":1,"web_cache_days":1,"web_cache_max_rows":2000}`)

	invID, err := s.CreateInvestigation(context.Background(), "manual", "test investigation")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	// Every severity, all ancient, but attached to the investigation —
	// none of these may ever be pruned regardless of age.
	for _, sev := range []Severity{SeverityOK, SeverityInfo, SeverityWarn, SeverityCrit} {
		seedFinding(t, s, sev, 400, &invID)
	}
	// A standalone (non-attached) finding of the same age, for contrast —
	// this one SHOULD go.
	seedFinding(t, s, SeverityOK, 400, nil)

	res, err := s.pruneOnce(context.Background())
	if err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	if res.DeletedFindings != 1 {
		t.Errorf("deleted findings = %d, want exactly 1 (only the standalone one)", res.DeletedFindings)
	}
	_, findings, err := s.GetInvestigation(context.Background(), invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if len(findings) != 4 {
		t.Errorf("investigation findings = %d, want all 4 to survive", len(findings))
	}
}

func TestPruneOnce_ZeroDaysSkipsThatTierEntirely(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	setSetting(t, db, SettingRetention, `{"enabled":true,"ok_days":0,"info_hours":0,"warn_crit_days":0,"web_cache_days":0,"web_cache_max_rows":0}`)

	seedFinding(t, s, SeverityOK, 5, nil)
	seedFinding(t, s, SeverityInfo, 5, nil)
	seedFinding(t, s, SeverityWarn, 5, nil)
	seedFinding(t, s, SeverityCrit, 5, nil)

	res, err := s.pruneOnce(context.Background())
	if err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	if res.DeletedFindings != 0 {
		t.Errorf("deleted findings = %d, want 0 — a tier value <=0 must skip that tier, not delete everything", res.DeletedFindings)
	}
}

func TestPruneOnce_Disabled(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	setSetting(t, db, SettingRetention, `{"enabled":false,"ok_days":1,"info_hours":1,"warn_crit_days":1,"web_cache_days":1,"web_cache_max_rows":1}`)
	seedFinding(t, s, SeverityOK, 5000, nil)

	res, err := s.pruneOnce(context.Background())
	if err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	if res.DeletedFindings != 0 {
		t.Errorf("deleted findings = %d, want 0 when retention is disabled", res.DeletedFindings)
	}
}

func TestPruneOnce_WebCacheAgeAndRowCap(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	setSetting(t, db, SettingRetention, `{"enabled":true,"ok_days":0,"info_hours":0,"warn_crit_days":0,"web_cache_days":7,"web_cache_max_rows":2}`)

	cache := web.NewSQLCache(db)
	now := s.d.Now()
	// One old (age-pruned), three fresh (row-cap trims to 2 of the 3).
	mustPut(t, cache, "old", now.Add(-30*24*time.Hour))
	mustPut(t, cache, "fresh1", now.Add(-time.Hour))
	mustPut(t, cache, "fresh2", now.Add(-2*time.Hour))
	mustPut(t, cache, "fresh3", now.Add(-3*time.Hour))

	res, err := s.pruneOnce(context.Background())
	if err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	if res.DeletedWebCache != 2 { // 1 aged-out + 1 over the row cap
		t.Errorf("deleted web cache = %d, want 2", res.DeletedWebCache)
	}
	if _, ok := cache.Get(context.Background(), "search", "fresh1"); !ok {
		t.Error("fresh1 (most recent) should survive the row cap")
	}
	if _, ok := cache.Get(context.Background(), "search", "old"); ok {
		t.Error("old row should have been pruned by age")
	}
}

func mustPut(t *testing.T, cache web.Cache, key string, fetchedAt time.Time) {
	t.Helper()
	err := cache.Put(context.Background(), web.CacheEntry{
		Kind: "search", Key: key, Provider: "test", Body: "{}",
		FetchedAt: fetchedAt, ExpiresAt: fetchedAt.Add(365 * 24 * time.Hour), // TTL far in the future — only age/row-cap should prune these
	})
	if err != nil {
		t.Fatalf("cache.Put(%s): %v", key, err)
	}
}

func TestMaybePrune_RespectsInterval(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	base := s.d.Now()

	s.maybePrune(context.Background(), base)
	waitFor(t, time.Second, func() bool {
		s.pruneMu.Lock()
		defer s.pruneMu.Unlock()
		return !s.lastPruneAt.IsZero()
	})
	s.pruneMu.Lock()
	first := s.lastPruneAt
	s.pruneMu.Unlock()

	// Well within the interval — must not fire again.
	s.maybePrune(context.Background(), base.Add(time.Minute))
	time.Sleep(20 * time.Millisecond)
	s.pruneMu.Lock()
	second := s.lastPruneAt
	s.pruneMu.Unlock()
	if !second.Equal(first) {
		t.Errorf("lastPruneAt changed within the interval: %v -> %v", first, second)
	}
}

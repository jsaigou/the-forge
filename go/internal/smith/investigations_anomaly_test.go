// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"
)

// TestReconcileOpenAnomalies_RehydratesDedupeMapFromStore is the regression
// test for the 2026-08-19 live finding: openAnomaly starts empty on every
// New() call, so a daemon restart used to forget which anomaly
// investigations were still open — the next occurrence of an already-open
// code opened a duplicate instead of reusing it. reconcileOpenAnomalies
// (called from Start, mirroring reconcileExecuting) must repopulate the map
// from the store before the anomaly hook starts listening.
func TestReconcileOpenAnomalies_RehydratesDedupeMapFromStore(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}

	// Simulate a fresh process: openAnomaly is empty even though invID is
	// still open in the store (exactly the state after a daemon restart).
	if len(s.openAnomaly) != 0 {
		t.Fatalf("openAnomaly should start empty, got %v", s.openAnomaly)
	}

	s.reconcileOpenAnomalies(ctx)

	s.mu.Lock()
	got, ok := s.openAnomaly["GTT_DRAIN_TIMEOUT"]
	s.mu.Unlock()
	if !ok {
		t.Fatal("reconcileOpenAnomalies did not rehydrate GTT_DRAIN_TIMEOUT")
	}
	if got != invID {
		t.Errorf("openAnomaly[GTT_DRAIN_TIMEOUT] = %d, want %d", got, invID)
	}
}

// TestReconcileOpenAnomalies_PicksMostRecentOnDuplicates covers the
// pre-existing-duplicates case (the real state found live: 11 open
// GTT_DRAIN_TIMEOUT investigations from before this fix existed). The most
// recently opened one becomes canonical; older duplicates are left alone —
// reconcileOpenAnomalies must never resolve/touch investigation rows, only
// populate the in-memory map.
func TestReconcileOpenAnomalies_PicksMostRecentOnDuplicates(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	ctx := context.Background()

	older, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "")
	if err != nil {
		t.Fatalf("CreateInvestigation (older): %v", err)
	}
	forceInvestigationOpenedAt(t, s, older, now.Add(-2*time.Hour))

	newer, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "")
	if err != nil {
		t.Fatalf("CreateInvestigation (newer): %v", err)
	}
	forceInvestigationOpenedAt(t, s, newer, now.Add(-1*time.Hour))

	s.reconcileOpenAnomalies(ctx)

	s.mu.Lock()
	got := s.openAnomaly["GTT_DRAIN_TIMEOUT"]
	s.mu.Unlock()
	if got != newer {
		t.Errorf("openAnomaly[GTT_DRAIN_TIMEOUT] = %d, want the more recent investigation %d (older duplicate %d must be left as-is)", got, newer, older)
	}

	older_, _, err := s.GetInvestigation(ctx, older)
	if err != nil {
		t.Fatalf("GetInvestigation(older): %v", err)
	}
	if older_.Status != "open" {
		t.Errorf("older duplicate status = %s, want still open — reconcile must never resolve investigations", older_.Status)
	}
}

// TestReconcileOpenAnomalies_DoesNotOverwriteAlreadyPopulatedEntry ensures
// reconcile is a pure "fill gaps" operation, never clobbering an entry a
// live handleAnomaly call already set (defensive: reconcile only runs once
// at Start, before startAnomalyHook subscribes, but the code itself should
// not assume ordering to stay correct).
func TestReconcileOpenAnomalies_DoesNotOverwriteAlreadyPopulatedEntry(t *testing.T) {
	db := openDB(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s := New(Deps{Store: db, Settings: db.Settings(), Now: fixedNow(now), Logf: func(string, ...any) {}})
	ctx := context.Background()

	invID, err := s.CreateInvestigation(ctx, "anomaly:GTT_DRAIN_TIMEOUT", "")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}

	s.mu.Lock()
	s.openAnomaly["GTT_DRAIN_TIMEOUT"] = 999999 // sentinel, not invID
	s.mu.Unlock()

	s.reconcileOpenAnomalies(ctx)

	s.mu.Lock()
	got := s.openAnomaly["GTT_DRAIN_TIMEOUT"]
	s.mu.Unlock()
	if got != 999999 {
		t.Errorf("reconcile overwrote an already-populated entry: got %d, want sentinel 999999 preserved", got)
	}
	_ = invID
}

func TestRelevantWarnCritCheckIDs_NarrowsKnownAnomalyCode(t *testing.T) {
	findings := []StoredFinding{
		{CheckID: "slot_agreement", Severity: SeverityWarn},
		{CheckID: "brain_resolvable", Severity: SeverityWarn},
		{CheckID: "gpu_hang", Severity: SeverityCrit},
	}
	got := relevantWarnCritCheckIDs("anomaly:GTT_DRAIN_TIMEOUT", findings)
	want := map[string]bool{"slot_agreement": true, "gpu_hang": true}
	if len(got) != len(want) {
		t.Fatalf("relevantWarnCritCheckIDs = %v, want exactly %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected check_id %q survived narrowing for GTT_DRAIN_TIMEOUT", id)
		}
	}
}

func TestRelevantWarnCritCheckIDs_UnmappedCodeFallsBackToFullSet(t *testing.T) {
	findings := []StoredFinding{
		{CheckID: "slot_agreement", Severity: SeverityWarn},
		{CheckID: "disk_space", Severity: SeverityWarn},
	}
	// UNIT_OOM has no curated entry in anomalyRelevantChecks — must fall
	// back to the full unnarrowed set rather than silently dropping checks
	// for a code with no evidenced mapping.
	got := relevantWarnCritCheckIDs("anomaly:UNIT_OOM", findings)
	if len(got) != 2 {
		t.Errorf("relevantWarnCritCheckIDs for an unmapped code = %v, want the full unnarrowed set of 2", got)
	}
}

func TestRelevantWarnCritCheckIDs_ManualTriggerFallsBackToFullSet(t *testing.T) {
	findings := []StoredFinding{
		{CheckID: "slot_agreement", Severity: SeverityWarn},
		{CheckID: "brain_resolvable", Severity: SeverityWarn},
	}
	got := relevantWarnCritCheckIDs("manual", findings)
	if len(got) != 2 {
		t.Errorf("relevantWarnCritCheckIDs for a manual trigger = %v, want the full unnarrowed set of 2", got)
	}
}

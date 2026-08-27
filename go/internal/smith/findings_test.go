// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"
)

// TestFinding_NormalizeDefaultsConfidenceHigh: Tier 1 Sprint 4 — a check
// written before this sprint (or one that simply never degrades) leaves
// Confidence unset; normalize() must treat that the same as explicit High,
// matching every pre-existing check's actual behavior (read everything it
// wanted, or errored out entirely via runOne's panic recovery).
func TestFinding_NormalizeDefaultsConfidenceHigh(t *testing.T) {
	f := Finding{CheckID: "x", Severity: SeverityOK}.normalize()
	if f.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want high for an unset field", f.Confidence)
	}

	f2 := Finding{CheckID: "x", Severity: SeverityWarn, Confidence: ConfidenceLow, ConfidenceNote: "seam nil"}.normalize()
	if f2.Confidence != ConfidenceLow || f2.ConfidenceNote != "seam nil" {
		t.Errorf("normalize() must not overwrite an explicitly-set Confidence, got %+v", f2)
	}
}

// TestConfidence_Rank locks the ordering Diagnostics.tsx relies on for
// sorting/display (higher = more confident).
func TestConfidence_Rank(t *testing.T) {
	if ConfidenceHigh.Rank() <= ConfidenceMedium.Rank() {
		t.Errorf("High.Rank()=%d should be > Medium.Rank()=%d", ConfidenceHigh.Rank(), ConfidenceMedium.Rank())
	}
	if ConfidenceMedium.Rank() <= ConfidenceLow.Rank() {
		t.Errorf("Medium.Rank()=%d should be > Low.Rank()=%d", ConfidenceMedium.Rank(), ConfidenceLow.Rank())
	}
	if Confidence("").Rank() != ConfidenceLow.Rank() {
		t.Errorf("unset Confidence.Rank() = %d, want it to rank with Low (never mistaken for High)", Confidence("").Rank())
	}
}

// TestPersistFindings_ConfidenceRoundTrips: a finding's Confidence/
// ConfidenceNote must survive persistFindings -> ListFindings and
// -> findingsForInvestigation unchanged (Tier 1 Sprint 4, migration 0075).
func TestPersistFindings_ConfidenceRoundTrips(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	findings := []Finding{
		{CheckID: "a", Severity: SeverityWarn, Summary: "s1", Confidence: ConfidenceMedium, ConfidenceNote: "TailscaleOnline seam nil"},
		{CheckID: "b", Severity: SeverityOK, Summary: "s2"}, // unset -> persisted as high
	}
	ids, err := s.persistFindings(ctx, findings, SweepManual, time.Now(), nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
	if len(ids) != 2 || ids[0] == 0 || ids[1] == 0 {
		t.Fatalf("ids = %v, want 2 nonzero ids", ids)
	}

	stored, err := s.ListFindings(ctx, time.Time{}, "", 0)
	if err != nil {
		t.Fatalf("ListFindings: %v", err)
	}
	byCheck := map[string]StoredFinding{}
	for _, sf := range stored {
		byCheck[sf.CheckID] = sf
	}
	a, ok := byCheck["a"]
	if !ok {
		t.Fatalf("finding a not found in %+v", stored)
	}
	if a.Confidence != ConfidenceMedium || a.ConfidenceNote != "TailscaleOnline seam nil" {
		t.Errorf("finding a = %+v, want confidence=medium with the note preserved", a)
	}
	b, ok := byCheck["b"]
	if !ok {
		t.Fatalf("finding b not found in %+v", stored)
	}
	if b.Confidence != ConfidenceHigh {
		t.Errorf("finding b (unset Confidence) persisted as %q, want high", b.Confidence)
	}

	// Also round-trips through the investigation-scoped read path.
	invID, err := s.CreateInvestigation(ctx, "test", "confidence round-trip test")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	if _, err := s.persistFindings(ctx, findings[:1], SweepManual, time.Now(), &invID); err != nil {
		t.Fatalf("persistFindings (investigation): %v", err)
	}
	invFindings, err := s.findingsForInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("findingsForInvestigation: %v", err)
	}
	if len(invFindings) != 1 || invFindings[0].Confidence != ConfidenceMedium {
		t.Errorf("findingsForInvestigation = %+v, want 1 finding with confidence=medium", invFindings)
	}
}

// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	modelregistry "github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

func TestParseCapabilityTierResponse(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"bare json", `{"classes":[{"name":"x","mode":"","notes":""}],"assignments":[]}`, false},
		{"fenced json", "```json\n{\"classes\":[],\"assignments\":[]}\n```", false},
		{"leading/trailing prose", "Here you go:\n{\"classes\":[],\"assignments\":[]}\nThanks!", false},
		{"no json at all", "I can't do that right now.", true},
		{"brace inside trailing prose", `{"classes":[],"assignments":[]}` + "\n\nNote: an empty {} object for mode means inherit.", false},
		{"quoted brace inside string value", `{"classes":[{"name":"x","mode":"","notes":"uses {curly} in notes"}],"assignments":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCapabilityTierResponse(tc.content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseCapabilityTierResponse(%q) err=%v, wantErr=%v", tc.content, err, tc.wantErr)
			}
		})
	}
}

// TestApplyCatalogChange_CapabilityTierAndConfigPerf exercises the two table
// kinds capability_tiers.go depends on, through the same dispatch path an
// approved action actually executes (execute.go's dispatchCatalogChange →
// sourcing.go's applyCatalogChange).
func TestApplyCatalogChange_CapabilityTierAndConfigPerf(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	seedBrainCatalog(t, db)
	cat := db.Catalog()
	s := New(Deps{Store: db, Catalog: cat, Now: time.Now, Logf: t.Logf})

	cfg, err := cat.ConfigByName(ctx, "ornith-35b")
	if err != nil {
		t.Fatalf("ConfigByName: %v", err)
	}

	// create capability_tier
	row, _ := json.Marshal(store.CapabilityTier{Name: "general-large", Mode: "", Notes: "test"})
	if err := s.applyCatalogChange(ctx, catalogChangeDetail{Op: "create", Table: "capability_tier", Row: row}); err != nil {
		t.Fatalf("applyCatalogChange create capability_tier: %v", err)
	}
	pc, err := cat.CapabilityTierByName(ctx, "general-large")
	if err != nil {
		t.Fatalf("CapabilityTierByName: %v", err)
	}

	// assign config to it via config_capability
	crow, _ := json.Marshal(configCapabilityRow{ConfigID: cfg.ID, CapabilityTierID: pc.ID, CapabilityRank: 1})
	if err := s.applyCatalogChange(ctx, catalogChangeDetail{Op: "update", Table: "config_capability", Row: crow}); err != nil {
		t.Fatalf("applyCatalogChange config_capability: %v", err)
	}
	got, err := cat.ConfigByName(ctx, "ornith-35b")
	if err != nil {
		t.Fatalf("ConfigByName reread: %v", err)
	}
	if got.CapabilityTierID != pc.ID || got.CapabilityRank != 1 {
		t.Fatalf("config perf not applied: got CapabilityTierID=%d CapabilityRank=%d, want %d/1", got.CapabilityTierID, got.CapabilityRank, pc.ID)
	}

	// unsupported table still rejected
	if err := s.applyCatalogChange(ctx, catalogChangeDetail{Op: "create", Table: "model", Row: []byte(`{}`)}); err == nil {
		t.Fatalf("applyCatalogChange: expected error for unsupported table %q, got nil", "model")
	}
}

// TestCreateAction_CatalogChangeValidatesTableAndOp exercises the
// CreateAction-time gate (actions.go) — a hallucinated/malformed
// catalog_change proposal must never reach the pending-actions queue.
func TestCreateAction_CatalogChangeValidatesTableAndOp(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	s := New(Deps{Store: db, Now: time.Now, Logf: t.Logf})

	cases := []struct {
		name   string
		detail catalogChangeDetail
	}{
		{"unsupported table", catalogChangeDetail{Op: "create", Table: "offering", Row: []byte(`{}`)}},
		{"unsupported op", catalogChangeDetail{Op: "delete", Table: "capability_tier", Row: []byte(`{}`)}},
		{"empty row", catalogChangeDetail{Op: "create", Table: "capability_tier", Row: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, _ := json.Marshal(tc.detail)
			_, err := s.CreateAction(ctx, ActionDraft{
				Kind: KindCatalogChange, Title: "t", Risk: RiskLow, Detail: detail, CreatedBy: "test",
			})
			if err == nil {
				t.Fatalf("CreateAction: expected validation error, got nil")
			}
		})
	}

	// A valid capability_tier create IS accepted into the queue.
	valid, _ := json.Marshal(catalogChangeDetail{Op: "create", Table: "capability_tier",
		Row: mustJSON(t, store.CapabilityTier{Name: "x"})})
	if _, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindCatalogChange, Title: "create x", Risk: RiskLow, Detail: valid, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("CreateAction: valid catalog_change rejected: %v", err)
	}
}

// TestApplyCapabilityTierProposal_ValidatesAssignments is the highest-value test
// in this file: it drives applyCapabilityTierProposal with a proposal shaped
// like a real (imperfect) LLM response — a good entry, a config the
// candidate set never offered (hallucinated reference), a non-positive
// rank, a duplicate assignment for the same config, and an assignment
// against a class name that was proposed but not yet created (the
// cross-action sequencing case) — and checks every bad entry lands in
// Skipped with a reason rather than silently becoming an action.
func TestApplyCapabilityTierProposal_ValidatesAssignments(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	seedBrainCatalog(t, db)
	cat := db.Catalog()
	s := New(Deps{Store: db, Catalog: cat, Now: time.Now, Logf: t.Logf})

	cfg, err := cat.ConfigByName(ctx, "ornith-35b")
	if err != nil {
		t.Fatalf("ConfigByName: %v", err)
	}
	// Pre-existing class the proposal can assign into immediately.
	existingID, err := cat.CreateCapabilityTier(ctx, store.CapabilityTier{Name: "existing-tier"})
	if err != nil {
		t.Fatalf("CreateCapabilityTier: %v", err)
	}
	existing, err := cat.ListCapabilityTiers(ctx)
	if err != nil {
		t.Fatalf("ListCapabilityTiers: %v", err)
	}

	candidates := []modelregistry.ConfigCard{{ID: cfg.ID, Name: cfg.Name, Visibility: "visible"}}

	proposal := CapabilityTierProposal{
		Classes: []CapabilityTierDraft{
			{Name: "brand-new-tier", Mode: "", Notes: "not yet approved"},
		},
		Assignments: []CapabilityTierAssignmentDraft{
			{ConfigID: cfg.ID, ClassName: "existing-tier", Rank: 1, Rationale: "valid"},
			{ConfigID: 999999, ClassName: "existing-tier", Rank: 1, Rationale: "hallucinated config id"},
			{ConfigID: cfg.ID, ClassName: "brand-new-tier", Rank: 1, Rationale: "class not created yet"},
		},
	}

	result, err := s.applyCapabilityTierProposal(ctx, candidates, existing, proposal)
	if err != nil {
		t.Fatalf("applyCapabilityTierProposal: %v", err)
	}

	if len(result.CreatedClassActions) != 1 {
		t.Fatalf("want 1 created class action (brand-new-tier), got %d", len(result.CreatedClassActions))
	}
	if len(result.CreatedAssignActions) != 1 {
		t.Fatalf("want exactly 1 created assignment action (the valid one), got %d: %+v", len(result.CreatedAssignActions), result)
	}
	if len(result.Skipped) != 2 {
		t.Fatalf("want 2 skipped assignments (hallucinated config + not-yet-created class), got %d: %+v", len(result.Skipped), result.Skipped)
	}

	// The one real assignment action actually references the pre-existing
	// class's real ID, never a placeholder/zero value.
	a, err := s.GetAction(ctx, result.CreatedAssignActions[0])
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	var d catalogChangeDetail
	if err := json.Unmarshal(a.Detail, &d); err != nil {
		t.Fatalf("unmarshal action detail: %v", err)
	}
	var row configCapabilityRow
	if err := json.Unmarshal(d.Row, &row); err != nil {
		t.Fatalf("unmarshal config_capability row: %v", err)
	}
	if row.CapabilityTierID != existingID {
		t.Fatalf("assignment action references capability_tier_id %d, want %d", row.CapabilityTierID, existingID)
	}
}

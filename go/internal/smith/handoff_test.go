// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jsaigou/the-forge/internal/engine"
)

// ── self-eviction stamping table ────────────────────────────────────────────

func TestStampSelfEviction(t *testing.T) {
	ctx := context.Background()

	newSmithWithBrain := func(t *testing.T, slot string, placer Placer) *Smith {
		t.Helper()
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		return New(Deps{
			Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
			Sched: newStubSched(map[string]string{slot: "ornith-35b"}),
			Placer: func() Placer {
				if placer == nil {
					return nil
				}
				return placer
			}(),
		})
	}

	t.Run("brain remote -> never self-evicting", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"deepseek-chat"`)
		s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Placer: &stubPlacer{}})
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "ornith-35b", Slot: "a3"}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if a.SelfEvicting {
			t.Errorf("action = %+v, want not self-evicting when brain is remote", a)
		}
	})

	t.Run("brain deterministic_only -> never self-evicting", func(t *testing.T) {
		db := openDB(t)
		s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Placer: &stubPlacer{}})
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "ornith-35b", Slot: "a3"}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if a.SelfEvicting {
			t.Errorf("action = %+v, want not self-evicting with no resolvable brain", a)
		}
	})

	t.Run("explicit unload_slot targeting the brain's own slot -> self-evicting", func(t *testing.T) {
		s := newSmithWithBrain(t, "a2", &stubPlacer{})
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "op",
			Detail: mustJSON(t, unloadSlotDetail{Slot: "a2"}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if !a.SelfEvicting || a.Handoff == nil || a.Handoff.State != HandoffRequired {
			t.Errorf("action = %+v, want self-evicting with a required handoff", a)
		}
	})

	t.Run("unload_slot targeting a different slot -> not self-evicting, no FitPlan needed", func(t *testing.T) {
		placer := &stubPlacer{}
		s := newSmithWithBrain(t, "a2", placer)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindUnloadSlot, Risk: RiskHigh, CreatedBy: "op",
			Detail: mustJSON(t, unloadSlotDetail{Slot: "a3"}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if a.SelfEvicting {
			t.Errorf("action = %+v, want not self-evicting", a)
		}
	})

	t.Run("load_config FitPlan places on the brain's slot -> self-evicting", func(t *testing.T) {
		placer := &stubPlacer{plan: engine.Plan{Fits: true, Slot: "a2"}}
		s := newSmithWithBrain(t, "a2", placer)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "big-model", Slot: ""}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if !a.SelfEvicting || a.Handoff == nil || a.Handoff.State != HandoffRequired {
			t.Errorf("action = %+v, want self-evicting (plan.Slot == brain slot)", a)
		}
		var detail map[string]any
		_ = json.Unmarshal(a.Detail, &detail)
		if _, ok := detail["fit_plan"]; !ok {
			t.Errorf("detail = %s, want fit_plan stamped", a.Detail)
		}
	})

	t.Run("load_config FitPlan evicts the brain's slot -> self-evicting", func(t *testing.T) {
		placer := &stubPlacer{plan: engine.Plan{Fits: true, Slot: "a3", Evict: []string{"a4", "a2"}}}
		s := newSmithWithBrain(t, "a2", placer)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "big-model", Slot: ""}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if !a.SelfEvicting {
			t.Errorf("action = %+v, want self-evicting (brain slot in evict list)", a)
		}
	})

	t.Run("load_config FitPlan avoids the brain's slot -> not self-evicting, plan still stamped", func(t *testing.T) {
		placer := &stubPlacer{plan: engine.Plan{Fits: true, Slot: "a3"}}
		s := newSmithWithBrain(t, "a2", placer)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "small-model", Slot: ""}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if a.SelfEvicting || a.Handoff != nil {
			t.Errorf("action = %+v, want not self-evicting", a)
		}
		var detail map[string]any
		_ = json.Unmarshal(a.Detail, &detail)
		if _, ok := detail["fit_plan"]; !ok {
			t.Errorf("detail = %s, want fit_plan stamped even when safe", a.Detail)
		}
	})

	t.Run("nil Placer on implicit load_config -> conservative degrade, never 'safe'", func(t *testing.T) {
		s := newSmithWithBrain(t, "a2", nil)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "unknown-model", Slot: ""}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if !a.SelfEvicting {
			t.Fatalf("action = %+v, want self-evicting (Placer nil must never be treated as safe)", a)
		}
		if a.Risk != RiskHigh {
			t.Errorf("risk = %s, want forced to high", a.Risk)
		}
		var detail map[string]any
		_ = json.Unmarshal(a.Detail, &detail)
		fp, _ := detail["fit_plan"].(map[string]any)
		if fp == nil || fp["unknown"] != true {
			t.Errorf("detail.fit_plan = %v, want {unknown:true,...}", detail["fit_plan"])
		}
	})

	t.Run("FitPlan error on load_config -> conservative degrade, never 'safe'", func(t *testing.T) {
		placer := &stubPlacer{planErr: errors.New("boom")}
		s := newSmithWithBrain(t, "a2", placer)
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "unknown-model", Slot: ""}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		if !a.SelfEvicting || a.Risk != RiskHigh {
			t.Errorf("action = %+v, want self-evicting + risk forced high on FitPlan error", a)
		}
	})
}

// ── exhaustive handoff guardrail table ──────────────────────────────────────

// selfEvictingAction creates a load_config draft guaranteed to be
// self-evicting (explicit-slot match against the brain's own slot — needs
// no Placer) and returns the fully-wired Smith + the created action.
func selfEvictingAction(t *testing.T) (*Smith, *Action) {
	t.Helper()
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched: newStubSched(map[string]string{"a2": "ornith-35b"}),
		Placer: &stubPlacer{
			slotNames: []string{"a1", "a2", "a3", "a4"},
		},
	})
	a, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, loadConfigDetail{Mode: "big-model", Slot: "a2"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if !a.SelfEvicting {
		t.Fatalf("test setup bug: action should be self-evicting, got %+v", a)
	}
	return s, a
}

func TestHandoffGuardrailTable(t *testing.T) {
	ctx := context.Background()

	t.Run("required: approve is blocked with HandoffRequiredError", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		_, err := s.ApproveAction(ctx, a.ID, "op")
		var herr *HandoffRequiredError
		if !errors.As(err, &herr) {
			t.Fatalf("ApproveAction error = %v, want *HandoffRequiredError", err)
		}
		if herr.Handoff.State != HandoffRequired {
			t.Errorf("handoff state = %s, want required", herr.Handoff.State)
		}
	})

	t.Run("after runbook issuance, approve is STILL blocked", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		issued, err := s.ResolveHandoff(ctx, a.ID, "runbook", "op")
		if err != nil {
			t.Fatalf("ResolveHandoff(runbook): %v", err)
		}
		if issued.Handoff.State != HandoffRunbookIssued {
			t.Fatalf("handoff state = %s, want runbook_issued", issued.Handoff.State)
		}
		if len(issued.Handoff.Runbook) < 3 {
			t.Fatalf("runbook = %+v, want >= 3 steps", issued.Handoff.Runbook)
		}
		for i, step := range issued.Handoff.Runbook {
			if i == 1 {
				continue // step 2 (index 1) is legitimately informational
			}
			if step.Command == "" {
				t.Errorf("runbook step %d (%q) has an empty command", i, step.Title)
			}
		}

		_, err = s.ApproveAction(ctx, a.ID, "op")
		var herr *HandoffRequiredError
		if !errors.As(err, &herr) {
			t.Fatalf("ApproveAction after runbook error = %v, want *HandoffRequiredError (issuance alone doesn't unblock)", err)
		}
		if herr.Handoff.State != HandoffRunbookIssued {
			t.Errorf("handoff state = %s, want runbook_issued", herr.Handoff.State)
		}
	})

	t.Run("after acknowledge, approve succeeds", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		if _, err := s.ResolveHandoff(ctx, a.ID, "runbook", "op"); err != nil {
			t.Fatalf("ResolveHandoff(runbook): %v", err)
		}
		acked, err := s.ResolveHandoff(ctx, a.ID, "acknowledge", "operator2")
		if err != nil {
			t.Fatalf("ResolveHandoff(acknowledge): %v", err)
		}
		if acked.Handoff.State != HandoffAcknowledged {
			t.Fatalf("handoff state = %s, want acknowledged", acked.Handoff.State)
		}
		if acked.Handoff.AcknowledgedBy == nil || *acked.Handoff.AcknowledgedBy != "operator2" {
			t.Errorf("acknowledged_by = %v, want operator2", acked.Handoff.AcknowledgedBy)
		}

		approved, err := s.ApproveAction(ctx, a.ID, "operator2")
		if err != nil {
			t.Fatalf("ApproveAction after acknowledge: %v", err)
		}
		if approved.Status != StatusApproved {
			t.Errorf("status = %s, want approved", approved.Status)
		}
	})

	t.Run("acknowledge before runbook issuance is rejected", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		if _, err := s.ResolveHandoff(ctx, a.ID, "acknowledge", "op"); err == nil {
			t.Error("expected an error acknowledging a handoff that has no issued runbook yet")
		}
	})

	t.Run("runbook re-issuance after acknowledge is rejected (state must be required)", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		if _, err := s.ResolveHandoff(ctx, a.ID, "runbook", "op"); err != nil {
			t.Fatalf("ResolveHandoff(runbook): %v", err)
		}
		if _, err := s.ResolveHandoff(ctx, a.ID, "acknowledge", "op"); err != nil {
			t.Fatalf("ResolveHandoff(acknowledge): %v", err)
		}
		if _, err := s.ResolveHandoff(ctx, a.ID, "runbook", "op"); err == nil {
			t.Error("expected an error re-issuing a runbook once already acknowledged")
		}
	})

	t.Run("remote resolution with no healthy candidate is rejected", func(t *testing.T) {
		// selfEvictingAction wires no ProviderHealth, so the one candidate
		// probeHandoffCandidates finds (deepseek, off seedBrainCatalog's
		// offering sharing ornith-35b's Model) is real but Healthy:false.
		s, a := selfEvictingAction(t)
		if len(a.Handoff.Candidates) == 0 {
			t.Fatalf("test setup bug: expected at least one probed candidate, got %+v", a.Handoff)
		}
		for _, c := range a.Handoff.Candidates {
			if c.Healthy {
				t.Fatalf("test setup bug: expected no healthy candidates, got %+v", c)
			}
		}
		if _, err := s.ResolveHandoff(ctx, a.ID, "remote", "op"); err == nil {
			t.Error("expected an error resolving remote handoff with no healthy candidate")
		}
		// Untouched: still pending/required.
		after, err := s.GetAction(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetAction: %v", err)
		}
		if after.Status != StatusPending || after.Handoff.State != HandoffRequired {
			t.Errorf("action = %+v, want unchanged (pending/required)", after)
		}
	})

	t.Run("remote resolution swaps smith.model to a healthy candidate", func(t *testing.T) {
		db := openDB(t)
		seedBrainCatalog(t, db)
		setSetting(t, db, SettingModel, `"ornith-35b"`)
		s := New(Deps{
			Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
			Sched:  newStubSched(map[string]string{"a2": "ornith-35b"}),
			Placer: &stubPlacer{slotNames: []string{"a1", "a2", "a3", "a4"}},
			ProviderHealth: func(context.Context, string) (string, error) {
				return "reachable", nil
			},
		})
		a, err := s.CreateAction(ctx, ActionDraft{
			Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
			Detail: mustJSON(t, loadConfigDetail{Mode: "big-model", Slot: "a2"}),
		})
		if err != nil {
			t.Fatalf("CreateAction: %v", err)
		}
		var chosen Candidate
		for _, c := range a.Handoff.Candidates {
			if c.Healthy {
				chosen = c
			}
		}
		if chosen.Model == "" {
			t.Fatalf("test setup bug: expected a healthy candidate, got %+v", a.Handoff.Candidates)
		}

		updated, err := s.ResolveHandoff(ctx, a.ID, "remote", "op")
		if err != nil {
			t.Fatalf("ResolveHandoff(remote): %v", err)
		}
		if updated.Handoff.State != HandoffRemoteSwapped {
			t.Errorf("handoff state = %s, want %s", updated.Handoff.State, HandoffRemoteSwapped)
		}
		if updated.Handoff.AcknowledgedBy == nil || *updated.Handoff.AcknowledgedBy != "op" {
			t.Errorf("acknowledged_by = %v, want op", updated.Handoff.AcknowledgedBy)
		}

		raw, err := db.Settings().Get(ctx, SettingModel)
		if err != nil {
			t.Fatalf("read smith.model: %v", err)
		}
		var model string
		json.Unmarshal(raw, &model)
		if model != chosen.Model {
			t.Errorf("smith.model = %q, want %q (the candidate's wire_model)", model, chosen.Model)
		}

		// remote_swapped now unblocks approval, same as acknowledged.
		if _, err := s.ApproveAction(ctx, a.ID, "op"); err != nil {
			t.Errorf("ApproveAction after remote swap: %v", err)
		}
	})

	t.Run("cancel rejects the action outright", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		cancelled, err := s.ResolveHandoff(ctx, a.ID, "cancel", "op")
		if err != nil {
			t.Fatalf("ResolveHandoff(cancel): %v", err)
		}
		if cancelled.Status != StatusRejected {
			t.Errorf("status = %s, want rejected", cancelled.Status)
		}
	})

	t.Run("unknown resolution is a validation error", func(t *testing.T) {
		s, a := selfEvictingAction(t)
		if _, err := s.ResolveHandoff(ctx, a.ID, "teleport", "op"); err == nil {
			t.Error("expected a validation error for an unknown resolution")
		}
	})
}

func TestHandoffRequiredErrorMessage(t *testing.T) {
	err := &HandoffRequiredError{ActionID: 42, Handoff: Handoff{State: HandoffRequired}}
	if got := err.Error(); got == "" {
		t.Error("HandoffRequiredError.Error() returned empty string")
	}
}

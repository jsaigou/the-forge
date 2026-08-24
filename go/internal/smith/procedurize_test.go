// SPDX-License-Identifier: Apache-2.0

package smith

// procedurize_test.go — "let smith fix it" (autonomous-remediation Sprint 3,
// docs/v5-smith.md §13): ProcedurePreview (read-only projection) and
// Procedurize (create the mapped procedure action, approve it, supersede
// the source) end to end against a real store.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

func TestProcedurePreview_MappedKind(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	preview, err := s.ProcedurePreview(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("ProcedurePreview: %v", err)
	}
	if preview.ProcedureID != "restart_down_unit" {
		t.Errorf("procedure_id = %q, want restart_down_unit", preview.ProcedureID)
	}
	if preview.NeedsMaintenance {
		t.Error("expected restart_down_unit to not need maintenance")
	}
	if preview.EstDurationSec != 15 {
		t.Errorf("est_duration_sec = %d, want 15", preview.EstDurationSec)
	}
}

func TestProcedurePreview_UnmappedKind(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindSettingsChange, Title: "test", Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, settingsChangeDetail{Key: SettingSchedule, Value: json.RawMessage(`{}`)}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.ProcedurePreview(context.Background(), src.ID); err == nil {
		t.Fatal("expected ProcedurePreview to reject a kind with no mapped procedure")
	}
}

func TestProcedurize_RestartForgeUnit_EndToEnd(t *testing.T) {
	execAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := execAt.Add(time.Minute)
	db := openDB(t)
	var restarted []string
	pub := &stubPublisher{}
	s := New(Deps{
		Store: db, Cfg: func() *config.Config { return &config.Config{} }, Publisher: pub,
		RestartUnit: func(_ context.Context, unit string) error { restarted = append(restarted, unit); return nil },
		Source:      buildSnapshotAt(fresh), Now: fixedNow(execAt), Logf: func(string, ...any) {},
	})
	s.bgCtx = context.Background()

	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	replacement, err := s.Procedurize(context.Background(), src.ID, "operator")
	if err != nil {
		t.Fatalf("Procedurize: %v", err)
	}
	if replacement.Kind != KindProcedure {
		t.Fatalf("replacement.Kind = %s, want procedure", replacement.Kind)
	}

	waitFor(t, time.Second, func() bool {
		a, _ := s.GetAction(context.Background(), replacement.ID)
		return a != nil && a.Status != StatusExecuting && a.Status != StatusApproved
	})
	final, err := s.GetAction(context.Background(), replacement.ID)
	if err != nil {
		t.Fatalf("GetAction(replacement): %v", err)
	}
	if final.Status != StatusDone {
		t.Fatalf("replacement status = %s, want done (result=%+v)", final.Status, final.Result)
	}
	if len(restarted) != 1 || restarted[0] != "forge-stt" {
		t.Fatalf("restarted = %v, want [forge-stt]", restarted)
	}

	source, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction(source): %v", err)
	}
	if source.Status != StatusSuperseded {
		t.Fatalf("source status = %s, want superseded", source.Status)
	}
	if !pub.has(EventActionUpdate) {
		t.Error("expected at least one smith:action_update publish")
	}
}

func TestProcedurize_NotPending_Refused(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Cfg: func() *config.Config { return &config.Config{} }})
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindRestartForgeUnit, Title: "restart forge-stt", Risk: RiskLow, CreatedBy: "smith",
		Detail: mustJSON(t, restartUnitDetail{Unit: "forge-stt"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	forceStatus(t, db, src.ID, StatusApproved)

	if _, err := s.Procedurize(context.Background(), src.ID, "operator"); err != ErrInvalidTransition {
		t.Fatalf("Procedurize on a non-pending source: err = %v, want ErrInvalidTransition", err)
	}
}

func TestProcedurize_UnmappedKind_Refused(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings()})
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindSettingsChange, Title: "test", Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, settingsChangeDetail{Key: SettingSchedule, Value: json.RawMessage(`{}`)}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := s.Procedurize(context.Background(), src.ID, "operator"); err == nil {
		t.Fatal("expected Procedurize to reject a kind with no mapped procedure")
	}
}

// TestProcedurize_SelfEvicting_FailsClosed proves the Sprint 3 safety guard:
// CreateAction's self-eviction stamping only runs for KindLoadConfig/
// KindUnloadSlot drafts, never for the KindProcedure action Procedurize
// would create — so a self_evicting unload_slot source must be refused
// outright rather than silently losing its handoff-required gate.
func TestProcedurize_SelfEvicting_FailsClosed(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	src, err := s.CreateAction(context.Background(), ActionDraft{
		Kind: KindUnloadSlot, Title: "unload a1", Risk: RiskHigh, CreatedBy: "smith",
		Detail: mustJSON(t, unloadSlotDetail{Slot: "a1"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE smith_actions SET self_evicting = 1 WHERE id = ?`, src.ID); err != nil {
		t.Fatalf("force self_evicting: %v", err)
	}

	if _, err := s.Procedurize(context.Background(), src.ID, "operator"); err == nil {
		t.Fatal("expected Procedurize to refuse a self-evicting source action")
	}
	source, err := s.GetAction(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if source.Status != StatusPending {
		t.Fatalf("source status = %s, want pending (untouched)", source.Status)
	}
}

// TestProcedureForAction_BinaryUpstreamRunbookMaps pins Sprint 6's G6 fix:
// a build-refresh runbook is KindRunbook, same as several unrelated
// proposal shapes — the DedupeKey prefix, not the kind alone, is what
// makes it recognizable.
func TestProcedureForAction_BinaryUpstreamRunbookMaps(t *testing.T) {
	a := &Action{Kind: KindRunbook, DedupeKey: dedupeKeyBinaryUpstreamPrefix + "llama.cpp (vulkan)"}
	id, ok := procedureForAction(a)
	if !ok || id != "build_refresh" {
		t.Fatalf("procedureForAction = (%q, %v), want (build_refresh, true)", id, ok)
	}
}

// TestProcedureForAction_OtherRunbooksDoNotMap confirms the discriminator
// is genuinely a prefix match on DedupeKey, not just "kind == runbook" —
// self-review closures and every other runbook shape must still refuse.
func TestProcedureForAction_OtherRunbooksDoNotMap(t *testing.T) {
	cases := []string{
		"runbook:kernel_params",
		"runbook:gtt_ceiling",
		"runbook:binary_stale:llama.cpp (kintsugi)",
		"",
	}
	for _, dk := range cases {
		a := &Action{Kind: KindRunbook, DedupeKey: dk}
		if id, ok := procedureForAction(a); ok {
			t.Errorf("dedupe_key %q: procedureForAction = (%q, true), want (_, false)", dk, id)
		}
	}
}

// TestProcedureParamsForAction_BinaryUpstreamExtractsName confirms the
// param reshape reads the tracked binary's Name out of
// proposeRebuildRunbook's real Detail shape (propose.go) and nothing else
// from it — build_refresh resolves path/source_ref/upstream_ref itself,
// fresh, from the live setting.
func TestProcedureParamsForAction_BinaryUpstreamExtractsName(t *testing.T) {
	a := &Action{
		Kind:      KindRunbook,
		DedupeKey: dedupeKeyBinaryUpstreamPrefix + "llama.cpp (vulkan)",
		Detail:    mustJSON(t, map[string]any{"check_id": "binary_versions", "name": "llama.cpp (vulkan)", "upstream_ahead": 4}),
	}
	params, err := procedureParamsForAction(a)
	if err != nil {
		t.Fatalf("procedureParamsForAction: %v", err)
	}
	if params["binary"] != "llama.cpp (vulkan)" {
		t.Fatalf("params = %+v, want binary=llama.cpp (vulkan)", params)
	}
}

// TestAction_MarshalJSON_Procedurizable exercises Sprint 6's G7 fix: the
// field is computed at marshal time from the same three conditions
// ActionCard.tsx's canProcedurize predicate checks, so there is exactly one
// place (procedureForAction) deciding which kinds get the button.
func TestAction_MarshalJSON_Procedurizable(t *testing.T) {
	cases := []struct {
		name string
		a    Action
		want bool
	}{
		{"pending mapped kind", Action{Kind: KindUnloadSlot, Status: StatusPending}, true},
		{"executing mapped kind", Action{Kind: KindUnloadSlot, Status: StatusExecuting}, false},
		{"self-evicting mapped kind", Action{Kind: KindUnloadSlot, Status: StatusPending, SelfEvicting: true}, false},
		{"unmapped kind", Action{Kind: KindSettingsChange, Status: StatusPending}, false},
		{"pending binary-upstream runbook", Action{Kind: KindRunbook, Status: StatusPending, DedupeKey: dedupeKeyBinaryUpstreamPrefix + "x"}, true},
		{"pending unrelated runbook", Action{Kind: KindRunbook, Status: StatusPending, DedupeKey: "runbook:kernel_params"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.a)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var decoded struct {
				Procedurizable bool `json:"procedurizable"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if decoded.Procedurizable != tc.want {
				t.Errorf("procedurizable = %v, want %v (json=%s)", decoded.Procedurizable, tc.want, raw)
			}
		})
	}
}

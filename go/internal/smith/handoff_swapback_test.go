// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
)

// TestExecuteAction_ProposesSwapBackAfterRemoteSwap covers the Wave B loop
// end to end: a self-evicting action's handoff is resolved "remote" (a
// healthy candidate swaps smith.model), the action executes, and
// executeAction's post-finalize hook auto-proposes a settings_change action
// to restore the original brain model — pending, not auto-executed (§3
// constraint 1: every state change stays behind approval).
func TestExecuteAction_ProposesSwapBackAfterRemoteSwap(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	placer := &stubPlacer{}
	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched:  newStubSched(map[string]string{"a2": "ornith-35b"}),
		Placer: placer,
		ProviderHealth: func(context.Context, string) (string, error) {
			return "reachable", nil
		},
		Logf: func(string, ...any) {},
	})
	ctx := context.Background()

	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindLoadConfig, Risk: RiskLow, CreatedBy: "op",
		Detail: mustJSON(t, loadConfigDetail{Mode: "big-model", Slot: "a2"}),
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if !a.SelfEvicting {
		t.Fatalf("test setup bug: action should be self-evicting, got %+v", a)
	}

	if _, err := s.ResolveHandoff(ctx, a.ID, "remote", "op"); err != nil {
		t.Fatalf("ResolveHandoff(remote): %v", err)
	}
	forceStatus(t, db, a.ID, StatusApproved)
	s.executeAction(ctx, a.ID)

	final, err := s.GetAction(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	// No collector Source is wired in this fixture, so finalizeResult can't
	// confirm freshness and lands on done_unverified rather than done — the
	// swap-back hook fires on ANY final status (see proposeSwapBack's doc
	// comment), so that's an expected outcome here, not a test bug.
	if final.Status != StatusDone && final.Status != StatusDoneUnverified {
		t.Fatalf("status = %s, want done or done_unverified (result=%+v)", final.Status, final.Result)
	}

	actions, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	wantDedupe := "swapback:" + strconv.FormatInt(a.ID, 10)
	var swapBack *Action
	for i := range actions {
		if actions[i].Kind == KindSettingsChange && actions[i].DedupeKey == wantDedupe {
			swapBack = &actions[i]
		}
	}
	if swapBack == nil {
		t.Fatalf("no swap-back proposal (dedupe_key %q) found among pending actions: %+v", wantDedupe, actions)
	}
	var detail settingsChangeDetail
	if err := json.Unmarshal(swapBack.Detail, &detail); err != nil {
		t.Fatalf("unmarshal swap-back detail: %v", err)
	}
	if detail.Key != SettingModel {
		t.Errorf("swap-back key = %q, want %q", detail.Key, SettingModel)
	}
	var restoredModel string
	if err := json.Unmarshal(detail.Value, &restoredModel); err != nil {
		t.Fatalf("unmarshal swap-back value: %v", err)
	}
	if restoredModel != "ornith-35b" {
		t.Errorf("swap-back restores model = %q, want ornith-35b (the original brain)", restoredModel)
	}
	if swapBack.CreatedBy != "smith" {
		t.Errorf("swap-back created_by = %q, want smith", swapBack.CreatedBy)
	}
}

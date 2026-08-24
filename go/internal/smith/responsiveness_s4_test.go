// SPDX-License-Identifier: Apache-2.0

package smith

// S4 responsiveness tests: the ≤5000-token startup ceiling on the assembled
// context, the turn-budget settings reader, and Chat() returning instantly
// even when the brain load would block.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/sched"
)

// TestBuildContext_StartupTokenBudget pins S4's hard ceiling: with EVERY
// block present and maximally large (findings, notifications, catalog
// matches, KB matches, web research), buildContext must drop
// lowest-priority blocks until the assembled system prompt is within the
// ~5000-token startup budget. The model must never prefill the whole KB.
func TestBuildContext_StartupTokenBudget(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Logf: t.Logf})
	ctx := context.Background()

	huge := strings.Repeat("word ", 20000) // ~100KB per block, far over budget

	// Seed oversized sources for every block seam.
	if err := db.Settings().Set(ctx, SettingMeshServices, []byte(`[{"name":"x","aliases":["x"],"address":"x.example.ts.net"}]`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := s.AppendUserMessage(ctx, func() int64 { c, _ := s.CreateConversation(ctx, ""); return c }(), huge); err != nil {
			t.Fatal(err)
		}
	}

	out := s.buildContext(ctx, huge, nil, nil, "")
	if got := approxTokenCount(out); got > 5000 {
		t.Errorf("assembled startup context ≈ %d tokens (%d chars), want ≤ 5000 — the whole-KB prefill failure mode is back", got, len(out))
	}
	if !strings.Contains(out, "== Self context ==") {
		t.Error("self-context block was dropped; it is high priority and must survive")
	}
}

func TestTurnBudget_ReaderDefaultsAndFallbacks(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: t.Logf})
	ctx := context.Background()

	def := DefaultTurnBudget()
	if def.FirstTurnS != 150 || def.EscalationS != 300 || def.RoundTimeoutS != 240 {
		t.Fatalf("DefaultTurnBudget = %+v, want 150/300/240", def)
	}
	got := s.TurnBudget(ctx)
	if got != def {
		t.Errorf("unset setting = %+v, want defaults %+v", got, def)
	}

	// Partial object: missing fields keep defaults, zero/negative values are
	// rejected (a zero budget would fail every turn instantly).
	if err := db.Settings().Set(ctx, SettingTurnBudget, []byte(`{"first_turn_s":60,"escalation_s":0}`)); err != nil {
		t.Fatal(err)
	}
	got = s.TurnBudget(ctx)
	if got.FirstTurnS != 60 || got.EscalationS != 300 || got.RoundTimeoutS != 240 {
		t.Errorf("partial setting = %+v, want 60/300/240", got)
	}

	// Malformed JSON → full defaults.
	if err := db.Settings().Set(ctx, SettingTurnBudget, []byte(`{not json`)); err != nil {
		t.Fatal(err)
	}
	if got := s.TurnBudget(ctx); got != def {
		t.Errorf("malformed setting = %+v, want defaults", got)
	}
}

// TestChat_ReturnsInstantlyWhileBrainLoadBlocks pins the S4 structural fix:
// Chat() must hand back a message id immediately even when the brain load
// can never succeed (the old code blocked inside Chat for the full load
// attempt before any message existed).
func TestChat_ReturnsInstantlyWhileBrainLoadBlocks(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, Err: errors.New("no idle slot available")}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{Escalate: true}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Chat took %s to return with a blocking brain load, want immediate", elapsed)
	}
}

// SPDX-License-Identifier: Apache-2.0

package smith

// answers_s3_test.go — Sprint S3 guardrail tests: action drafting, refusals,
// context attachment, and the resolution loop. Every refusal is a test;
// every action-drafting path is a test.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// ── Refusal tests (§3.5 guardrails) ──────────────────────────────────────────

func TestS3_RefusalForgeDaemon(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_forge-daemon"})
	if !ok {
		t.Fatal("refusal should answer (ok=true) with the plain-language reason")
	}
	if !strings.Contains(fa.Text, "won't restart forge-daemon") {
		t.Errorf("refusal = %q, want the forge-daemon refusal reason", fa.Text)
	}
	if fa.ActionID != nil {
		t.Error("refusal must not create an action")
	}
}

func TestS3_RefusalSlotUnit(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_slot_unit"})
	if !ok {
		t.Fatal("refusal should answer (ok=true)")
	}
	if !strings.Contains(fa.Text, "scheduler slot unit") {
		t.Errorf("refusal = %q, want the slot-unit refusal reason", fa.Text)
	}
	if fa.ActionID != nil {
		t.Error("refusal must not create an action")
	}
}

func TestS3_RefusalEmptyEntity(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	_, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: ""})
	if ok {
		t.Error("empty action entity should return ok=false (no match)")
	}
}

// ── Action drafting: restart_forge_unit ───────────────────────────────────

func TestS3_RestartForgeUnitDraft(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "test")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convIDCopy := convID
	intent := Intent{Family: FamilyAction, Entity: "restart_forge-stt", ConversationID: &convIDCopy}
	fa, ok := s.Answer(ctx, intent)
	if !ok {
		t.Fatal("Answer returned !ok for restart_forge-stt")
	}
	if fa.ActionID == nil {
		t.Fatal("answer should carry an ActionID")
	}
	if !strings.Contains(fa.Text, "drafted a proposal to restart forge-stt") {
		t.Errorf("answer = %q, want 'drafted a proposal to restart forge-stt'", fa.Text)
	}
	a, err := s.GetAction(ctx, *fa.ActionID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Kind != KindRestartForgeUnit {
		t.Errorf("kind = %q, want %q", a.Kind, KindRestartForgeUnit)
	}
	if a.Status != StatusPending {
		t.Errorf("status = %q, want pending", a.Status)
	}
	if a.CreatedBy != "smith" {
		t.Errorf("created_by = %q, want smith", a.CreatedBy)
	}
	if a.ConversationID == nil || *a.ConversationID != convID {
		t.Errorf("conversation_id = %v, want %d", a.ConversationID, convID)
	}
}

func TestS3_RestartTTSUnitDraft(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), func(d *Deps) {
		d.Cfg = func() *config.Config {
			return &config.Config{Server: config.Server{TTSUnit: "forge-tts"}}
		}
	})
	ctx := context.Background()
	intent := Intent{Family: FamilyAction, Entity: "restart_tts"}
	fa, ok := s.Answer(ctx, intent)
	if !ok {
		t.Fatal("Answer returned !ok for restart_tts")
	}
	if fa.ActionID == nil {
		t.Fatal("answer should carry an ActionID")
	}
	a, err := s.GetAction(ctx, *fa.ActionID)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Kind != KindRestartForgeUnit {
		t.Errorf("kind = %q, want %q", a.Kind, KindRestartForgeUnit)
	}
	var detail restartUnitDetail
	if err := json.Unmarshal(a.Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if detail.Unit != "forge-tts" {
		t.Errorf("detail.unit = %q, want forge-tts", detail.Unit)
	}
}

// ── Action drafting: restart llama.cpp (scheduler-mediated) ──────────────────

func TestS3_RestartLlamaCppWithLoadedSlot(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), func(d *Deps) {
		d.Sched = newStubSched(map[string]string{"a1": "qwen36-mtp"})
		d.Placer = &stubPlacer{}
	})
	ctx := context.Background()
	fa, ok := s.Answer(ctx, Intent{Family: FamilyAction, Entity: "restart_llama.cpp"})
	if !ok {
		t.Fatal("Answer returned !ok for restart_llama.cpp")
	}
	if fa.ActionID == nil {
		t.Fatal("answer should carry an ActionID (the unload proposal)")
	}
	if !strings.Contains(fa.Text, "scheduler-mediated") {
		t.Errorf("answer = %q, want 'scheduler-mediated'", fa.Text)
	}
	if !strings.Contains(fa.Text, "qwen36-mtp") {
		t.Errorf("answer = %q, want it to mention the loaded mode", fa.Text)
	}
	// Verify both unload + load actions were created.
	actions, err := s.ListActions(ctx, StatusPending, nil, 10)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	var hasUnload, hasLoad bool
	for _, a := range actions {
		if a.Kind == KindUnloadSlot {
			hasUnload = true
		}
		if a.Kind == KindLoadConfig {
			hasLoad = true
		}
	}
	if !hasUnload {
		t.Error("expected an unload_slot action")
	}
	if !hasLoad {
		t.Error("expected a load_config action")
	}
}

func TestS3_RestartLlamaCppNothingLoaded(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), func(d *Deps) {
		d.Sched = newStubSched(map[string]string{})
	})
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_llama.cpp"})
	if !ok {
		t.Fatal("Answer returned !ok")
	}
	if !strings.Contains(fa.Text, "nothing to restart") {
		t.Errorf("answer = %q, want 'nothing to restart'", fa.Text)
	}
	if fa.ActionID != nil {
		t.Error("should not create an action when nothing is loaded")
	}
}

func TestS3_RestartLlamaCppNoScheduler(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_llama.cpp"})
	if !ok {
		t.Fatal("Answer returned !ok")
	}
	if !strings.Contains(fa.Text, "can't see that") {
		t.Errorf("answer = %q, want a gap answer about scheduler not wired", fa.Text)
	}
}

// ── Context attachment (§3.4, R5) ────────────────────────────────────────────

func TestS3_ClassifyContextItems(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	cases := []struct {
		name     string
		items    []ChatContext
		wantFam  IntentFamily
		wantEnt  string
	}{
		{
			name:    "source matches check ID (gpu_hang)",
			items:   []ChatContext{{Code: "KFD_EVICTION", Message: "GPU queue eviction", Source: "gpu_hang"}},
			wantFam: FamilyHealth,
			wantEnt: "gpu",
		},
		{
			name:    "alert code INFERENCE_HANG maps to gpu",
			items:   []ChatContext{{Code: "INFERENCE_HANG", Message: "inference stalled", Source: "gpu_hang"}},
			wantFam: FamilyHealth,
			wantEnt: "gpu",
		},
		{
			name:    "alert code GTT_HIGH maps to gtt",
			items:   []ChatContext{{Code: "GTT_HIGH", Message: "GTT high", Source: ""}},
			wantFam: FamilyHealth,
			wantEnt: "gtt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent, ok := s.classifyContextItems(ctx, tc.items)
			if !ok {
				t.Fatal("classifyContextItems returned !ok")
			}
			if intent.Family != tc.wantFam {
				t.Errorf("family = %q, want %q", intent.Family, tc.wantFam)
			}
			if intent.Entity != tc.wantEnt {
				t.Errorf("entity = %q, want %q", intent.Entity, tc.wantEnt)
			}
		})
	}
}

func TestS3_ChatWithContextSeed(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {
		d.Cfg = func() *config.Config {
			return &config.Config{Ports: map[string]int{"embedding": 8083, "stt": 8084}}
		}
	})
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	// Simulate the httpapi handler: compose the seed text from context.
	seedText := "[alert KFD_EVICTION] GPU queue eviction detected (source: gpu_hang, 2026-08-14 14:23:20)"
	msgID, err := s.Chat(ctx, convID, seedText, ChatOptions{
		Context: []ChatContext{{Code: "KFD_EVICTION", Message: "GPU queue eviction detected", Source: "gpu_hang", At: 1723647800}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, msgs, err := s.GetConversation(ctx, convID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.ID == msgID && m.Content != "" {
				return true
			}
		}
		return false
	})
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	// The user message should contain the composed seed text.
	var foundUser bool
	for _, m := range msgs {
		if m.Kind == MsgKindUser && strings.Contains(m.Content, "KFD_EVICTION") {
			foundUser = true
		}
	}
	if !foundUser {
		t.Error("user message should contain the composed seed text")
	}
	// The answer should be about GPU health.
	var foundAnswer bool
	for _, m := range msgs {
		if m.ID == msgID && m.Content != "" {
			if strings.Contains(m.Content, "checked just now") {
				foundAnswer = true
			}
		}
	}
	if !foundAnswer {
		t.Error("answer message should contain a fast-path answer")
	}
}

// ── Action-kind transcript message (§2.4.2) ─────────────────────────────────

func TestS3_ChatActionKindMessage(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "restart forge-stt", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Wait for the action-kind message (appended after the answer in the
	// fast-path goroutine).
	waitFor(t, 2*time.Second, func() bool {
		_, msgs, err := s.GetConversation(ctx, convID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.Kind == MsgKindAction && m.Evidence != nil {
				return true
			}
		}
		return false
	})
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	// The answer should mention the drafted proposal.
	var foundAnswer bool
	for _, m := range msgs {
		if m.ID == msgID && strings.Contains(m.Content, "drafted a proposal") {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Error("answer should mention 'drafted a proposal'")
	}
	// An action-kind message should be appended with {"action_id": N} evidence.
	var foundActionMsg bool
	for _, m := range msgs {
		if m.Kind == MsgKindAction && m.Evidence != nil {
			var ev map[string]any
			if err := json.Unmarshal([]byte(*m.Evidence), &ev); err == nil {
				if _, ok := ev["action_id"]; ok {
					foundActionMsg = true
				}
			}
		}
	}
	if !foundActionMsg {
		t.Error("expected an action-kind message with {action_id: N} evidence")
	}
}

// ── Resolution loop (§2.4.1) ─────────────────────────────────────────────────

func TestS3_ResolutionLoop(t *testing.T) {
	cfg := &config.Config{Ports: map[string]int{"embedding": 8083, "stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8083: true, 8084: true} // all ports up → always_on_ports returns ok
	s := answerSmith(t, snap, func(d *Deps) {
		d.Cfg = func() *config.Config { return cfg }
	})
	ctx := context.Background()

	// 1. Create a conversation + investigation.
	convID, err := s.CreateConversation(ctx, "test")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	invID, err := s.CreateInvestigation(ctx, "manual", "always_on_ports was warn")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}

	// 2. Persist a warn finding for always_on_ports attached to the investigation.
	warnAt := time.Unix(500, 0)
	_, err = s.persistFindings(ctx, []Finding{{
		CheckID: "always_on_ports", Severity: SeverityWarn, Summary: "embedding not listening",
	}}, SweepManual, warnAt, &invID)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	// 3. Create an action attached to the investigation + conversation.
	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRestartForgeUnit, Title: "Restart forge-stt",
		Risk: RiskLow, Detail: []byte(`{"unit":"forge-stt"}`),
		InvestigationID: &invID, ConversationID: &convID, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	// 4. Call proposeResolution (normally triggered by maybeProposeResolution
	//    after executeAction completes done).
	s.proposeResolution(ctx, a.ID, invID)

	// 5. Verify the investigation is resolved.
	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status != "resolved" {
		t.Errorf("investigation status = %q, want resolved", inv.Status)
	}
	if inv.ResolvedByActionID == nil || *inv.ResolvedByActionID != a.ID {
		t.Errorf("resolved_by_action_id = %v, want %d", inv.ResolvedByActionID, a.ID)
	}

	// 6. Verify a summary message was posted to the conversation.
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var foundSummary bool
	for _, m := range msgs {
		if m.Kind == MsgKindDeterministic && strings.Contains(m.Content, "fixed") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Error("expected a summary message containing 'fixed'")
	}
}

func TestS3_ResolutionLoopChecksStillFailing(t *testing.T) {
	// Same setup as above but the snapshot shows ports still down — the
	// re-run check should still be warn, and the investigation should NOT
	// be resolved.
	cfg := &config.Config{Ports: map[string]int{"embedding": 8083, "stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8083: false, 8084: false} // ports still down
	s := answerSmith(t, snap, func(d *Deps) {
		d.Cfg = func() *config.Config { return cfg }
	})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "test")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	invID, err := s.CreateInvestigation(ctx, "manual", "always_on_ports was warn")
	if err != nil {
		t.Fatalf("CreateInvestigation: %v", err)
	}
	warnAt := time.Unix(500, 0)
	_, err = s.persistFindings(ctx, []Finding{{
		CheckID: "always_on_ports", Severity: SeverityWarn, Summary: "embedding not listening",
	}}, SweepManual, warnAt, &invID)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
	a, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRestartForgeUnit, Title: "Restart forge-stt",
		Risk: RiskLow, Detail: []byte(`{"unit":"forge-stt"}`),
		InvestigationID: &invID, ConversationID: &convID, CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	s.proposeResolution(ctx, a.ID, invID)

	inv, _, err := s.GetInvestigation(ctx, invID)
	if err != nil {
		t.Fatalf("GetInvestigation: %v", err)
	}
	if inv.Status == "resolved" {
		t.Error("investigation should NOT be resolved when checks still fail")
	}
	if inv.ResolvedByActionID != nil {
		t.Error("resolved_by_action_id should not be set when checks still fail")
	}
	// Summary should mention still failing.
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var foundSummary bool
	for _, m := range msgs {
		if m.Kind == MsgKindDeterministic && strings.Contains(m.Content, "still failing") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Error("expected a summary message containing 'still failing'")
	}
}

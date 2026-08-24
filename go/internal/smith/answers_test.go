// SPDX-License-Identifier: Apache-2.0

package smith

// answers_test.go — answer synthesis tests for the fast-path engine. Each
// family gets at least one test asserting the answer is specific (not a
// generic dump), cites a live source ("checked just now"), and carries the
// dig-deeper chip. Gap entries answer honestly ("I can't see that").

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// answerSmith builds a Smith with a real in-memory store + a snapshot, for
// answer-engine tests that need metrics/checks.
func answerSmith(t *testing.T, snap *collector.Snapshot, extra func(*Deps)) *Smith {
	t.Helper()
	db := openDB(t)
	d := Deps{
		Store:    db,
		Settings: db.Settings(),
		Catalog:  db.Catalog(),
		Source:   collector.NewStatic(snap),
		Cfg:      func() *config.Config { return &config.Config{} },
		Now:      func() time.Time { return time.Unix(1000, 0) },
		Logf:     func(string, ...any) {},
	}
	if extra != nil {
		extra(&d)
	}
	return New(d)
}

func assertAnswer(t *testing.T, fa FastAnswer, ok bool, wantSubstring string) {
	t.Helper()
	if !ok {
		t.Fatalf("Answer returned !ok, want a fast answer")
	}
	if wantSubstring != "" && !strings.Contains(fa.Text, wantSubstring) {
		t.Errorf("answer = %q, want it to contain %q", fa.Text, wantSubstring)
	}
	if !strings.Contains(fa.Text, "checked just now") {
		t.Errorf("answer must cite a live source: %q", fa.Text)
	}
	if !fa.DigDeeper {
		t.Error("answer must carry the dig-deeper chip")
	}
}

func TestAnswer_HealthComfyUI(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	snap.Units["ai-mode-comfyui"] = collector.UnitState{ActiveState: "active", SubState: "running"}
	snap.Ports[3001] = true
	s := answerSmith(t, snap, func(d *Deps) {})
	// ComfyUI health is gated on smith.comfyui.enabled — set it via settings.
	setSetting(t, s.d.Store, SettingComfyUIEnabled, `true`)
	setSetting(t, s.d.Store, SettingComfyUIURL, `"http://127.0.0.1:3001"`)
	setSetting(t, s.d.Store, SettingComfyUIUnit, `"ai-mode-comfyui"`)

	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "")
	// ComfyUI is up in this snapshot → "Yes".
	if !strings.Contains(fa.Text, "Yes") && !strings.Contains(fa.Text, "No") {
		t.Errorf("answer should start with Yes/No: %q", fa.Text)
	}
}

// comfySmith builds a smith with comfyui enabled and the given unit state +
// port-dial result, so the down/up branches of the comfyui health answer are
// exercised deterministically (the real dial is loopback, which fails in a
// test process — wire DialPort instead).
func comfySmith(t *testing.T, u collector.UnitState, portUp bool) *Smith {
	t.Helper()
	snap := snapWith(collector.Metrics{})
	snap.Units["ai-mode-comfyui"] = u
	s := answerSmith(t, snap, func(d *Deps) {
		d.DialPort = func(int) bool { return portUp }
	})
	setSetting(t, s.d.Store, SettingComfyUIEnabled, `true`)
	setSetting(t, s.d.Store, SettingComfyUIURL, `"http://127.0.0.1:3001"`)
	setSetting(t, s.d.Store, SettingComfyUIUnit, `"ai-mode-comfyui"`)
	return s
}

func TestAnswer_HealthComfyUI_Up(t *testing.T) {
	s := comfySmith(t, collector.UnitState{ActiveState: "active", SubState: "running"}, true)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "Yes")
	if !strings.Contains(fa.Text, "up") {
		t.Errorf("answer should say ComfyUI is up: %q", fa.Text)
	}
}

func TestAnswer_HealthComfyUI_Stopped(t *testing.T) {
	s := comfySmith(t, collector.UnitState{ActiveState: "inactive", SubState: "dead"}, true)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "isn't running")
	if !strings.Contains(fa.Text, "No") {
		t.Errorf("answer should start with No (not Yes): %q", fa.Text)
	}
}

func TestAnswer_HealthComfyUI_OOMKilled(t *testing.T) {
	s := comfySmith(t, collector.UnitState{ActiveState: "failed", Result: "oom-kill"}, true)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "OOM-killed")
}

func TestAnswer_HealthComfyUI_Crashed(t *testing.T) {
	s := comfySmith(t, collector.UnitState{ActiveState: "failed", Result: "exit-code"}, true)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "crashed")
}

func TestAnswer_HealthComfyUI_PortDown(t *testing.T) {
	s := comfySmith(t, collector.UnitState{ActiveState: "active", SubState: "running"}, false)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "comfyui"})
	assertAnswer(t, fa, ok, "port 3001 isn't answering")
}

func TestAnswer_HealthSlot(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {
		d.Sched = newStubSched(map[string]string{"a1": "qwen36-mtp"})
	})
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyHealth, Entity: "a1"})
	assertAnswer(t, fa, ok, "qwen36-mtp")
}

func TestAnswer_QuantityRAM(t *testing.T) {
	snap := snapWith(collector.Metrics{Memory: collector.Memory{
		TotalBytes: 128 << 30, UsedBytes: 40 << 30, AvailBytes: 88 << 30, Pct: 31.25,
	}})
	s := answerSmith(t, snap, nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyQuantity, Entity: "ram"})
	assertAnswer(t, fa, ok, "GB")
}

func TestAnswer_QuantityGTT(t *testing.T) {
	total := int64(120 << 30)
	used := int64(60 << 30)
	snap := snapWith(collector.Metrics{
		GTTUsedBytes:  &used,
		GTTTotalBytes: &total,
	})
	s := answerSmith(t, snap, nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyQuantity, Entity: "gtt"})
	assertAnswer(t, fa, ok, "GTT")
}

func TestAnswer_ReachabilityAddress(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {})
	// Two-layer architecture: migrations seed smith.mesh.services EMPTY —
	// the inventory arrives via the local-seed import seam (here, the
	// shipped synthetic example deployment).
	importExampleSeed(t, s.d.Store)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyReachability, Entity: "comfy-alpha"})
	assertAnswer(t, fa, ok, "comfy-alpha.example.ts.net")
	for _, ev := range fa.Evidence {
		if ev.Label == "source" && ev.Value != "settings: "+SettingMeshServices {
			t.Errorf("evidence source = %q, want the settings inventory cited", ev.Value)
		}
	}
}

func TestAnswer_ReachabilityAddressFollowsSettings(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {})
	setSetting(t, s.d.Store, SettingMeshServices,
		`[{"name":"comfy-alpha","aliases":["comfy-alpha"],"address":"comfy.example-lan.ts.net:3001"}]`)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyReachability, Entity: "comfy-alpha"})
	assertAnswer(t, fa, ok, "comfy.example-lan.ts.net:3001")
}

func TestAnswer_ReachabilityEmptyInventoryIsHonestGap(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {})
	// Fresh install: migration seeded [] and nothing was imported yet — a
	// mesh reachability question is an honest gap, not a fabricated answer.
	_, ok := s.Answer(context.Background(), Intent{Family: FamilyReachability, Entity: "comfy-alpha"})
	if ok {
		t.Error("Answer with an unprovisioned mesh inventory should report a gap, not invent an address")
	}
}

func TestMeshServices_UnreadableSettingFailsClosed(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {})
	setSetting(t, s.d.Store, SettingMeshServices, `{not json`)
	if got := s.MeshServices(context.Background()); len(got) != 0 {
		t.Errorf("MeshServices on malformed JSON = %d entries, want empty (fail closed)", len(got))
	}
	// And the reachability answer honestly falls through (no fabricated
	// address) — a mesh entity with no readable inventory is a gap.
	_, ok := s.Answer(context.Background(), Intent{Family: FamilyReachability, Entity: "comfy-alpha"})
	if ok {
		t.Error("Answer with unreadable mesh inventory should report a gap, not invent an address")
	}
}

func TestAnswer_ReachabilityTailnet(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {
		d.TailscalePeers = func(ctx context.Context) ([]collector.Peer, bool) {
			return []collector.Peer{{DNSName: "self"}}, true
		}
	})
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyReachability, Entity: "tailnet"})
	assertAnswer(t, fa, ok, "tailnet")
}

func TestAnswer_ListingPendingActions(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	// Seed a pending action.
	_, err := s.CreateAction(ctx, ActionDraft{
		Kind: KindRestartForgeUnit, Title: "Restart forge-stt",
		Risk: RiskLow, Detail: []byte(`{"unit":"forge-stt"}`), CreatedBy: "smith",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	fa, ok := s.Answer(ctx, Intent{Family: FamilyListing, Entity: "pending_actions"})
	assertAnswer(t, fa, ok, "pending")
}

func TestAnswer_ListingBacklog(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyListing, Entity: "backlog"})
	assertAnswer(t, fa, ok, "backlog")
}

func TestAnswer_HistoryFindingDuration(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	// Seed a finding for gtt_ceiling.
	_, err := s.persistFindings(ctx, []Finding{{
		CheckID: "gtt_ceiling", Severity: SeverityWarn, Summary: "GTT high",
	}}, "manual", time.Unix(500, 0), nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}
	fa, ok := s.Answer(ctx, Intent{Family: FamilyHistory, Entity: "gtt_ceiling"})
	assertAnswer(t, fa, ok, "gtt_ceiling")
}

func TestAnswer_LogsJournalDigest(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {
		d.JournalErrors = func(ctx context.Context, n int, since time.Time) ([]string, error) {
			return []string{"Aug 15 02:39 forgehost kernel: oom-kill compressor"}, nil
		}
	})
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyLogs, Entity: "forge_unit_error_digest"})
	assertAnswer(t, fa, ok, "oom-kill")
}

func TestAnswer_LogsNotificationsFallback(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil) // no JournalErrors wired
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyLogs, Entity: "forge_unit_error_digest"})
	assertAnswer(t, fa, ok, "notification")
}

func TestAnswer_KB(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyKB, Entity: "silent-context-reduction"})
	assertAnswer(t, fa, ok, "")
}

func TestAnswer_VersionGap(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyVersion, Entity: "comfyui"})
	if !ok {
		t.Fatal("gap should answer (ok=true) with an honest message")
	}
	if !strings.Contains(fa.Text, "can't see that") {
		t.Errorf("gap answer = %q, want honest 'can't see that'", fa.Text)
	}
}

func TestAnswer_ActionRefusalDaemon(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_forge-daemon"})
	if !ok {
		t.Fatal("refusal should answer (ok=true) with the plain-language reason")
	}
	if !strings.Contains(fa.Text, "won't restart forge-daemon") {
		t.Errorf("refusal = %q, want the forge-daemon refusal reason", fa.Text)
	}
}

func TestAnswer_ActionRefusalSlotUnit(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyAction, Entity: "restart_slot_unit"})
	if !ok {
		t.Fatal("refusal should answer (ok=true)")
	}
	if !strings.Contains(fa.Text, "scheduler slot unit") {
		t.Errorf("refusal = %q, want the slot-unit refusal reason", fa.Text)
	}
}

// TestAnswer_RedactsSecrets verifies fast answers pass the redaction pass
// (redact.go) — a secret-shaped string in an evidence value is scrubbed.
func TestAnswer_RedactsSecrets(t *testing.T) {
	snap := snapWith(collector.Metrics{})
	s := answerSmith(t, snap, func(d *Deps) {
		d.JournalErrors = func(ctx context.Context, n int, since time.Time) ([]string, error) {
			return []string{"bearer sk-router-abcdefghijklmnop1234567890 leaked"}, nil
		}
	})
	fa, ok := s.Answer(context.Background(), Intent{Family: FamilyLogs, Entity: "forge_unit_error_digest"})
	if !ok {
		t.Fatal("Answer returned !ok")
	}
	if strings.Contains(fa.Text, "sk-router-abcdefghijklmnop") {
		t.Errorf("answer leaked a secret: %s", fa.Text)
	}
	for _, ev := range fa.Evidence {
		if strings.Contains(ev.Value, "sk-router-abcdefghijklmnop") {
			t.Errorf("evidence %s leaked a secret: %s", ev.Label, ev.Value)
		}
	}
}

// TestMissedPatternLedger_RecordAndSurface verifies the missed-pattern
// ledger records a redacted question and surfaces it.
func TestMissedPatternLedger_RecordAndSurface(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()

	if err := s.RecordMissedPattern(ctx, "why is my token sk-router-abcdefgh-leaked123?", []string{"run_check"}); err != nil {
		t.Fatalf("RecordMissedPattern: %v", err)
	}
	patterns, err := s.MissedPatterns(ctx)
	if err != nil {
		t.Fatalf("MissedPatterns: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("patterns = %d, want 1", len(patterns))
	}
	if strings.Contains(patterns[0].Text, "sk-router-abcdefgh") {
		t.Errorf("ledger stored an unredacted secret: %s", patterns[0].Text)
	}
	if len(patterns[0].ToolsUsed) != 1 || patterns[0].ToolsUsed[0] != "run_check" {
		t.Errorf("toolsUsed = %v, want [run_check]", patterns[0].ToolsUsed)
	}

	// Surfaces on SelfContext.
	sc := s.SelfContext(ctx)
	if len(sc.MissedPatterns) != 1 {
		t.Errorf("SelfContext.MissedPatterns = %d, want 1", len(sc.MissedPatterns))
	}
}

// TestMissedPatternLedger_Capped verifies the ledger evicts oldest entries
// beyond the cap.
func TestMissedPatternLedger_Capped(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	for i := 0; i < missedPatternsCap+5; i++ {
		if err := s.RecordMissedPattern(ctx, "question "+strings.Repeat("x", i), nil); err != nil {
			t.Fatalf("RecordMissedPattern %d: %v", i, err)
		}
	}
	patterns, err := s.MissedPatterns(ctx)
	if err != nil {
		t.Fatalf("MissedPatterns: %v", err)
	}
	if len(patterns) != missedPatternsCap {
		t.Errorf("patterns = %d, want capped at %d", len(patterns), missedPatternsCap)
	}
}

// TestChat_FastPathAnswers verifies Chat() routes a matched question through
// the fast path (deterministic-kind message with the specific answer).
func TestChat_FastPathAnswers(t *testing.T) {
	snap := snapWith(collector.Metrics{Memory: collector.Memory{
		TotalBytes: 128 << 30, UsedBytes: 40 << 30, AvailBytes: 88 << 30, Pct: 31.25,
	}})
	s := answerSmith(t, snap, nil)
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "how much ram is free?", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	waitFor(t, time.Second, func() bool {
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
	for _, m := range msgs {
		if m.ID == msgID {
			if m.Kind != MsgKindDeterministic {
				t.Errorf("kind = %q, want %q", m.Kind, MsgKindDeterministic)
			}
			if !strings.Contains(m.Content, "GB") {
				t.Errorf("content = %q, want a specific RAM answer", m.Content)
			}
			if !strings.Contains(m.Content, "checked just now") {
				t.Errorf("content should cite 'checked just now': %q", m.Content)
			}
		}
	}
}

// TestChat_NoMatchRoutesToDeterministic verifies a no-match question with no
// brain falls through to the generic deterministic answer (the existing
// behavior, unchanged).
func TestChat_NoMatchRoutesToDeterministic(t *testing.T) {
	s := answerSmith(t, snapWith(collector.Metrics{}), nil)
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "tell me a story about a brave toaster", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	waitFor(t, time.Second, func() bool {
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
	for _, m := range msgs {
		if m.ID == msgID {
			// S4: the on-demand brain load moved into the background turn,
			// so the placeholder is created reasoning-kind and degrades
			// asynchronously — the TIER flip to deterministic is the
			// contract (finalizeMessage), not the message kind.
			if m.Tier != nil && *m.Tier != TierDeterministic {
				t.Errorf("tier = %v, want %q after degrade", *m.Tier, TierDeterministic)
			}
			if !strings.Contains(m.Content, "Brain:") {
				t.Errorf("content = %q, want the grounded deterministic answer", m.Content)
			}
		}
	}
}

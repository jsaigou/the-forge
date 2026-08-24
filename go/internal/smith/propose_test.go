// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── individual proposers, driven through real checks via RunChecks ─────────

func TestProposeRestartDownService(t *testing.T) {
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084, "embedding": 8083}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: false, 8083: true}
	s := New(Deps{
		Store: db, Source: collector.NewStatic(snap),
		Cfg: func() *config.Config { return cfg },
	})

	if _, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	actions, err := s.ListActions(context.Background(), StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	var found bool
	for _, a := range actions {
		if a.Kind == KindRestartForgeUnit && a.DedupeKey == KindRestartForgeUnit+":forge-stt" {
			found = true
			if a.CreatedBy != "smith" || a.Risk != RiskLow {
				t.Errorf("proposal = %+v, want created_by=smith risk=low", a)
			}
		}
	}
	if !found {
		t.Errorf("no restart_forge_unit proposal for forge-stt among %+v", actions)
	}
}

func TestProposeRestartCompressorProxy(t *testing.T) {
	db := openDB(t)
	if err := db.Routing().SaveProxy(context.Background(), store.ProxyRow{
		Service: "local", Port: 8788, TargetURL: "http://127.0.0.1:8080", Unit: "headroom@local",
	}); err != nil {
		t.Fatalf("SaveProxy: %v", err)
	}
	snap := snapWith(collector.Metrics{}) // headroom@local absent from Units -> inactive -> down
	s := New(Deps{Store: db, Source: collector.NewStatic(snap), Cfg: func() *config.Config { return &config.Config{} }})

	if _, err := s.RunChecks(context.Background(), "", []string{"compressor_reachability"}, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	actions, err := s.ListActions(context.Background(), StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 1 || actions[0].Kind != KindRestartForgeUnit || actions[0].DedupeKey != KindRestartForgeUnit+":headroom@local" {
		t.Errorf("actions = %+v, want one restart_forge_unit for headroom@local", actions)
	}
}

func TestProposeReconcileOrphanSlot(t *testing.T) {
	db := openDB(t)
	snap := snapWith(collector.Metrics{})
	snap.Slots["a3"] = collector.SlotState{Slot: "a3", Mode: "stray-model"}
	s := New(Deps{
		Store: db, Source: collector.NewStatic(snap),
		Sched: newStubSched(map[string]string{"a3": ""}),
	})

	if _, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	actions, err := s.ListActions(context.Background(), StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	var found bool
	for _, a := range actions {
		if a.Kind == KindUnloadSlot && a.DedupeKey == KindUnloadSlot+":a3" {
			found = true
			if a.Risk != RiskHigh {
				t.Errorf("proposal risk = %s, want high", a.Risk)
			}
		}
	}
	if !found {
		t.Errorf("no unload_slot proposal for orphaned a3 among %+v", actions)
	}
}

// TestProposeNeverTargetsOwnBrainSlot is guardrail 2 (docs/v5-smith.md
// §4.5): smith must never AUTO-PROPOSE unloading its own brain's slot.
// Exercises proposeReconcileOrphanSlot directly with a hand-built
// BrainResolution/Finding pair, rather than trying to reproduce this
// end-to-end via RunChecks: Brain()'s local_slot resolution and
// slot_agreement's orphan-direction condition both read the exact same
// s.d.Sched.Status().Slots map, and are mutually exclusive by
// construction — a slot Brain() resolves as "loaded per the scheduler" can
// never simultaneously be one slot_agreement sees as "scheduler thinks
// empty". The guardrail is real belt-and-braces (the brief's own words) for
// exactly this reason: today it can't fire via a live sweep, but the
// proposer must never rely on that staying true.
func TestProposeNeverTargetsOwnBrainSlot(t *testing.T) {
	br := BrainResolution{Resolution: BrainLocalSlot, Slot: "a3", Model: "ornith-35b"}
	f := Finding{
		CheckID:  "slot_agreement",
		Severity: SeverityWarn,
		Evidence: map[string]any{"mismatches": []slotMismatch{
			{Slot: "a3", SchedulerMode: "", UnitMode: "ornith-35b"}, // orphan direction, on the brain's own slot
		}},
	}
	drafts := proposeReconcileOrphanSlot(&CheckEnv{}, f, br)
	if len(drafts) != 0 {
		t.Errorf("proposeReconcileOrphanSlot(brain's own slot) = %+v, want no drafts", drafts)
	}

	// Sanity: the same mismatch on a DIFFERENT slot still proposes normally
	// — the guardrail is scoped to the brain's slot specifically, not a
	// blanket suppression of the whole check.
	f.Evidence["mismatches"] = []slotMismatch{{Slot: "a4", SchedulerMode: "", UnitMode: "stray"}}
	drafts = proposeReconcileOrphanSlot(&CheckEnv{}, f, br)
	if len(drafts) != 1 {
		t.Errorf("proposeReconcileOrphanSlot(a different slot) = %+v, want exactly one draft", drafts)
	}
}

func TestProposeKernelParamsRunbook(t *testing.T) {
	db := openDB(t)
	cmdline := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(cmdline, []byte("BOOT_IMAGE=/vmlinuz amdgpu.mcbp=0 quiet"), 0o644); err != nil {
		t.Fatalf("write cmdline fixture: %v", err)
	}
	s := New(Deps{Store: db, CmdlinePath: cmdline})

	if _, err := s.RunChecks(context.Background(), "", []string{"kernel_params"}, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	actions, err := s.ListActions(context.Background(), StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 1 || actions[0].Kind != KindRunbook || actions[0].Risk != RiskInfo {
		t.Fatalf("actions = %+v, want one info-risk runbook", actions)
	}
	if !strings.Contains(string(actions[0].Detail), "vm_fragment_size") {
		t.Errorf("detail = %s, want it to name the missing param", actions[0].Detail)
	}
	if !strings.Contains(string(actions[0].Detail), `"check_id":"kernel_params"`) {
		t.Errorf("detail = %s, want the source check id stamped (§5.5)", actions[0].Detail)
	}
}

func TestProposeFreeMemoryRunbook(t *testing.T) {
	db := openDB(t)
	total := int64(120 << 30)
	snap := snapWith(collector.Metrics{
		GTTUsedBytes:  int64p(int64(float64(total) * 0.97)),
		GTTTotalBytes: int64p(total),
	})
	s := New(Deps{Store: db, Source: collector.NewStatic(snap)})

	if _, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	actions, err := s.ListActions(context.Background(), StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	var found bool
	for _, a := range actions {
		if a.Kind == KindRunbook && a.DedupeKey == KindRunbook+":gtt_ceiling" {
			found = true
			if !strings.Contains(string(a.Detail), `"check_id":"gtt_ceiling"`) {
				t.Errorf("detail = %s, want the source check id stamped (§5.5)", a.Detail)
			}
		}
	}
	if !found {
		t.Errorf("no gtt_ceiling runbook proposal among %+v", actions)
	}
}

// ── dedupe / supersede / cap ─────────────────────────────────────────────

func TestProposeDedupeReusesPendingRow(t *testing.T) {
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: false}
	s := New(Deps{Store: db, Source: collector.NewStatic(snap), Cfg: func() *config.Config { return cfg }})
	ctx := context.Background()

	if _, err := s.RunChecks(ctx, ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks #1: %v", err)
	}
	first, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("no proposals created on first sweep")
	}

	if _, err := s.RunChecks(ctx, ScopeQuick, nil, SweepManual); err != nil {
		t.Fatalf("RunChecks #2: %v", err)
	}
	second, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(second) != len(first) {
		t.Errorf("pending count grew from %d to %d across an unchanged repeat sweep — dedupe not reusing rows",
			len(first), len(second))
	}
	// Same IDs, not new rows.
	firstIDs := map[int64]bool{}
	for _, a := range first {
		firstIDs[a.ID] = true
	}
	for _, a := range second {
		if !firstIDs[a.ID] {
			t.Errorf("action %d is a new row, want the same row reused across sweeps", a.ID)
		}
	}
}

func TestProposeSupersedeOnChangedDetail(t *testing.T) {
	db := openDB(t)
	cmdline := filepath.Join(t.TempDir(), "cmdline")
	writeCmdline := func(content string) {
		if err := os.WriteFile(cmdline, []byte(content), 0o644); err != nil {
			t.Fatalf("write cmdline: %v", err)
		}
	}
	writeCmdline("BOOT_IMAGE=/vmlinuz amdgpu.mcbp=0 quiet") // missing vm_fragment_size only
	s := New(Deps{Store: db, CmdlinePath: cmdline})
	ctx := context.Background()

	if _, err := s.RunChecks(ctx, "", []string{"kernel_params"}, SweepManual); err != nil {
		t.Fatalf("RunChecks #1: %v", err)
	}
	first, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("ListActions after #1 = %+v, err=%v, want exactly 1", first, err)
	}

	writeCmdline("BOOT_IMAGE=/vmlinuz quiet") // now BOTH params missing — different detail
	if _, err := s.RunChecks(ctx, "", []string{"kernel_params"}, SweepManual); err != nil {
		t.Fatalf("RunChecks #2: %v", err)
	}

	old, err := s.GetAction(ctx, first[0].ID)
	if err != nil {
		t.Fatalf("GetAction(old): %v", err)
	}
	if old.Status != StatusSuperseded {
		t.Errorf("old proposal status = %s, want superseded", old.Status)
	}

	pending, err := s.ListActions(ctx, StatusPending, nil, 0)
	if err != nil {
		t.Fatalf("ListActions(pending): %v", err)
	}
	if len(pending) != 1 || pending[0].ID == first[0].ID {
		t.Errorf("pending after supersede = %+v, want exactly one NEW row", pending)
	}
}

func TestProposeCapAt20(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		unit := fmt.Sprintf("headroom@proxy%d", i)
		if err := db.Routing().SaveProxy(ctx, store.ProxyRow{
			Service: fmt.Sprintf("proxy%d", i), Port: 9000 + i,
			TargetURL: "http://127.0.0.1:1", Unit: unit,
		}); err != nil {
			t.Fatalf("SaveProxy(%d): %v", i, err)
		}
	}
	snap := snapWith(collector.Metrics{}) // every headroom@proxyN unit absent -> down
	s := New(Deps{
		Store: db, Source: collector.NewStatic(snap), Logf: func(string, ...any) {},
		Cfg: func() *config.Config { return &config.Config{} },
	})

	if _, err := s.RunChecks(ctx, "", []string{"compressor_reachability"}, SweepManual); err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	n, err := s.PendingActionCount(ctx)
	if err != nil {
		t.Fatalf("PendingActionCount: %v", err)
	}
	if n != maxAutoOpenProposals {
		t.Errorf("pending count = %d, want capped at %d", n, maxAutoOpenProposals)
	}
}

// ── ProposalIDs stamped on the returned Finding ─────────────────────────────

func TestProposeStampsFindingProposalIDs(t *testing.T) {
	db := openDB(t)
	cfg := &config.Config{Ports: map[string]int{"stt": 8084}}
	snap := snapWith(collector.Metrics{})
	snap.Ports = map[int]bool{8084: false}
	s := New(Deps{Store: db, Source: collector.NewStatic(snap), Cfg: func() *config.Config { return cfg }})

	findings, err := s.RunChecks(context.Background(), ScopeQuick, nil, SweepManual)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	var portsFinding *Finding
	for i := range findings {
		if findings[i].CheckID == "always_on_ports" {
			portsFinding = &findings[i]
		}
	}
	if portsFinding == nil {
		t.Fatal("always_on_ports finding missing from sweep result")
	}
	if len(portsFinding.ProposalIDs) == 0 {
		t.Errorf("finding.ProposalIDs = %v, want at least one proposal ID", portsFinding.ProposalIDs)
	}
}

// TestProposeRebuildRunbook_BlastRadiusDisclosed pins the proposal-time
// half of the G8 fix (first live build_refresh eval, 2026-08-20): the
// tracked binary "llama.cpp (vulkan)" actually backed nemotron's rocm
// build, sharing its tree with other builds — and only a human live
// inspection caught it. A build-refresh proposal must enumerate the
// catalog builds the tree really backs, with each one's consumer configs,
// so an operator approves against named reality, never a label.
func TestProposeRebuildRunbook_BlastRadiusDisclosed(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	cat := db.Catalog()
	seedBrainCatalog(t, db)
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	root := "/x/fork"
	if _, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-vulkan", BinaryPath: root + "/build-vulkan/bin/llama-server", Backend: "vulkan",
	}); err != nil {
		t.Fatalf("CreateBuild vulkan: %v", err)
	}
	rocmID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-rocm", BinaryPath: root + "/build/bin/llama-server", Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild rocm: %v", err)
	}
	// Give the rocm build a real consumer so the blast radius has a name.
	cfg, err := cat.ConfigByName(ctx, "ornith-35b")
	if err != nil {
		t.Fatalf("ConfigByName: %v", err)
	}
	cfg.BuildID = rocmID
	if err := cat.UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	env := &CheckEnv{Catalog: cat}
	f := Finding{
		CheckID: "binary_versions", Severity: SeverityInfo,
		Evidence: map[string]any{"binaries": []binaryStatus{{
			Name: "fork (vulkan)", Kind: "llama_build", Path: root + "/build/bin/llama-server",
			SourceRef: root, UpstreamRef: "origin/master", UpstreamAhead: 4,
		}}},
	}
	drafts := proposeRebuildRunbook(withBehindThreshold(env), f, BrainResolution{})
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	var detail struct {
		BlastRadius []blastRadiusBuild `json:"blast_radius"`
		Steps       []RunbookStep      `json:"steps"`
	}
	if err := json.Unmarshal(drafts[0].Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if len(detail.BlastRadius) != 2 {
		t.Fatalf("blast_radius = %+v, want both builds under the tree", detail.BlastRadius)
	}
	var sawRocmConsumer bool
	for _, b := range detail.BlastRadius {
		switch b.Name {
		case "standard-rocm":
			if len(b.Configs) != 1 || b.Configs[0] != "ornith-35b" {
				t.Errorf("standard-rocm configs = %v, want [ornith-35b]", b.Configs)
			}
			sawRocmConsumer = true
		case "standard-vulkan":
			if len(b.Configs) != 0 {
				t.Errorf("standard-vulkan configs = %v, want none", b.Configs)
			}
		default:
			t.Errorf("unexpected blast-radius build %+v", b)
		}
	}
	if !sawRocmConsumer {
		t.Errorf("blast radius never named the rocm build's consumer: %+v", detail.BlastRadius)
	}
	if len(detail.Steps) == 0 || !strings.Contains(detail.Steps[0].Why, "backs 2 catalog build(s)") {
		t.Errorf("first step Why = %q, want the human-readable blast-radius note", detail.Steps[0].Why)
	}
}

// TestProposeRebuildRunbook_NilCatalogProposesWithoutBlastRadius — the
// disclosure must degrade to silence, never to a crash or a false "no
// impact" claim, when the catalog seam is unavailable.
func TestProposeRebuildRunbook_NilCatalogProposesWithoutBlastRadius(t *testing.T) {
	f := Finding{
		CheckID: "binary_versions", Severity: SeverityInfo,
		Evidence: map[string]any{"binaries": []binaryStatus{{
			Name: "fork (vulkan)", Kind: "llama_build", Path: "/x/fork/build/bin/llama-server",
			SourceRef: "/x/fork", UpstreamRef: "origin/master", UpstreamAhead: 2,
		}}},
	}
	drafts := proposeRebuildRunbook(withBehindThreshold(&CheckEnv{}), f, BrainResolution{})
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1 (an unreadable catalog must not suppress the proposal)", len(drafts))
	}
	var detail struct {
		BlastRadius []blastRadiusBuild `json:"blast_radius"`
		Steps       []RunbookStep      `json:"steps"`
	}
	if err := json.Unmarshal(drafts[0].Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if len(detail.BlastRadius) != 0 {
		t.Fatalf("blast_radius = %+v, want empty when the catalog is unwired", detail.BlastRadius)
	}
	if len(detail.Steps) == 0 || strings.Contains(detail.Steps[0].Why, "backs") {
		t.Errorf("first step Why = %q, must not claim any blast radius it could not verify", detail.Steps[0].Why)
	}
}

// withBehindThreshold sets a permissive drift threshold for mechanism tests
// whose fixtures carry small UpstreamAhead values.
func withBehindThreshold(env *CheckEnv) *CheckEnv {
	env.Thresholds.BuildRefreshBehindN = 1
	return env
}

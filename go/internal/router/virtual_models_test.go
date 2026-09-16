// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeVirtualModelRegistry is a minimal registry.Registry for
// throughputVirtualCandidates tests — only Cards is exercised.
type fakeVirtualModelRegistry struct {
	cards []registry.ConfigCard
}

func (f *fakeVirtualModelRegistry) Cards(context.Context, time.Time) ([]registry.ConfigCard, error) {
	return f.cards, nil
}
func (f *fakeVirtualModelRegistry) ModelCards(context.Context, time.Time) ([]registry.Card, error) {
	return nil, nil
}
func (f *fakeVirtualModelRegistry) WeightEstimateBytes(int64) (int64, bool) { return 0, false }
func (f *fakeVirtualModelRegistry) CostPer1k(int64) float64                 { return 0 }
func (f *fakeVirtualModelRegistry) PowerEstPer1m(int64) (float64, bool)     { return 0, false }

func tps(v float64) *float64 { return &v }

func seedVirtualModelConfig(t *testing.T, db *store.DB, name string, tierID, rank int64) store.Config {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()
	mdlID, err := cat.CreateModel(ctx, store.Model{Name: name + "-model"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "v"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	gguf, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: gguf.ID, ArtifactType: "weight",
		FilePath: name + ".gguf", FileSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	cfgID, err := cat.CreateConfig(ctx, store.Config{
		Name: name, VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID,
		NCtx: 8192, Parallel: 1, Status: "unverified", Visibility: "visible",
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if tierID != 0 {
		if err := cat.UpdateConfigCapabilityTier(ctx, cfgID, tierID, rank); err != nil {
			t.Fatalf("UpdateConfigCapabilityTier: %v", err)
		}
	}
	cfg, err := cat.GetConfig(ctx, cfgID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	return cfg
}

func TestCapabilityTierVirtualCandidates_RankOrderAndHiddenExcluded(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	tierID, err := cat.CreateCapabilityTier(ctx, store.CapabilityTier{Name: "t"})
	if err != nil {
		t.Fatalf("CreateCapabilityTier: %v", err)
	}
	c3 := seedVirtualModelConfig(t, db, "rank3", tierID, 3)
	c1 := seedVirtualModelConfig(t, db, "rank1", tierID, 1)
	_ = seedVirtualModelConfig(t, db, "rank2-hidden", tierID, 2)
	if err := cat.UpdateConfig(ctx, func() store.Config {
		hidden, _ := cat.ConfigByName(ctx, "rank2-hidden")
		hidden.Visibility = "hidden"
		return hidden
	}()); err != nil {
		t.Fatalf("UpdateConfig (hide): %v", err)
	}

	s := &Server{deps: Deps{StoreCatalog: cat}}
	got := s.capabilityTierVirtualCandidates(ctx, store.VirtualModel{Kind: "capability_tier", CapabilityTierID: tierID})
	if len(got) != 2 {
		t.Fatalf("want 2 visible candidates, got %d: %+v", len(got), got)
	}
	if got[0].Name != c1.Name || got[1].Name != c3.Name {
		t.Fatalf("want rank order [rank1, rank3], got [%s, %s]", got[0].Name, got[1].Name)
	}
}

func TestThroughputVirtualCandidates_ExcludesUnprofiledAndSortsDescending(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	fast := seedVirtualModelConfig(t, db, "fast-config", 0, 0)
	slow := seedVirtualModelConfig(t, db, "slow-config", 0, 0)
	unprofiled := seedVirtualModelConfig(t, db, "unprofiled-config", 0, 0)

	reg := &fakeVirtualModelRegistry{cards: []registry.ConfigCard{
		{ID: fast.ID, Name: fast.Name, Visibility: "visible", Performance: registry.Performance{MeasuredTS: tps(97.2)}},
		{ID: slow.ID, Name: slow.Name, Visibility: "visible", Performance: registry.Performance{MeasuredTS: tps(6.4)}},
		{ID: unprofiled.ID, Name: unprofiled.Name, Visibility: "visible", Performance: registry.Performance{MeasuredTS: nil}},
	}}

	s := &Server{deps: Deps{StoreCatalog: cat, Registry: reg}}
	got := s.throughputVirtualCandidates(ctx)
	if len(got) != 2 {
		t.Fatalf("want 2 profiled candidates (unprofiled excluded), got %d: %+v", len(got), got)
	}
	if got[0].Name != fast.Name || got[1].Name != slow.Name {
		t.Fatalf("want descending tps order [fast, slow], got [%s, %s]", got[0].Name, got[1].Name)
	}
}

func TestResolveVirtualModel_PrefersLoadedThenFallsBackToRank1(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	tierID, err := cat.CreateCapabilityTier(ctx, store.CapabilityTier{Name: "t"})
	if err != nil {
		t.Fatalf("CreateCapabilityTier: %v", err)
	}
	rank1 := seedVirtualModelConfig(t, db, "rank1", tierID, 1)
	rank2 := seedVirtualModelConfig(t, db, "rank2", tierID, 2)
	if _, err := cat.CreateVirtualModel(ctx, store.VirtualModel{Name: "local-smart", Kind: "capability_tier", CapabilityTierID: tierID}); err != nil {
		t.Fatalf("CreateVirtualModel: %v", err)
	}
	if _, err := cat.CreateVirtualModel(ctx, store.VirtualModel{Name: "local-hidden", Kind: "capability_tier", CapabilityTierID: tierID, Visibility: "hidden"}); err != nil {
		t.Fatalf("CreateVirtualModel (hidden): %v", err)
	}

	t.Run("nothing loaded falls back to rank1", func(t *testing.T) {
		fs := &capability_substitutionSched{slots: map[string]string{"a1": ""}}
		s := &Server{deps: Deps{StoreCatalog: cat, Sched: fs}}
		got, ok := s.resolveVirtualModel(ctx, "local-smart")
		if !ok || got.Name != rank1.Name {
			t.Fatalf("want (rank1, true), got (%+v, %v)", got, ok)
		}
	})

	t.Run("prefers the loaded lower-priority member over an unloaded rank1", func(t *testing.T) {
		fs := &capability_substitutionSched{slots: map[string]string{"a1": rank2.Name}}
		s := &Server{deps: Deps{StoreCatalog: cat, Sched: fs}}
		got, ok := s.resolveVirtualModel(ctx, "local-smart")
		if !ok || got.Name != rank2.Name {
			t.Fatalf("want (rank2, true) since it's already loaded, got (%+v, %v)", got, ok)
		}
	})

	t.Run("hidden virtual model never resolves", func(t *testing.T) {
		fs := &capability_substitutionSched{slots: map[string]string{"a1": ""}}
		s := &Server{deps: Deps{StoreCatalog: cat, Sched: fs}}
		if _, ok := s.resolveVirtualModel(ctx, "local-hidden"); ok {
			t.Fatalf("hidden virtual model must never resolve")
		}
	})

	t.Run("unknown name never resolves", func(t *testing.T) {
		fs := &capability_substitutionSched{slots: map[string]string{"a1": ""}}
		s := &Server{deps: Deps{StoreCatalog: cat, Sched: fs}}
		if _, ok := s.resolveVirtualModel(ctx, "not-a-real-virtual-model"); ok {
			t.Fatalf("unknown name must never resolve")
		}
	})
}

// TestVirtualModel_EndToEndThroughCatalogChain drives a real
// /v1/chat/completions request for a virtual model name through the full
// substitutionHarness (the same one capability_substitution_test.go uses) —
// confirms resolution isn't just correct in isolation but actually reaches
// EnsureLoaded and gets served, exactly like a direct config-name request.
func TestVirtualModel_EndToEndThroughCatalogChain(t *testing.T) {
	h := newSubstitutionHarness(t, "")
	h.seed("weak", 10)
	h.seed("strong", 0)
	if _, err := h.db.Catalog().CreateVirtualModel(context.Background(), store.VirtualModel{
		Name: "local-smart", Kind: "capability_tier", CapabilityTierID: h.classID,
	}); err != nil {
		t.Fatalf("CreateVirtualModel: %v", err)
	}
	// Nothing loaded yet — resolution must pick "strong" (rank 0, most
	// capable) and this request must trigger a real load for it.
	h.sched.ensureResult["strong"] = sched.Ticket{Status: "loaded", TargetSlot: "a2"}

	rec := h.request("local-smart", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var sawStrong bool
	for _, m := range h.sched.calls {
		if m == "strong" {
			sawStrong = true
		}
		if m == "weak" {
			t.Errorf("EnsureLoaded(weak) was called — local-smart must never resolve to the lower-ranked member")
		}
	}
	if !sawStrong {
		t.Errorf("EnsureLoaded(strong) was never called — local-smart failed to trigger a load for its resolved target")
	}
}

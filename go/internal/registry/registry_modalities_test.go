// SPDX-License-Identifier: Apache-2.0

package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Modalities (Sprint J1) ───────────────────────────────────────────────────
//
// seedModalityCatalog is a lighter-weight sibling of seedFullCatalog, purpose
// -built to control the two knobs resolveModalities branches on: the model's
// own Modalities and the config's MMProjArtifactID/Modalities/mmproj
// Missing flag. mmprojMissing/hasMmproj/cfgOverride let each test exercise
// exactly one of the four resolveModalities branches.
func seedModalityCatalog(t *testing.T, ctx context.Context, cat store.Catalog, modelModalities []string, hasMmproj, mmprojMissing bool, cfgOverride *[]string) int64 {
	t.Helper()
	mdlID, _ := cat.CreateModel(ctx, store.Model{
		Name: "Test Omni Model", Modalities: modelModalities,
	})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt_, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt_.ID, FilePath: "model.gguf", ArtifactType: "weight",
	})
	var mmprojID int64
	if hasMmproj {
		mmprojID, _ = cat.CreateArtifact(ctx, store.Artifact{
			VariantID: varID, FormatID: fmt_.ID, FilePath: "mmproj.gguf",
			IsAuxiliary: true, ArtifactType: "mmproj", Missing: mmprojMissing,
		})
	}
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	configID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "test-omni", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, MMProjArtifactID: mmprojID, NCtx: 4096,
		Status: "unverified", Visibility: "visible", Modalities: cfgOverride,
	})
	return configID
}

func modalityGapIDs(gaps []registry.ModalityGap) []string {
	out := make([]string, len(gaps))
	for i, g := range gaps {
		out[i] = g.ID
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConfigCards_Modalities_NoMmproj_Unavailable: no mmproj linked at all →
// only text is enabled, every other model-level modality is unavailable
// with reason "no mmproj linked" — the 12-of-17 real-config case on ForgeHost.
func TestConfigCards_Modalities_NoMmproj_Unavailable(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision"}, false, false, nil)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	c := cards[0]
	if !stringSlicesEqual(c.Modalities, []string{"text"}) {
		t.Errorf("Modalities: got %+v, want [text]", c.Modalities)
	}
	gotGaps := modalityGapIDs(c.ModalitiesUnavailable)
	if !stringSlicesEqual(gotGaps, []string{"vision"}) {
		t.Errorf("ModalitiesUnavailable IDs: got %+v, want [vision]", gotGaps)
	}
	if c.ModalitiesUnavailable[0].Reason != "no mmproj linked" {
		t.Errorf("reason: got %q", c.ModalitiesUnavailable[0].Reason)
	}
}

// TestConfigCards_Modalities_MissingMmprojFile: mmproj is linked but the
// artifact itself is marked Missing (no longer on disk) — same unavailable
// treatment as no-mmproj-linked, different reason.
func TestConfigCards_Modalities_MissingMmprojFile(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision", "audio"}, true, true, nil)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.Cards(ctx, time.Time{})
	c := cards[0]
	if !stringSlicesEqual(c.Modalities, []string{"text"}) {
		t.Errorf("Modalities: got %+v, want [text]", c.Modalities)
	}
	gotGaps := modalityGapIDs(c.ModalitiesUnavailable)
	if !stringSlicesEqual(gotGaps, []string{"vision", "audio"}) {
		t.Errorf("ModalitiesUnavailable IDs: got %+v, want [vision audio]", gotGaps)
	}
	for _, g := range c.ModalitiesUnavailable {
		if g.Reason != "mmproj file missing on disk" {
			t.Errorf("reason for %q: got %q", g.ID, g.Reason)
		}
	}
}

// TestConfigCards_Modalities_InheritFromModel: a real, present mmproj and no
// override → the config inherits the model's full modality list verbatim,
// nothing unavailable.
func TestConfigCards_Modalities_InheritFromModel(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision", "audio"}, true, false, nil)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.Cards(ctx, time.Time{})
	c := cards[0]
	if !stringSlicesEqual(c.Modalities, []string{"text", "vision", "audio"}) {
		t.Errorf("Modalities: got %+v, want [text vision audio]", c.Modalities)
	}
	if len(c.ModalitiesUnavailable) != 0 {
		t.Errorf("ModalitiesUnavailable: got %+v, want none", c.ModalitiesUnavailable)
	}
}

// TestConfigCards_Modalities_ExplicitOverrideWins is the live Nemotron-Omni
// case: a model that architecturally supports audio, but whose deployed
// build can't yet — the config's explicit override narrows the inherited
// set and reports no gap (an operator's explicit assertion isn't a "gap",
// it's a deliberate fact).
func TestConfigCards_Modalities_ExplicitOverrideWins(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	override := []string{"text", "vision"}
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision", "audio"}, true, false, &override)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.Cards(ctx, time.Time{})
	c := cards[0]
	if !stringSlicesEqual(c.Modalities, []string{"text", "vision"}) {
		t.Errorf("Modalities: got %+v, want [text vision] (override, no audio)", c.Modalities)
	}
	if len(c.ModalitiesUnavailable) != 0 {
		t.Errorf("ModalitiesUnavailable: got %+v, want none (explicit override isn't a gap)", c.ModalitiesUnavailable)
	}
}

// TestConfigCards_Modalities_ExplicitEmptyOverride: an explicit empty
// override (an operator asserting "text only") must be distinguishable from
// "no override" (nil) — this is exactly why Config.Modalities is a pointer,
// not a bare slice.
func TestConfigCards_Modalities_ExplicitEmptyOverride(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	override := []string{}
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision"}, true, false, &override)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.Cards(ctx, time.Time{})
	c := cards[0]
	if !stringSlicesEqual(c.Modalities, []string{"text"}) {
		t.Errorf("Modalities: got %+v, want [text] (explicit empty override)", c.Modalities)
	}
}

// TestModelCards_Modalities: the model-scoped Card always carries the
// model's own architectural modalities, independent of any one config's
// ability to deliver them (unlike ConfigCard, which narrows).
func TestModelCards_Modalities(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	// No mmproj at all -- if Card.Modalities were wrongly narrowed the same
	// way ConfigCard is, this would come back [text] instead of the full
	// architectural list.
	seedModalityCatalog(t, ctx, db.Catalog(), []string{"text", "vision", "audio"}, false, false, nil)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if !stringSlicesEqual(cards[0].Modalities, []string{"text", "vision", "audio"}) {
		t.Errorf("Modalities: got %+v, want [text vision audio]", cards[0].Modalities)
	}
}

// TestModalities_RoundTripThroughStore is the dead-column regression guard
// (PriceCachedInPer1M's 2026-07-31 trap): Create -> Get -> Update -> Get
// must carry Modalities through every hop, for both Model and Config.
func TestModalities_RoundTripThroughStore(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	cat := db.Catalog()

	mdlID, err := cat.CreateModel(ctx, store.Model{Name: "RT Model", Modalities: []string{"text", "vision"}})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	got, err := cat.GetModel(ctx, mdlID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if !stringSlicesEqual(got.Modalities, []string{"text", "vision"}) {
		t.Fatalf("GetModel after create: Modalities = %+v", got.Modalities)
	}

	got.Modalities = []string{"text", "vision", "audio"}
	if err := cat.UpdateModel(ctx, got); err != nil {
		t.Fatalf("UpdateModel: %v", err)
	}
	got2, err := cat.GetModel(ctx, mdlID)
	if err != nil {
		t.Fatalf("GetModel after update: %v", err)
	}
	if !stringSlicesEqual(got2.Modalities, []string{"text", "vision", "audio"}) {
		t.Fatalf("GetModel after update: Modalities = %+v, want [text vision audio] (dead-column regression)", got2.Modalities)
	}

	// Config: nil -> explicit override -> nil again, through Create/Update/Get.
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base"})
	fmt_, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: varID, FormatID: fmt_.ID, FilePath: "w.gguf", ArtifactType: "weight"})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	cfgID, err := cat.CreateConfig(ctx, store.Config{
		Name: "rt-config", VariantID: varID, WeightArtifactID: weightID, EngineID: eng.ID, NCtx: 4096,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	gotCfg, err := cat.GetConfig(ctx, cfgID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if gotCfg.Modalities != nil {
		t.Fatalf("GetConfig after create with no override: Modalities = %+v, want nil", gotCfg.Modalities)
	}

	override := []string{"text", "vision"}
	gotCfg.Modalities = &override
	if err := cat.UpdateConfig(ctx, gotCfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	gotCfg2, err := cat.GetConfig(ctx, cfgID)
	if err != nil {
		t.Fatalf("GetConfig after update: %v", err)
	}
	if gotCfg2.Modalities == nil || !stringSlicesEqual(*gotCfg2.Modalities, []string{"text", "vision"}) {
		t.Fatalf("GetConfig after override update: Modalities = %+v, want [text vision] (dead-column regression)", gotCfg2.Modalities)
	}

	gotCfg2.Modalities = nil
	if err := cat.UpdateConfig(ctx, gotCfg2); err != nil {
		t.Fatalf("UpdateConfig (clear override): %v", err)
	}
	gotCfg3, err := cat.GetConfig(ctx, cfgID)
	if err != nil {
		t.Fatalf("GetConfig after clearing override: %v", err)
	}
	if gotCfg3.Modalities != nil {
		t.Fatalf("GetConfig after clearing override: Modalities = %+v, want nil (back to derive)", gotCfg3.Modalities)
	}
}

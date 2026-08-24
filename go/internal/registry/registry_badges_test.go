// SPDX-License-Identifier: Apache-2.0

package registry_test

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Badges (Sprint J2 vocabulary overhaul) ───────────────────────────────────
//
// seedBadgeCatalog is a lighter-weight sibling of seedFullCatalog/
// seedModalityCatalog, purpose-built to control the three inputs deriveBadges
// combines: key_features, is_abliterated, and modalities. hasMmproj controls
// whether the config inherits the model's modalities (mirrors
// seedModalityCatalog) or narrows to text-only — the exact mechanism the
// config-scoped-vs-model-scoped badge test below depends on.
func seedBadgeCatalog(t *testing.T, ctx context.Context, cat store.Catalog, keyFeatures []string, isAbliterated bool, modelModalities []string, hasMmproj bool) int64 {
	t.Helper()
	mdlID, _ := cat.CreateModel(ctx, store.Model{
		Name: "Badge Test Model", KeyFeatures: keyFeatures, Modalities: modelModalities,
	})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "base", IsAbliterated: isAbliterated})
	fmt_, _ := cat.FormatByName(ctx, "GGUF")
	weightID, _ := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: fmt_.ID, FilePath: "model.gguf", ArtifactType: "weight",
	})
	var mmprojID int64
	if hasMmproj {
		mmprojID, _ = cat.CreateArtifact(ctx, store.Artifact{
			VariantID: varID, FormatID: fmt_.ID, FilePath: "mmproj.gguf",
			IsAuxiliary: true, ArtifactType: "mmproj",
		})
	}
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	configID, _ := cat.CreateConfig(ctx, store.Config{
		Name: "badge-test", VariantID: varID, WeightArtifactID: weightID,
		EngineID: eng.ID, MMProjArtifactID: mmprojID, NCtx: 4096,
		Status: "unverified", Visibility: "visible",
	})
	return configID
}

func badgeIDs(badges []registry.Badge) []string {
	out := make([]string, len(badges))
	for i, b := range badges {
		out[i] = b.ID
	}
	return out
}

// TestModelCards_Badges_SuppressedFeaturesProduceNothing: fast/multimodal/
// moe/mtp/dense must vanish entirely — no glyph AND no generic text-pill
// fallback (unlike a genuinely unrecognized feature).
func TestModelCards_Badges_SuppressedFeaturesProduceNothing(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), []string{"Fast", "MoE", "Dense", "Multimodal", "MTP"}, false, nil, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if len(cards[0].Badges) != 0 {
		t.Errorf("Badges: got %+v, want none (all 5 suppressed)", cards[0].Badges)
	}
}

// TestModelCards_Badges_SuppressedVsUnrecognized: a suppressed feature next
// to a genuinely unrecognized one — only the unrecognized one survives, as
// a text pill.
func TestModelCards_Badges_SuppressedVsUnrecognized(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), []string{"Fast", "Genomics"}, false, nil, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"text:genomics"}) {
		t.Errorf("Badges: got %+v, want [text:genomics] (Fast suppressed, Genomics kept as text pill)", got)
	}
	if cards[0].Badges[0].Label != "Genomics" {
		t.Errorf("pill label: got %q, want original-case %q", cards[0].Badges[0].Label, "Genomics")
	}
}

// TestModelCards_Badges_StaleVisionKeyFeatureSuppressed is a live-deploy
// regression guard: Nemotron 3 Nano Omni's real catalog data carries a
// literal "Vision" key_feature string that predates the J1 typed modalities
// column. Since vision/hearing badges are synthesized from modalities (not
// key_features) by design, that stale string must be suppressed the same as
// "multimodal" — not fall through as a redundant "text:vision" pill next to
// the real modality-derived vision glyph.
func TestModelCards_Badges_StaleVisionKeyFeatureSuppressed(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), []string{"Vision", "Reasoning", "Fast"}, false, []string{"text", "vision", "audio"}, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"reasoning", "vision", "hearing"}) {
		t.Errorf("Badges: got %+v, want [reasoning vision hearing] (no duplicate text:vision pill, Fast suppressed)", got)
	}
}

// TestModelCards_Badges_VisionHearingFromModalities: vision/hearing are
// synthesized from modalities, not key_features strings — a model with no
// "vision"/"hearing" key_feature at all still gets both glyphs.
func TestModelCards_Badges_VisionHearingFromModalities(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), nil, false, []string{"text", "vision", "audio"}, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"vision", "hearing"}) {
		t.Errorf("Badges: got %+v, want [vision hearing] (priority order)", got)
	}
}

// TestModelCards_Badges_PriorityOrdering: regardless of input order, glyph
// badges come out sorted reasoning -> vision/hearing -> coding -> uncensored
// -> long-context.
func TestModelCards_Badges_PriorityOrdering(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	// Deliberately out-of-priority-order input.
	seedBadgeCatalog(t, ctx, db.Catalog(), []string{"long context", "coding", "reasoning"}, false, nil, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"reasoning", "coding", "long-context"}) {
		t.Errorf("Badges: got %+v, want [reasoning coding long-context]", got)
	}
}

// TestModelCards_Badges_FourGlyphCap: with all 6 glyph categories earned at
// once, only the top 4 by priority survive — the dropped two (uncensored,
// long-context, the two lowest-priority tiers) vanish entirely, same as a
// suppressed feature (no glyph, no pill fallback). No real catalog model
// currently earns more than 3 badges — this is a defensive cap, exercised
// only synthetically here, never live.
func TestModelCards_Badges_FourGlyphCap(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx,
		db.Catalog(),
		[]string{"reasoning", "coding", "long context"}, true, // is_abliterated adds uncensored
		[]string{"text", "vision", "audio"}, false,
	)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"reasoning", "vision", "hearing", "coding"}) {
		t.Errorf("Badges: got %+v, want capped to [reasoning vision hearing coding] (uncensored, long-context dropped)", got)
	}
}

// TestModelCards_Badges_AbliteratedAndUncensoredDedupe: is_abliterated=true
// plus both "Abliterated" and "Uncensored" key_feature strings all collapse
// onto the single retired-name "uncensored" badge (Sprint J2 renamed the
// abliterated slug/label away).
func TestModelCards_Badges_AbliteratedAndUncensoredDedupe(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), []string{"Abliterated", "Uncensored"}, true, nil, false)
	reg := registry.New(db.Catalog(), nil, nil)

	cards, _ := reg.ModelCards(ctx, time.Time{})
	got := badgeIDs(cards[0].Badges)
	if !stringSlicesEqual(got, []string{"uncensored"}) {
		t.Errorf("Badges: got %+v, want [uncensored] (deduped, retired abliterated slug)", got)
	}
	if cards[0].Badges[0].Label != "Uncensored" || cards[0].Badges[0].Icon != "uncensored" {
		t.Errorf("badge shape: got %+v", cards[0].Badges[0])
	}
}

// TestCards_Badges_ConfigScopedVisionNarrowsWithMmproj is the plan's
// explicit requirement: config-scoped cards derive vision/hearing from the
// CONFIG's effective (narrowed) modalities, not the model's raw ones — a
// config with no mmproj must not show a vision badge even though its model
// architecturally supports it, while the model-scoped Card (unaffected by
// any one config) still does.
func TestCards_Badges_ConfigScopedVisionNarrowsWithMmproj(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	seedBadgeCatalog(t, ctx, db.Catalog(), nil, false, []string{"text", "vision"}, false /* no mmproj */)
	reg := registry.New(db.Catalog(), nil, nil)

	modelCards, err := reg.ModelCards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ModelCards: %v", err)
	}
	if got := badgeIDs(modelCards[0].Badges); !stringSlicesEqual(got, []string{"vision"}) {
		t.Errorf("model-scoped Badges: got %+v, want [vision] (model's own architecture)", got)
	}

	configCards, err := reg.Cards(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Cards: %v", err)
	}
	if got := badgeIDs(configCards[0].Badges); len(got) != 0 {
		t.Errorf("config-scoped Badges: got %+v, want none (no mmproj linked -> vision unavailable, not badged)", got)
	}
}

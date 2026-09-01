// SPDX-License-Identifier: Apache-2.0

package smith

// intents_test.go — the Sprint S2-Go coverage-matrix test. Reads Sprint R's
// catalog fixture (testdata/smith_intent_catalog.json) and verifies the
// classifier matches every answerable family × entity pair, every known gap
// answers honestly, and the 19 seed regressions map to the expected family.
// A newly tracked entity missing from the classifier should fail a test,
// not silently go unaskable (§5.3 DoD).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// catalogFixture mirrors testdata/smith_intent_catalog.json (the subset the
// coverage matrix reads). Kept unexported + minimal — this test owns its
// shape, not the wire freeze.
type catalogFixture struct {
	Entries []struct {
		Family     string   `json:"family"`
		Entity     string   `json:"entity"`
		Phrasings  []string `json:"phrasings"`
		Answerable bool     `json:"answerable"`
		Notes      string   `json:"notes"`
	} `json:"entries"`
	Seeds []struct {
		Seed     int    `json:"seed"`
		Question string `json:"question"`
		Entry    string `json:"entry"`
		Gap      bool   `json:"gap"`
	} `json:"seeds"`
}

func loadCatalogFixture(t *testing.T) catalogFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/smith_intent_catalog.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f catalogFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

// classifySmith builds a Smith with the curated default entities (no live
// deps) — sufficient for Classify over the code-curated families (health,
// quantity, probes) when no settings/sched/cfg are wired.
func classifySmith(t *testing.T) *Smith {
	t.Helper()
	return New(Deps{Logf: func(string, ...any) {}})
}

// fixtureSmith builds a Smith over a real in-memory store whose layer-2
// seams are provisioned from the SHIPPED SYNTHETIC example deployment
// (docs/examples/smith-local-seed.example.json via ImportLocalSeed — the
// same seam a real install uses for its own file). The fixture's mesh
// entities follow that example; migrations deliberately seed the settings
// EMPTY (two-layer knowledge architecture 2026-08-21: deployment data
// never ships, so no compiled-in defaults to classify against).
func fixtureSmith(t *testing.T) *Smith {
	t.Helper()
	db := openDB(t)
	importExampleSeed(t, db)
	return New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
}

// importExampleSeed provisions db's smith settings from the shipped
// synthetic example deployment (the two layers' boundary exercised exactly
// the way production exercises it — file → ImportLocalSeed → settings).
func importExampleSeed(t *testing.T, db *store.DB) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRootForTest(t), "docs", "examples", "smith-local-seed.example.json"))
	if err != nil {
		t.Fatalf("read example seed: %v", err)
	}
	if _, err := ImportLocalSeed(context.Background(), db.Settings(), raw); err != nil {
		t.Fatalf("ImportLocalSeed(example): %v", err)
	}
}

// TestCoverageMatrix_ClassifyAnswerable verifies the classifier matches an
// acceptable family (and entity where deterministic) for every answerable
// catalog entry's phrasings. Some phrasings appear under multiple families in
// the fixture (documented overlaps, e.g. "how much gtt is free?" is both
// quantity.gtt and kb.gtt-ceiling) — for those, any fixture-listed family is
// accepted. This is the core §5.3 coverage matrix.
func TestCoverageMatrix_ClassifyAnswerable(t *testing.T) {
	fixture := loadCatalogFixture(t)
	s := fixtureSmith(t)
	ctx := context.Background()

	// Build phrasing → set of acceptable (family, entity) pairs from ALL
	// entries, so overlaps accept any listed family.
	type pair struct{ family, entity string }
	acceptable := map[string]map[pair]bool{}
	for _, e := range fixture.Entries {
		if !e.Answerable {
			continue
		}
		for _, p := range e.Phrasings {
			if acceptable[p] == nil {
				acceptable[p] = map[pair]bool{}
			}
			acceptable[p][pair{e.Family, e.Entity}] = true
		}
	}

	var failures int
	for phrasing, pairs := range acceptable {
		intent := s.Classify(ctx, phrasing)
		got := pair{string(intent.Family), intent.Entity}
		if !pairs[got] {
			// For kb, the entity is a KBSearch-resolved slug — accept any
			// non-empty kb entity (the test can't pin the exact slug the
			// keyword ranker returns).
			if intent.Family == FamilyKB {
				for want := range pairs {
					if want.family == string(FamilyKB) && intent.Entity != "" {
						goto ok
					}
				}
			}
			wantFamilies := make([]string, 0, len(pairs))
			for w := range pairs {
				wantFamilies = append(wantFamilies, w.family+"/"+w.entity)
			}
			t.Errorf("Classify(%q) = %s/%q, want one of %v", phrasing, intent.Family, intent.Entity, wantFamilies)
			failures++
			continue
		}
	ok:
	}
	if failures > 0 {
		t.Fatalf("%d coverage-matrix failures (see above)", failures)
	}
}

// TestCoverageMatrix_KnownGapsAnswerHonestly verifies every answerable=false
// (gap) entry classifies to its family and the answer engine reports it
// can't answer (honest gap).
func TestCoverageMatrix_KnownGapsAnswerHonestly(t *testing.T) {
	fixture := loadCatalogFixture(t)
	s := fixtureSmith(t)
	ctx := context.Background()

	for _, e := range fixture.Entries {
		if e.Answerable {
			continue
		}
		for _, phrasing := range e.Phrasings {
			intent := s.Classify(ctx, phrasing)
			if intent.Family != IntentFamily(e.Family) {
				t.Errorf("gap family mismatch for %q: got %s, want %s", phrasing, intent.Family, e.Family)
				continue
			}
			// For action gaps, the classifier must match the entity so
			// Answer can refuse with the plain-language reason. For version
			// gaps, the entity should match (comfyui/forge).
			if intent.Entity == "" {
				t.Errorf("gap entity empty for %q (expected %q)", phrasing, e.Entity)
				continue
			}
			// The answer engine reports an honest gap / refusal.
			_, ok := s.Answer(ctx, intent)
			if !ok {
				t.Errorf("gap %q: Answer returned !ok (should answer honestly)", phrasing)
			}
		}
	}
}

// TestSeedRegressions verifies the 19 operator seed questions map to the
// expected family (Appendix A's seed exemplars → Sprint R's seed map).
func TestSeedRegressions(t *testing.T) {
	fixture := loadCatalogFixture(t)
	s := fixtureSmith(t)
	ctx := context.Background()

	for _, seed := range fixture.Seeds {
		entry := seed.Entry
		wantFamily := IntentFamily(strings.SplitN(entry, ".", 2)[0])
		intent := s.Classify(ctx, seed.Question)
		if intent.Family != wantFamily {
			t.Errorf("seed #%d %q: family got %s, want %s (entity=%q)", seed.Seed, seed.Question, intent.Family, wantFamily, intent.Entity)
		}
	}
}

// TestClassify_NoMatchOnGeneric verifies generic open-ended questions the
// classifier can't confidently ground fall through to no_match (which routes
// to THINK). This is the conservative-trigger guarantee (§2.2).
func TestClassify_NoMatchOnGeneric(t *testing.T) {
	s := classifySmith(t)
	ctx := context.Background()
	for _, q := range []string{
		"why is the box slow?",
		"tell me about the universe",
		"what should I name my cat?",
		"",
	} {
		intent := s.Classify(ctx, q)
		if intent.Family != FamilyNoMatch {
			t.Errorf("Classify(%q) = %s/%q, want no_match", q, intent.Family, intent.Entity)
		}
	}
}

// TestClassify_IsDeterministic verifies no LLM is anywhere in the classify
// path — a Smith with no brain, no store, no config, no network still
// classifies (the outage guarantee, §2.2).
func TestClassify_IsDeterministic(t *testing.T) {
	s := classifySmith(t)
	ctx := context.Background()
	intent := s.Classify(ctx, "is comfyui healthy?")
	if intent.Family != FamilyHealth || intent.Entity != "comfyui" {
		t.Fatalf("Classify with no deps = %s/%q, want health/comfyui", intent.Family, intent.Entity)
	}
}

// TestClassifyContext_UnitScopedAlertRoutesToCrashedUnit pins the fix for
// the 2026-09-01 incident: a UNIT_CRASH alert carrying the real unit name
// must route to THAT unit's own health entity, not the generic "forge"
// bucket every unit crash fell into before (smith conversation 64 —
// forge-comfyui crashed, got diagnosed via forge_self's unrelated DB-
// integrity check instead).
func TestClassifyContext_UnitScopedAlertRoutesToCrashedUnit(t *testing.T) {
	s := classifySmith(t)
	ctx := context.Background()
	cases := []struct {
		name       string
		code, unit string
		wantEntity string
	}{
		{"comfyui crash", "UNIT_CRASH", "forge-comfyui", "comfyui"},
		{"a2 oom", "UNIT_OOM", "forge-a2", "a2"},
		{"embedding restart", "UNIT_RESTARTED", "forge-embedding", "embedding"},
		{"compressor proxy crash", "UNIT_CRASH", "headroom@deepseek", "compressor"},
		{"forge-daemon itself", "UNIT_CRASH", "forge-daemon", "forge"},
		{"unknown unit falls back to generic code mapping", "UNIT_CRASH", "some-unknown-unit", "forge"},
		{"no unit falls back to generic code mapping", "UNIT_CRASH", "", "forge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := s.classifyWithContext(ctx, "", []ChatContext{{Code: tc.code, Unit: tc.unit, Message: "test"}})
			if intent.Family != FamilyHealth || intent.Entity != tc.wantEntity {
				t.Errorf("code=%s unit=%q => %s/%q, want health/%q", tc.code, tc.unit, intent.Family, intent.Entity, tc.wantEntity)
			}
		})
	}
}

// TestClassifyContext_NonUnitScopedCodeIgnoresUnit proves a Unit riding
// along on a non-unit-scoped alert code (e.g. GTT_HIGH, which has no single
// owning unit) is ignored rather than mis-routed.
func TestClassifyContext_NonUnitScopedCodeIgnoresUnit(t *testing.T) {
	s := classifySmith(t)
	ctx := context.Background()
	intent := s.classifyWithContext(ctx, "", []ChatContext{{Code: "GTT_HIGH", Unit: "forge-comfyui", Message: "test"}})
	if intent.Family != FamilyHealth || intent.Entity != "gtt" {
		t.Errorf("= %s/%q, want health/gtt (GTT_HIGH is not unit-scoped, must ignore the stray Unit field)", intent.Family, intent.Entity)
	}
}

// TestUnitToHealthEntity_TTSUnitIsConfigDriven proves the TTS unit name
// (deployment-specific, cfg.Server.TTSUnit) is resolved dynamically rather
// than only matching the literal "forge-tts" default.
func TestUnitToHealthEntity_TTSUnitIsConfigDriven(t *testing.T) {
	s := New(Deps{Cfg: func() *config.Config {
		return &config.Config{Server: config.Server{TTSUnit: "forge-tts-custom"}}
	}, Logf: func(string, ...any) {}})
	if got := s.unitToHealthEntity("forge-tts-custom"); got != "tts" {
		t.Errorf("unitToHealthEntity(custom TTS unit) = %q, want tts", got)
	}
	if got := s.unitToHealthEntity("forge-tts"); got != "tts" {
		t.Errorf("unitToHealthEntity(default forge-tts) = %q, want tts (default literal must still work)", got)
	}
}

// TestKnownEntities_GrowsWithTrackedBinaries verifies a newly tracked binary
// becomes askable with zero classifier changes (§2.7: "derived, not
// enumerated"). Adding a binary to smith.binaries.tracked makes it a version
// entity the classifier matches.
func TestKnownEntities_GrowsWithTrackedBinaries(t *testing.T) {
	db := openDB(t)
	setSetting(t, db, SettingBinariesEnabled, `true`)
	setSetting(t, db, SettingBinariesTracked, `[{"name":"magic-llm","kind":"llama_build","path":"/opt/magic/llama-server","source_kind":"git","source_ref":"/opt/magic"}]`)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	ctx := context.Background()

	intent := s.Classify(ctx, "what version of magic-llm is running?")
	if intent.Family != FamilyVersion {
		t.Fatalf("family = %s, want version for a newly tracked binary", intent.Family)
	}
	if intent.Entity != "magic-llm" {
		t.Errorf("entity = %q, want magic-llm (newly tracked binaries auto-askable)", intent.Entity)
	}
}

// TestKnownEntities_GrowsWithMeshServices verifies a newly registered mesh
// service becomes reachability-askable with zero classifier changes
// (§2.7 "derived, not enumerated"; open-source-readiness finding 1 — the
// mesh map is smith.mesh.services settings, never compiled-in).
func TestKnownEntities_GrowsWithMeshServices(t *testing.T) {
	db := openDB(t)
	setSetting(t, db, SettingMeshServices, `[{"name":"forge-wiki","aliases":["forge-wiki","forge wiki"],"address":"forge-wiki.example.ts.net:8443"}]`)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	ctx := context.Background()

	intent := s.Classify(ctx, "is forge-wiki reachable on the tailnet?")
	if intent.Family != FamilyReachability {
		t.Fatalf("family = %s, want reachability for a newly registered mesh service", intent.Family)
	}
	if intent.Entity != "forge-wiki" {
		t.Errorf("entity = %q, want forge-wiki (newly registered mesh services auto-askable)", intent.Entity)
	}
}

// TestKnownEntities_MeshInventoryTrulyDrivesReachability verifies the mesh
// inventory is discovered, not memorized: with an empty smith.mesh.services
// the code-curated mesh entities are GONE from the reachability family
// (only the deployment-agnostic live probes remain). A compiled-in fallback
// list would fail this test.
func TestKnownEntities_MeshInventoryTrulyDrivesReachability(t *testing.T) {
	db := openDB(t)
	setSetting(t, db, SettingMeshServices, `[]`)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	ctx := context.Background()

	// "a0" is no longer a reachability entity, and "reachable" is not a
	// health cue — the conservative classifier falls through to no_match.
	if intent := s.Classify(ctx, "is a0 reachable?"); intent.Family != FamilyNoMatch {
		t.Errorf("empty inventory: Classify(\"is a0 reachable?\") = %s/%q, want no_match", intent.Family, intent.Entity)
	}
	// The live probes stay askable on every deployment.
	if intent := s.Classify(ctx, "is the tailnet up?"); intent.Family != FamilyReachability || intent.Entity != "tailnet" {
		t.Errorf("empty inventory: tailnet probe = %s/%q, want reachability/tailnet", intent.Family, intent.Entity)
	}
}

// TestClassify_HealthEntities verifies the health family instantiates over
// every known service/slot/check entity.
func TestClassify_HealthEntities(t *testing.T) {
	s := classifySmith(t)
	ctx := context.Background()
	cases := []struct {
		question string
		entity   string
	}{
		{"is comfyui healthy?", "comfyui"},
		{"is comfyui up?", "comfyui"},
		{"is compressor responding?", "compressor"},
		{"is a0 up?", "a0"},
		{"is a1 up?", "a1"},
		{"is a3 up?", "a3"},
		{"is embedding up?", "embedding"},
		{"is the gpu ok?", "gpu"},
		{"is forge healthy?", "forge"},
		{"is search working?", "search"},
	}
	for _, tc := range cases {
		intent := s.Classify(ctx, tc.question)
		if intent.Family != FamilyHealth || intent.Entity != tc.entity {
			t.Errorf("Classify(%q) = %s/%q, want health/%s", tc.question, intent.Family, intent.Entity, tc.entity)
		}
	}
}

// TestClassify_ReachabilityDisambiguation verifies shared aliases (comfyui,
// a1) route to health on a bare "up" and to reachability on a strong cue.
// fixtureSmith: the mesh aliases ("comfyui" → comfy-alpha, "a1") come from
// the imported example seed, not compiled-in defaults.
func TestClassify_ReachabilityDisambiguation(t *testing.T) {
	s := fixtureSmith(t)
	ctx := context.Background()
	// "comfyui up" → health (shared alias, weak cue).
	intent := s.Classify(ctx, "is comfyui up?")
	if intent.Family != FamilyHealth {
		t.Errorf("comfyui up: family = %s, want health (shared alias weak cue → health)", intent.Family)
	}
	// "comfyui available via tailnet" → reachability (strong cue).
	intent = s.Classify(ctx, "is comfyui available via tailnet?")
	if intent.Family != FamilyReachability || intent.Entity != "comfy-alpha" {
		t.Errorf("comfyui available via tailnet: = %s/%q, want reachability/comfy-alpha", intent.Family, intent.Entity)
	}
	// "comfy-alpha up" → reachability (reachability-only alias, weak cue OK).
	intent = s.Classify(ctx, "is comfy-alpha up?")
	if intent.Family != FamilyReachability || intent.Entity != "comfy-alpha" {
		t.Errorf("comfy-alpha up: = %s/%q, want reachability/comfy-alpha", intent.Family, intent.Entity)
	}
	// "a1 up" → health (shared alias).
	intent = s.Classify(ctx, "is a1 up?")
	if intent.Family != FamilyHealth {
		t.Errorf("a1 up: family = %s, want health", intent.Family)
	}
	// "a1 reachable via tailnet" → reachability (strong cue).
	intent = s.Classify(ctx, "is a1 reachable via tailnet?")
	if intent.Family != FamilyReachability || intent.Entity != "a1" {
		t.Errorf("a1 reachable: = %s/%q, want reachability/a1", intent.Family, intent.Entity)
	}
}

// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestGenealogiesMigrationFixesLiveShapedData replays 0017_genealogies.sql's
// data-fixing logic against data shaped exactly like the real ForgeHost catalog
// (queried live 2026-07-29 during the product/QA sprint investigation),
// proving the migration does the right thing on real data — not just that
// it no-ops safely on an empty one (TestMigrateFresh/TestCatalogFullRoundTrip
// already cover that).
//
// Migrations apply once at Open(), before any test-inserted data exists, so
// the normal Open() flow can't exercise "the migration runs against
// pre-existing seeded data" — that only happens for real when the live
// ForgeHost binary starts up against its persistent DB file. This test
// simulates that: seed realistic pre-migration data by hand, then
// re-execute the migration file's DML portion (the DDL header —
// CREATE TABLE / ALTER TABLE — already ran once at Open() and cannot be
// re-run; splitting it out here mirrors how the real migration runner
// executes the whole file as one Exec, per db.go's migrate()).
func TestGenealogiesMigrationFixesLiveShapedData(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	// ── Seed pre-migration shape (mirrors the live ForgeHost query results) ──
	seedFamily := func(name string) int64 {
		id, err := cat.CreateFamily(ctx, Family{Name: name})
		if err != nil {
			t.Fatalf("CreateFamily %s: %v", name, err)
		}
		return id
	}
	gemma := seedFamily("Gemma")
	nvidia := seedFamily("Nvidia")
	poolside := seedFamily("Poolside")
	genomics := seedFamily("Genomics")
	translation := seedFamily("Translation")
	qwen := seedFamily("Qwen")
	swallow := seedFamily("Swallow")

	seedModel := func(name, creator, hfRepo string, familyID int64) int64 {
		id, err := cat.CreateModel(ctx, Model{
			Name: name, Creator: creator, HFRepo: hfRepo, FamilyID: familyID,
		})
		if err != nil {
			t.Fatalf("CreateModel %s: %v", name, err)
		}
		return id
	}
	seedModel("Gemma 4 26B A4B (MTP)", "Google", "TrevorJS/gemma-4-26B-A4B-it-uncensored-GGUF", gemma)
	seedModel("Gemma 4 31B (MTP)", "Google", "TrevorJS/gemma-4-31B-it-uncensored-GGUF", gemma)
	seedModel("Gemma 4 E2B", "Google", "HauhauCS/Gemma-4-E2B-Uncensored-HauhauCS-Aggressive", gemma)
	seedModel("Nemotron 3 Nano Omni", "NVIDIA", "unsloth/NVIDIA-Nemotron-3-Nano-Omni-30B-A3B-Reasoning-GGUF", nvidia)
	seedModel("Nemotron 3 Super 120B", "NVIDIA", "unsloth/NVIDIA-Nemotron-3-Super-120B-A12B-GGUF", nvidia)
	seedModel("Nemotron Puzzle 75B", "NVIDIA", "RemySkye/NVIDIA-Nemotron-Labs-3-Puzzle-75B-A9B-GGUF", nvidia)
	seedModel("Laguna-S-2.1", "Poolside", "poolside/Laguna-S-2.1", poolside)
	seedModel("Carbon 8B", "", "", genomics)
	seedModel("Hy-MT2 30B", "", "", translation) // creator "" — the Tencent fix target
	seedModel("Qwen2.5 Coder 7B", "Alibaba", "Qwen/Qwen2.5-Coder-7B-Instruct-GGUF", qwen)
	qwen3CoderNext := seedModel("Qwen3 Coder Next", "Alibaba", "unsloth/Qwen3-Coder-Next-GGUF", qwen)
	seedModel("Qwen3.6 35B (Aggressive)", "Alibaba", "HauhauCS/Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive", qwen)
	seedModel("Qwen3.6 35B MTP", "Alibaba", "unsloth/Qwen3.6-35B-A3B-MTP-GGUF", qwen)
	seedModel("Swallow LLM", "Swallow Project", "", swallow)                                                // the stock one (no hf_repo)
	seedModel("Swallow LLM", "Swallow Project", "rinrin0413/Qwen3-Swallow-8B-RL-v0.2-Q4_K_M-GGUF", swallow) // the Qwen3-derived one
	seedModel("GPT-OSS 120B", "OpenAI", "HauhauCS/GPTOSS-120B-Uncensored-HauhauCS-Aggressive", 0)
	seedModel("Ornith 1.0 35B", "DeepReinforce", "unsloth/Ornith-1.0-35B-GGUF", 0)
	seedModel("deepseek-v4-pro", "", "", 0)
	seedModel("deepseek-v4-flash", "", "", 0)
	seedModel("glm-5.2", "", "", 0)
	gptOSSDup := seedModel("gpt-oss-120b", "OpenAI", "", 0) // the lowercase stub duplicate
	kimi1 := seedModel("kimi-k2.7-code", "", "", 0)         // orphan duplicate, no offering
	kimi2 := seedModel("kimi-k2.7-code", "", "", 0)         // the one with a live offering below
	_ = qwen3CoderNext

	if err := db.Routing().SaveProvider(ctx, ProviderRow{Name: "aiand", APIKey: "sk-x", CreatedAt: ts(1)}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	aiandID := testProviderID(t, db, "aiand")
	if _, err := cat.CreateOffering(ctx, Offering{ModelID: gptOSSDup, ProviderID: aiandID, WireModel: "openai/gpt-oss-120b", Enabled: true}); err != nil {
		t.Fatalf("seed gpt-oss offering: %v", err)
	}
	if _, err := cat.CreateOffering(ctx, Offering{ModelID: kimi2, ProviderID: aiandID, WireModel: "moonshotai/kimi-k2.7-code", Enabled: true}); err != nil {
		t.Fatalf("seed kimi offering: %v", err)
	}
	_ = kimi1

	// ── Replay the migration's DML against this now-seeded data ──
	body, err := os.ReadFile("migrations/0017_genealogies.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	const splitMarker = "-- ── Genealogies"
	i := strings.Index(string(body), splitMarker)
	if i < 0 {
		t.Fatalf("split marker %q not found in migration file — did it get renamed?", splitMarker)
	}
	dml := string(body)[i:]
	if _, err := db.SQL().ExecContext(ctx, dml); err != nil {
		t.Fatalf("replay migration DML: %v", err)
	}

	// ── Assert the fixed shape ──
	families, err := cat.ListFamilies(ctx)
	if err != nil {
		t.Fatalf("ListFamilies: %v", err)
	}
	byName := map[string]Family{}
	for _, f := range families {
		byName[f.Name] = f
	}
	wantFamilies := []string{
		"Gemma 4", "Nemotron 3", "Laguna S 2", "Carbon", "Hy-MT2",
		"Qwen 2.5", "Qwen 3", "Qwen 3.6", "Swallow 32B", "Qwen3 Swallow 8B",
		"GPT-OSS", "Ornith 1.0", "DeepSeek V4", "GLM 5.2",
	}
	for _, name := range wantFamilies {
		if _, ok := byName[name]; !ok {
			t.Errorf("expected family %q to exist after migration; got %v", name, familyNames(families))
		}
	}
	for _, stale := range []string{"Gemma", "Nvidia", "Poolside", "Genomics", "Translation", "Qwen", "Swallow"} {
		if _, ok := byName[stale]; ok {
			t.Errorf("stale family name %q should have been renamed away", stale)
		}
	}

	genealogies, err := cat.ListGenealogies(ctx)
	if err != nil {
		t.Fatalf("ListGenealogies: %v", err)
	}
	wantGenealogies := []string{"Gemma", "Nemotron", "Laguna", "Carbon", "Hunyuan", "Qwen", "Swallow", "GPT-OSS", "Ornith", "DeepSeek", "GLM"}
	genByName := map[string]Genealogy{}
	for _, g := range genealogies {
		genByName[g.Name] = g
	}
	for _, name := range wantGenealogies {
		if _, ok := genByName[name]; !ok {
			t.Errorf("expected genealogy %q to exist", name)
		}
	}

	// Family → genealogy links.
	if byName["Qwen 2.5"].GenealogyID != genByName["Qwen"].ID ||
		byName["Qwen 3"].GenealogyID != genByName["Qwen"].ID ||
		byName["Qwen 3.6"].GenealogyID != genByName["Qwen"].ID {
		t.Error("all three Qwen generations should share the Qwen genealogy")
	}
	if byName["Hy-MT2"].GenealogyID != genByName["Hunyuan"].ID {
		t.Error("Hy-MT2 family should be under the Hunyuan genealogy")
	}

	// Model reassignment + creator fix.
	models, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	modelByName := map[string]Model{}
	var swallowLLMCount int
	for _, m := range models {
		if m.Name == "Swallow LLM" {
			swallowLLMCount++
		}
		modelByName[m.Name] = m
	}
	if swallowLLMCount != 0 {
		t.Errorf("expected both 'Swallow LLM' rows renamed away, found %d still named that", swallowLLMCount)
	}
	if got, ok := modelByName["Swallow 32B"]; !ok || got.FamilyID != byName["Swallow 32B"].ID {
		t.Errorf("Swallow 32B model missing or misassigned: %+v", got)
	}
	if got, ok := modelByName["Qwen3 Swallow 8B RL"]; !ok || got.FamilyID != byName["Qwen3 Swallow 8B"].ID {
		t.Errorf("Qwen3 Swallow 8B RL model missing or misassigned: %+v", got)
	}
	if got := modelByName["Qwen3 Coder Next"]; got.FamilyID != byName["Qwen 3"].ID {
		t.Errorf("Qwen3 Coder Next family = %d, want Qwen 3's id %d", got.FamilyID, byName["Qwen 3"].ID)
	}
	if got := modelByName["Hy-MT2 30B"]; got.Creator != "Tencent" {
		t.Errorf("Hy-MT2 30B creator = %q, want Tencent", got.Creator)
	}
	if got := modelByName["GPT-OSS 120B"]; got.FamilyID != byName["GPT-OSS"].ID {
		t.Errorf("GPT-OSS 120B family = %d, want GPT-OSS's id %d", got.FamilyID, byName["GPT-OSS"].ID)
	}
	if _, stillThere := modelByName["gpt-oss-120b"]; stillThere {
		t.Error("lowercase 'gpt-oss-120b' duplicate should have been deleted")
	}
	if _, stillThere := modelByName["kimi-k2.7-code"]; stillThere {
		t.Error("'kimi-k2.7-code' should have been deleted entirely")
	}

	// The gpt-oss-120b offering must have been re-pointed onto the
	// canonical model, not lost when the stub was deleted.
	offerings, err := cat.ListOfferingsForModel(ctx, modelByName["GPT-OSS 120B"].ID)
	if err != nil {
		t.Fatalf("ListOfferingsForModel: %v", err)
	}
	found := false
	for _, o := range offerings {
		if o.WireModel == "openai/gpt-oss-120b" {
			found = true
		}
	}
	if !found {
		t.Error("expected the aiand gpt-oss-120b offering to survive, re-pointed at the canonical model")
	}

	// The kimi offering must be gone entirely (cascaded with its model).
	allOfferings, err := cat.ListOfferings(ctx)
	if err != nil {
		t.Fatalf("ListOfferings: %v", err)
	}
	for _, o := range allOfferings {
		if o.WireModel == "moonshotai/kimi-k2.7-code" {
			t.Error("kimi offering should have been cascade-deleted with its model")
		}
	}
}

func familyNames(fs []Family) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// newCatalogTestServer builds a Server backed by a real in-memory store.DB
// with the Catalog dependency wired. The DB has all migrations applied
// (including 0008_model_catalog), so enum tables (quantizations, formats,
// engines) are already seeded.
func newCatalogTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Paths:  config.Paths{ModelsDir: ""},
	})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Catalog:   db.Catalog(),
		// Compressor (0042): handleCatalogOfferingCreate/Update now resolve an
		// offering body's provider NAME to the real provider_id FK via
		// resolveOfferingProviderID(s.deps.Routing.ProviderByName) — must be
		// wired to the same db seedCatalogPrereqs seeds into, matching
		// production (one *store.DB backs both).
		Routing: db.Routing(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

// seedCatalogPrereqs inserts the prerequisite entities (family, model,
// variant, artifact, engine, build, provider) that the CRUD tests build on.
// Returns the IDs the tests need.
type catalogPrereqs struct {
	familyID       int64
	modelID        int64
	variantID      int64
	artifactID     int64
	mmprojID       int64
	engineID       int64
	buildID        int64
	quantID        int64
	formatID       int64
	providerExists bool
}

func seedCatalogPrereqs(t *testing.T, db *store.DB) catalogPrereqs {
	t.Helper()
	ctx := t.Context()
	cat := db.Catalog()

	famID, err := cat.CreateFamily(ctx, store.Family{Name: "TestFamily"})
	if err != nil {
		t.Fatalf("CreateFamily: %v", err)
	}
	mdlID, err := cat.CreateModel(ctx, store.Model{
		FamilyID: famID, Name: "TestModel", Architecture: "llama",
	})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{
		ModelID: mdlID, Name: "base", TrainedCtx: 131072,
	})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	q, err := cat.QuantizationByName(ctx, "Q8_0")
	if err != nil {
		t.Fatalf("QuantizationByName: %v", err)
	}
	f, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: f.ID,
		FilePath: "/models/test.gguf", ArtifactType: "weight",
	})
	if err != nil {
		t.Fatalf("CreateArtifact weight: %v", err)
	}
	mmID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, QuantizationID: q.ID, FormatID: f.ID,
		FilePath: "/models/mmproj.gguf", ArtifactType: "mmproj", IsAuxiliary: true,
	})
	if err != nil {
		t.Fatalf("CreateArtifact mmproj: %v", err)
	}
	engines, err := cat.ListEngines(ctx)
	if err != nil {
		t.Fatalf("ListEngines: %v", err)
	}
	engID := engines[0].ID
	bID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: engID, Name: "vulkan-build", BinaryPath: "/usr/bin/llama-server",
		Backend: "vulkan",
	})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	// Seed a provider for offering tests.
	_, err = db.SQL().ExecContext(ctx,
		`INSERT INTO router_providers (name, api_key, created_at) VALUES ('testprov', 'key', 0)`)
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return catalogPrereqs{
		familyID:       famID,
		modelID:        mdlID,
		variantID:      varID,
		artifactID:     artID,
		mmprojID:       mmID,
		engineID:       engID,
		buildID:        bID,
		quantID:        q.ID,
		formatID:       f.ID,
		providerExists: true,
	}
}

// ── Genealogy + Family CRUD (product/QA sprint, 2026-07-29) ─────────────────
// Families were list-only before this sprint; genealogy (the level above
// family) is new. Mirrors the Model CRUD test pattern above.

func TestCatalogGenealogyCRUD(t *testing.T) {
	s, _ := newCatalogTestServer(t)

	// Create.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/genealogies", bytes.NewBufferString(`{"name":"Nemotron"}`)))
	if w.Code != 201 {
		t.Fatalf("create genealogy = %d, want 201: %s", w.Code, w.Body.String())
	}
	var g genealogyJSON
	decodeJSON(t, w.Body, &g)
	if g.Name != "Nemotron" {
		t.Errorf("created genealogy = %+v", g)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/genealogies", nil))
	if w.Code != 200 {
		t.Fatalf("list genealogies = %d", w.Code)
	}
	var list []genealogyJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 genealogy, got %d", len(list))
	}

	// Get by ID.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/genealogies/"+itoa(g.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get genealogy = %d", w.Code)
	}

	// Update.
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/genealogies/"+itoa(g.ID), bytes.NewBufferString(`{"name":"Nemotron Line"}`)))
	if w.Code != 200 {
		t.Fatalf("update genealogy = %d: %s", w.Code, w.Body.String())
	}
	var g2 genealogyJSON
	decodeJSON(t, w.Body, &g2)
	if g2.Name != "Nemotron Line" {
		t.Errorf("updated name = %q", g2.Name)
	}

	// A family can reference it.
	fw := do(t, s, authedRequest("POST", "/api/v1/catalog/families",
		bytes.NewBufferString(`{"name":"Nemotron 3","genealogy_id":`+itoa(g.ID)+`}`)))
	if fw.Code != 201 {
		t.Fatalf("create family with genealogy = %d: %s", fw.Code, fw.Body.String())
	}
	var f familyJSON
	decodeJSON(t, fw.Body, &f)
	if f.GenealogyID != g.ID {
		t.Errorf("family.genealogy_id = %d, want %d", f.GenealogyID, g.ID)
	}

	// Delete the genealogy — the family survives with genealogy_id cleared
	// (ON DELETE SET NULL), not refused and not cascade-deleted.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/genealogies/"+itoa(g.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete genealogy = %d: %s", w.Code, w.Body.String())
	}
	fw = do(t, s, authedRequest("GET", "/api/v1/catalog/families/"+itoa(f.ID), nil))
	if fw.Code != 200 {
		t.Fatalf("get family after genealogy delete = %d, want 200 (family must survive)", fw.Code)
	}
	var f2 familyJSON
	decodeJSON(t, fw.Body, &f2)
	if f2.GenealogyID != 0 {
		t.Errorf("family.genealogy_id after genealogy delete = %d, want 0", f2.GenealogyID)
	}

	// Get after delete → 404.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/genealogies/"+itoa(g.ID), nil))
	if w.Code != 404 {
		t.Errorf("get deleted genealogy = %d, want 404", w.Code)
	}
}

func TestCatalogFamilyCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db) // creates a "TestModel" under a family we'll delete below

	// Create.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/families", bytes.NewBufferString(`{"name":"Gemma 4"}`)))
	if w.Code != 201 {
		t.Fatalf("create family = %d, want 201: %s", w.Code, w.Body.String())
	}
	var f familyJSON
	decodeJSON(t, w.Body, &f)
	if f.Name != "Gemma 4" {
		t.Errorf("created family = %+v", f)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/families", nil))
	if w.Code != 200 {
		t.Fatalf("list families = %d", w.Code)
	}
	var list []familyJSON
	decodeJSON(t, w.Body, &list)
	if len(list) < 2 { // pq.familyID (TestFamily) + the new one
		t.Errorf("expected ≥2 families, got %d", len(list))
	}

	// Get by ID.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/families/"+itoa(f.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get family = %d", w.Code)
	}

	// Update.
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/families/"+itoa(f.ID), bytes.NewBufferString(`{"name":"Gemma 4 Updated"}`)))
	if w.Code != 200 {
		t.Fatalf("update family = %d: %s", w.Code, w.Body.String())
	}
	var f2 familyJSON
	decodeJSON(t, w.Body, &f2)
	if f2.Name != "Gemma 4 Updated" {
		t.Errorf("updated name = %q", f2.Name)
	}

	// Update with a non-existent genealogy_id must be rejected.
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/families/"+itoa(f.ID), bytes.NewBufferString(`{"name":"x","genealogy_id":99999}`)))
	if w.Code != 422 {
		t.Errorf("update with bad genealogy_id = %d, want 422: %s", w.Code, w.Body.String())
	}

	// Delete a family with a model referencing it — model survives with
	// family_id cleared (ON DELETE SET NULL), not refused.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/families/"+itoa(pq.familyID), nil))
	if w.Code != 200 {
		t.Fatalf("delete family-in-use = %d, want 200 (families don't refuse on dependents): %s", w.Code, w.Body.String())
	}
	mw := do(t, s, authedRequest("GET", "/api/v1/catalog/models/"+itoa(pq.modelID), nil))
	var m modelJSON
	decodeJSON(t, mw.Body, &m)
	if m.FamilyID != 0 {
		t.Errorf("model.family_id after its family was deleted = %d, want 0", m.FamilyID)
	}

	// Get after delete → 404.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/families/"+itoa(pq.familyID), nil))
	if w.Code != 404 {
		t.Errorf("get deleted family = %d, want 404", w.Code)
	}
}

// ── Model CRUD ───────────────────────────────────────────────────────────────

func TestCatalogModelCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Create.
	body := `{"name":"NewModel","family_id":` + itoa(pq.familyID) + `,"architecture":"gemma","creator":"TestCo"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create model = %d, want 201: %s", w.Code, w.Body.String())
	}
	var m modelJSON
	decodeJSON(t, w.Body, &m)
	if m.Name != "NewModel" || m.Architecture != "gemma" {
		t.Errorf("created model = %+v", m)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/models", nil))
	if w.Code != 200 {
		t.Fatalf("list models = %d", w.Code)
	}
	var list []modelJSON
	decodeJSON(t, w.Body, &list)
	if len(list) < 2 {
		t.Errorf("expected ≥2 models, got %d", len(list))
	}

	// Get by ID.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/models/"+itoa(m.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get model = %d", w.Code)
	}
	var m2 modelJSON
	decodeJSON(t, w.Body, &m2)
	if m2.Name != "NewModel" {
		t.Errorf("got model name %q", m2.Name)
	}

	// Update.
	body = `{"name":"UpdatedModel","family_id":` + itoa(pq.familyID) + `,"architecture":"llama","creator":"TestCo2"}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/models/"+itoa(m.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update model = %d: %s", w.Code, w.Body.String())
	}
	var m3 modelJSON
	decodeJSON(t, w.Body, &m3)
	if m3.Name != "UpdatedModel" {
		t.Errorf("updated name = %q", m3.Name)
	}

	// Validate (should pass).
	body = `{"name":"ValidModel","family_id":` + itoa(pq.familyID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models/validate", bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Errorf("validate model = %d: %s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/models/"+itoa(m.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete model = %d: %s", w.Code, w.Body.String())
	}

	// Get after delete → 404.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/models/"+itoa(m.ID), nil))
	if w.Code != 404 {
		t.Errorf("get deleted model = %d, want 404", w.Code)
	}
}

func TestCatalogModelValidation(t *testing.T) {
	s, _ := newCatalogTestServer(t)
	// Empty name → 422.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(`{"name":""}`)))
	if w.Code != 422 {
		t.Fatalf("empty name = %d, want 422", w.Code)
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if resp["error"] != "validation_failed" {
		t.Errorf("error = %v", resp["error"])
	}
}

// ── Variant CRUD ──────────────────────────────────────────────────────────────

func TestCatalogVariantCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	body := `{"model_id":` + itoa(pq.modelID) + `,"name":"abliterated","derivation_type":"abliteration","trained_ctx":131072,"is_abliterated":true}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/variants", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create variant = %d: %s", w.Code, w.Body.String())
	}
	var v variantJSON
	decodeJSON(t, w.Body, &v)
	if v.Name != "abliterated" || !v.IsAbliterated {
		t.Errorf("created variant = %+v", v)
	}

	// List filtered by model_id.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/variants?model_id="+itoa(pq.modelID), nil))
	if w.Code != 200 {
		t.Fatalf("list variants = %d", w.Code)
	}
	var list []variantJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 2 {
		t.Errorf("expected 2 variants, got %d", len(list))
	}

	// Update.
	body = `{"model_id":` + itoa(pq.modelID) + `,"name":"renamed","derivation_type":"finetune","trained_ctx":262144}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/variants/"+itoa(v.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update variant = %d: %s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/variants/"+itoa(v.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete variant = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogVariantValidation(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Missing model_id.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/variants", bytes.NewBufferString(`{"name":"x"}`)))
	if w.Code != 422 {
		t.Fatalf("missing model_id = %d, want 422", w.Code)
	}
	// Nonexistent model_id.
	body := `{"model_id":99999,"name":"x"}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/variants", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("nonexistent model = %d, want 422", w.Code)
	}
	// Invalid derivation_type.
	body = `{"model_id":` + itoa(pq.modelID) + `,"name":"x","derivation_type":"bogus"}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/variants", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("bad derivation_type = %d, want 422", w.Code)
	}
}

// ── Config CRUD (the DoD test) ───────────────────────────────────────────────

func TestCatalogConfigCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Create a Config — this is the DoD: "CRUD round-trips a Config from
	// creation to load on ForgeHost."
	body := `{"name":"test-config","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) +
		`,"mmproj_artifact_id":` + itoa(pq.mmprojID) +
		`,"n_ctx":131072,"parallel":1,"extra_args":["--swa-full"]}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create config = %d: %s", w.Code, w.Body.String())
	}
	var c configJSON
	decodeJSON(t, w.Body, &c)
	if c.Name != "test-config" || c.NCtx != 131072 {
		t.Errorf("created config = %+v", c)
	}
	if len(c.ExtraArgs) != 1 || c.ExtraArgs[0] != "--swa-full" {
		t.Errorf("extra_args = %v", c.ExtraArgs)
	}

	// Get by ID.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get config = %d", w.Code)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/configs", nil))
	if w.Code != 200 {
		t.Fatalf("list configs = %d", w.Code)
	}
	var list []configJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 config, got %d", len(list))
	}

	// List filtered by variant_id.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/configs?variant_id="+itoa(pq.variantID), nil))
	if w.Code != 200 {
		t.Fatalf("list configs for variant = %d", w.Code)
	}
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 config for variant, got %d", len(list))
	}

	// Update.
	body = `{"name":"test-config-v2","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) +
		`,"n_ctx":65536,"parallel":2,"visibility":"hidden"}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update config = %d: %s", w.Code, w.Body.String())
	}
	var c2 configJSON
	decodeJSON(t, w.Body, &c2)
	if c2.Name != "test-config-v2" || c2.NCtx != 65536 || c2.Visibility != "hidden" {
		t.Errorf("updated config = %+v", c2)
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete config = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogConfigValidation(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Invalid name (doesn't match pattern).
	body := `{"name":"UPPERCASE","variant_id":` + itoa(pq.variantID) + `,"weight_artifact_id":` + itoa(pq.artifactID) + `,"engine_id":` + itoa(pq.engineID) + `}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("invalid name = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	fields := resp["fields"].(map[string]any)
	if _, ok := fields["name"]; !ok {
		t.Errorf("expected 'name' field error, got %v", fields)
	}

	// Nonexistent variant.
	body = `{"name":"bad-var","variant_id":99999,"weight_artifact_id":` + itoa(pq.artifactID) + `,"engine_id":` + itoa(pq.engineID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("nonexistent variant = %d, want 422", w.Code)
	}
	decodeJSON(t, w.Body, &resp)
	fields = resp["fields"].(map[string]any)
	if _, ok := fields["variant_id"]; !ok {
		t.Errorf("expected 'variant_id' field error, got %v", fields)
	}

	// Weight artifact of wrong type (mmproj instead of weight).
	body = `{"name":"bad-art","variant_id":` + itoa(pq.variantID) + `,"weight_artifact_id":` + itoa(pq.mmprojID) + `,"engine_id":` + itoa(pq.engineID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("wrong artifact type = %d, want 422", w.Code)
	}
	decodeJSON(t, w.Body, &resp)
	fields = resp["fields"].(map[string]any)
	if _, ok := fields["weight_artifact_id"]; !ok {
		t.Errorf("expected 'weight_artifact_id' field error, got %v", fields)
	}

	// Duplicate name — create one, then try to create another with the same name.
	body = `{"name":"dup","variant_id":` + itoa(pq.variantID) + `,"weight_artifact_id":` + itoa(pq.artifactID) + `,"engine_id":` + itoa(pq.engineID) + `,"build_id":` + itoa(pq.buildID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("first create = %d: %s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("duplicate name = %d, want 422", w.Code)
	}
	decodeJSON(t, w.Body, &resp)
	fields = resp["fields"].(map[string]any)
	if _, ok := fields["name"]; !ok {
		t.Errorf("expected 'name' field error, got %v", fields)
	}

	// Missing build_id — required, not optional (a Config with no Build has
	// no defined backend, which used to silently default to vulkan).
	body = `{"name":"no-build","variant_id":` + itoa(pq.variantID) + `,"weight_artifact_id":` + itoa(pq.artifactID) + `,"engine_id":` + itoa(pq.engineID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("missing build_id = %d, want 422: %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &resp)
	fields = resp["fields"].(map[string]any)
	if _, ok := fields["build_id"]; !ok {
		t.Errorf("expected 'build_id' field error, got %v", fields)
	}
}

// TestCatalogModalitiesRoundTrip is the Sprint J1 dead-column regression
// guard (the PriceCachedInPer1M trap, 2026-07-31: a field written by a form
// but missing from one read path is invisible; a column missing from an
// Update handler is silently wiped on every save). Model.Modalities must
// survive create->update->get; Config.Modalities must survive the nil ->
// explicit-override -> nil round trip (configJSON's `omitzero` tag means a
// derive-mode config must not even carry the key on the wire).
func TestCatalogModalitiesRoundTrip(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Model: create with modalities, confirm on create + get, then update.
	body := `{"name":"OmniModel","modalities":["text","vision","audio"]}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create model = %d: %s", w.Code, w.Body.String())
	}
	var m modelJSON
	decodeJSON(t, w.Body, &m)
	if len(m.Modalities) != 3 {
		t.Fatalf("created model modalities = %+v, want 3 entries", m.Modalities)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/catalog/models/"+itoa(m.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get model = %d", w.Code)
	}
	var m2 modelJSON
	decodeJSON(t, w.Body, &m2)
	if len(m2.Modalities) != 3 || m2.Modalities[2] != "audio" {
		t.Fatalf("get model modalities = %+v, want [text vision audio]", m2.Modalities)
	}

	body = `{"name":"OmniModel","modalities":["text","vision"]}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/models/"+itoa(m.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update model = %d: %s", w.Code, w.Body.String())
	}
	var m3 modelJSON
	decodeJSON(t, w.Body, &m3)
	if len(m3.Modalities) != 2 {
		t.Fatalf("updated model modalities = %+v, want [text vision]", m3.Modalities)
	}

	// Reject an unknown modality.
	body = `{"name":"BadModel","modalities":["text","smell"]}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("unknown modality = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	fields := resp["fields"].(map[string]any)
	if _, ok := fields["modalities"]; !ok {
		t.Errorf("expected 'modalities' field error, got %v", fields)
	}

	// Config: create with no modalities key at all -> derive mode, the field
	// must be entirely absent from the response body (omitzero), not null.
	body = `{"name":"omni-cfg","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create config = %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"modalities"`)) {
		t.Errorf("derive-mode config body should omit modalities entirely, got: %s", w.Body.String())
	}
	var c configJSON
	decodeJSON(t, w.Body, &c)
	if c.Modalities != nil {
		t.Fatalf("derive-mode config Modalities = %+v, want nil", c.Modalities)
	}

	// Update with an explicit override.
	body = `{"name":"omni-cfg","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) +
		`,"modalities":["text","vision"]}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update config = %d: %s", w.Code, w.Body.String())
	}
	var c2 configJSON
	decodeJSON(t, w.Body, &c2)
	if c2.Modalities == nil || len(*c2.Modalities) != 2 {
		t.Fatalf("overridden config Modalities = %+v, want [text vision]", c2.Modalities)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get config = %d", w.Code)
	}
	var c3 configJSON
	decodeJSON(t, w.Body, &c3)
	if c3.Modalities == nil || len(*c3.Modalities) != 2 {
		t.Fatalf("get config after override: Modalities = %+v, want [text vision] (dead-column regression)", c3.Modalities)
	}

	// Reject an unknown modality on a config too.
	body = `{"name":"omni-cfg","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) +
		`,"modalities":["telepathy"]}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("unknown modality on config = %d, want 422: %s", w.Code, w.Body.String())
	}
}

func TestCatalogConfigDeleteLoaded(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	ctx := t.Context()

	// Create a config.
	id, err := db.Catalog().CreateConfig(ctx, store.Config{
		Name: "loaded-config", VariantID: pq.variantID,
		WeightArtifactID: pq.artifactID, EngineID: pq.engineID,
		NCtx: 131072, Parallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	// Simulate it being loaded by setting the collector snapshot.
	snap := s.deps.Snapshots.Current()
	snap.Slots["a1"] = collector.SlotState{Mode: "loaded-config"}

	// Delete without force → 409.
	w := do(t, s, authedRequest("DELETE", "/api/v1/catalog/configs/"+itoa(id), nil))
	if w.Code != 409 {
		t.Fatalf("delete loaded config without force = %d, want 409", w.Code)
	}

	// Delete with force → 200.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/configs/"+itoa(id)+"?force=true", nil))
	if w.Code != 200 {
		t.Fatalf("delete loaded config with force = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// ── Offering CRUD ───────────────────────────────────────────────────────────

func TestCatalogOfferingCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	body := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"test-model","price_in_per_1m":0.5,"price_out_per_1m":1.5,"currency":"USD","context_length":131072,"enabled":true}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create offering = %d: %s", w.Code, w.Body.String())
	}
	var o offeringJSON
	decodeJSON(t, w.Body, &o)
	if o.WireModel != "test-model" || !o.Enabled {
		t.Errorf("created offering = %+v", o)
	}
	if o.PriceCachedInPer1M != nil {
		t.Errorf("price_cached_in_per_1m = %v, want nil (not supplied on create)", *o.PriceCachedInPer1M)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/offerings", nil))
	if w.Code != 200 {
		t.Fatalf("list offerings = %d", w.Code)
	}
	var list []offeringJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 offering, got %d", len(list))
	}

	// Update — also sets price_cached_in_per_1m, which must round-trip
	// through Get/Update (a real pre-existing gap: only ListOfferings used
	// to read this column back).
	body = `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"test-model-v2","price_in_per_1m":0.6,"price_out_per_1m":1.6,"price_cached_in_per_1m":0.06,"currency":"EUR","context_length":65536,"enabled":false}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/offerings/"+itoa(o.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update offering = %d: %s", w.Code, w.Body.String())
	}
	var updated offeringJSON
	decodeJSON(t, w.Body, &updated)
	if updated.PriceCachedInPer1M == nil || *updated.PriceCachedInPer1M != 0.06 {
		t.Errorf("price_cached_in_per_1m after update = %v, want 0.06", updated.PriceCachedInPer1M)
	}

	// Get — must reflect the same value, not just the update response.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/offerings/"+itoa(o.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get offering = %d: %s", w.Code, w.Body.String())
	}
	var fetched offeringJSON
	decodeJSON(t, w.Body, &fetched)
	if fetched.PriceCachedInPer1M == nil || *fetched.PriceCachedInPer1M != 0.06 {
		t.Errorf("price_cached_in_per_1m on GET = %v, want 0.06", fetched.PriceCachedInPer1M)
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/offerings/"+itoa(o.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete offering = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogOfferingValidation(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Missing wire_model.
	body := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("missing wire_model = %d, want 422", w.Code)
	}

	// Nonexistent provider.
	body = `{"model_id":` + itoa(pq.modelID) + `,"provider":"nonexistent","wire_model":"test"}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("nonexistent provider = %d, want 422", w.Code)
	}

	// Nonexistent model.
	body = `{"model_id":99999,"provider":"testprov","wire_model":"test"}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("nonexistent model = %d, want 422", w.Code)
	}
}

// TestCatalogOfferingPriority covers the multi-provider preference field
// (0032): omitted on create → the column default 100 ("no preference"),
// explicit on create → round-trips, omitted on update → preserved. The
// zero-value trap: a bare int would make every omitted-priority create the
// TOP priority (lowest value wins).
func TestCatalogOfferingPriority(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Omitted → 100.
	body := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"m1","enabled":true}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var o offeringJSON
	decodeJSON(t, w.Body, &o)
	if o.Priority != 100 {
		t.Errorf("default priority = %d, want 100", o.Priority)
	}

	// Explicit → round-trips.
	body = `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"m2","enabled":true,"priority":10}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var o2 offeringJSON
	decodeJSON(t, w.Body, &o2)
	if o2.Priority != 10 {
		t.Errorf("explicit priority = %d, want 10", o2.Priority)
	}

	// Update omitting priority → preserved, not reset.
	body = `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"m2","enabled":true}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/offerings/"+itoa(o2.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update = %d: %s", w.Code, w.Body.String())
	}
	var updated offeringJSON
	decodeJSON(t, w.Body, &updated)
	if updated.Priority != 10 {
		t.Errorf("priority after omit-on-update = %d, want 10 (preserved)", updated.Priority)
	}

	// Negative rejected.
	body = `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"m3","enabled":true,"priority":-1}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("negative priority = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// TestCatalogOfferingDuplicateProviderWireModel proves the (provider,
// wire_model) uniqueness gate: two rows the router can't distinguish were
// freely creatable before 2026-08-06 (this is how the duplicate Qwen rows
// got in). Same provider + wire on create → 422; an update may keep its own
// values but must not collide with a DIFFERENT offering.
func TestCatalogOfferingDuplicateProviderWireModel(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	body := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"dup","enabled":true}`
	if w := do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body))); w.Code != 201 {
		t.Fatalf("first create = %d: %s", w.Code, w.Body.String())
	}
	// Exact duplicate → 422.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("duplicate create = %d, want 422: %s", w.Code, w.Body.String())
	}

	// A second offering with a distinct wire name...
	body2 := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"other","enabled":true}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/offerings", bytes.NewBufferString(body2)))
	if w.Code != 201 {
		t.Fatalf("second create = %d: %s", w.Code, w.Body.String())
	}
	var other offeringJSON
	decodeJSON(t, w.Body, &other)

	// ...renamed onto the first one's wire name → 422.
	body3 := `{"model_id":` + itoa(pq.modelID) + `,"provider":"testprov","wire_model":"dup","enabled":true}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/offerings/"+itoa(other.ID), bytes.NewBufferString(body3)))
	if w.Code != 422 {
		t.Fatalf("update into duplicate = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// ── Benchmark CRUD + F7 gate ─────────────────────────────────────────────────

func TestCatalogBenchmarkCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Create a self_measured benchmark (no source_url needed).
	body := `{"metric":"decode_tps","value":"35.2","source":"self_measured","subject_type":"config","subject_id":` + itoa(pq.variantID) + `,"notes":"measured on ForgeHost"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create benchmark = %d: %s", w.Code, w.Body.String())
	}
	var b benchmarkJSON
	decodeJSON(t, w.Body, &b)
	if b.Metric != "decode_tps" || b.Value != "35.2" {
		t.Errorf("created benchmark = %+v", b)
	}

	// Update.
	body = `{"metric":"decode_tps","value":"38.1","source":"self_measured","subject_type":"config","subject_id":` + itoa(pq.variantID) + `}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/benchmarks/"+itoa(b.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update benchmark = %d: %s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/benchmarks/"+itoa(b.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete benchmark = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogBenchmarkF7Gate(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// Published without source_url → 422 (F7 gate).
	body := `{"metric":"GPQA Diamond","value":"0.843","source":"published","subject_type":"model","subject_id":` + itoa(pq.modelID) + `}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("published without source_url = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	fields := resp["fields"].(map[string]any)
	if _, ok := fields["source_url"]; !ok {
		t.Errorf("expected 'source_url' field error, got %v", fields)
	}

	// Published without source_date → 422.
	body = `{"metric":"GPQA Diamond","value":"0.843","source":"published","source_url":"https://example.com/bench","subject_type":"model","subject_id":` + itoa(pq.modelID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("published without source_date = %d, want 422", w.Code)
	}
	decodeJSON(t, w.Body, &resp)
	fields = resp["fields"].(map[string]any)
	if _, ok := fields["source_date"]; !ok {
		t.Errorf("expected 'source_date' field error, got %v", fields)
	}

	// Published with both source_url + source_date → 201.
	body = `{"metric":"GPQA Diamond","value":"0.843","source":"published","source_url":"https://example.com/bench","source_date":"2026-01-15","subject_type":"model","subject_id":` + itoa(pq.modelID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("published with url+date = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestCatalogBenchmarkValidation(t *testing.T) {
	s, _ := newCatalogTestServer(t)
	// Invalid source.
	body := `{"metric":"x","value":"1","source":"bogus","subject_type":"model","subject_id":1}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("invalid source = %d, want 422", w.Code)
	}
	// Invalid subject_type.
	body = `{"metric":"x","value":"1","source":"self_measured","subject_type":"bogus","subject_id":1}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("invalid subject_type = %d, want 422", w.Code)
	}
	// Bad source_date format.
	body = `{"metric":"x","value":"1","source":"published","source_url":"https://x.com","source_date":"01/15/2026","subject_type":"model","subject_id":1}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("bad source_date = %d, want 422", w.Code)
	}
}

// TestCatalogBenchmarkOfferingSubjectNarrowed is Phase 8 (pre-release
// feedback sprint): subject_type="offering" is rejected on CREATE — no card
// of any kind reads offering-scoped benchmarks (registry.go's loadSnapshot
// never indexed them) — while "config" (the whole point of this phase's
// registry union) keeps working.
func TestCatalogBenchmarkOfferingSubjectNarrowed(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	body := `{"metric":"x","value":"1","source":"self_measured","subject_type":"offering","subject_id":1}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("create offering-scoped = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	fields := resp["fields"].(map[string]any)
	if _, ok := fields["subject_type"]; !ok {
		t.Errorf("expected 'subject_type' field error, got %v", fields)
	}

	body = `{"metric":"safe_memory_bytes","value":"1000","source":"self_measured","subject_type":"config","subject_id":` + itoa(pq.variantID) + `}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/benchmarks", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create config-scoped = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// TestCatalogBenchmarkOfferingGrandfather covers the hazard the narrowing
// above creates: a pre-existing offering-scoped row (from before this
// phase) must stay editable through PUT, or it becomes permanently
// unsavable via the only UI that can reach it. Seeds the row directly at
// the store layer (CreateBenchmark has no FK/CHECK on subject_type — the
// HTTP validator is the only gate), matching how such a row would already
// exist in a live DB predating this migration.
func TestCatalogBenchmarkOfferingGrandfather(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	cat := db.Catalog()
	ctx := t.Context()

	offeringID, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "price_in_per_1m", Value: "0.5", Source: "provider_reported",
		SubjectType: "offering", SubjectID: 42,
	})
	if err != nil {
		t.Fatalf("seed offering-scoped benchmark: %v", err)
	}

	// PUT keeping subject_type="offering" + the same subject_id -> 200
	// (grandfathered).
	body := `{"metric":"price_in_per_1m","value":"0.55","source":"provider_reported","subject_type":"offering","subject_id":42}`
	w := do(t, s, authedRequest("PUT", "/api/v1/catalog/benchmarks/"+itoa(offeringID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("grandfathered PUT = %d, want 200: %s", w.Code, w.Body.String())
	}

	// PUT re-scoping the same row to "config" -> 200 (the strict path, but
	// re-scoping AWAY from offering is never what's being restricted).
	body = `{"metric":"price_in_per_1m","value":"0.55","source":"provider_reported","subject_type":"config","subject_id":` + itoa(pq.variantID) + `}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/benchmarks/"+itoa(offeringID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("re-scope to config PUT = %d, want 200: %s", w.Code, w.Body.String())
	}

	// A DIFFERENT, brand-new row can never be created or edited into
	// "offering" — grandfathering only protects a row that was ALREADY
	// offering-scoped before the edit.
	newID, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: "other_metric", Value: "1", Source: "self_measured",
		SubjectType: "model", SubjectID: pq.modelID,
	})
	if err != nil {
		t.Fatalf("seed model-scoped benchmark: %v", err)
	}
	body = `{"metric":"other_metric","value":"1","source":"self_measured","subject_type":"offering","subject_id":42}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/benchmarks/"+itoa(newID), bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("re-scope model-scoped row TO offering = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// ── Note CRUD ────────────────────────────────────────────────────────────────

func TestCatalogNoteCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	body := `{"subject_type":"model","subject_id":` + itoa(pq.modelID) + `,"author":"testuser","body":"tends to truncate long outputs"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/notes", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create note = %d: %s", w.Code, w.Body.String())
	}
	var n noteJSON
	decodeJSON(t, w.Body, &n)
	if n.Body != "tends to truncate long outputs" || n.Author != "testuser" {
		t.Errorf("created note = %+v", n)
	}
	if n.CreatedAt == "" {
		t.Error("expected non-empty created_at")
	}

	// List filtered by subject.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/notes?subject_type=model&subject_id="+itoa(pq.modelID), nil))
	if w.Code != 200 {
		t.Fatalf("list notes = %d", w.Code)
	}
	var list []noteJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 note, got %d", len(list))
	}

	// Update.
	body = `{"subject_type":"model","subject_id":` + itoa(pq.modelID) + `,"author":"testuser","body":"updated note"}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/notes/"+itoa(n.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update note = %d: %s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/notes/"+itoa(n.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete note = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogNoteValidation(t *testing.T) {
	s, _ := newCatalogTestServer(t)
	// Missing body.
	body := `{"subject_type":"model","subject_id":1,"author":"testuser"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/notes", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("missing body = %d, want 422", w.Code)
	}
	// Invalid subject_type.
	body = `{"subject_type":"bogus","subject_id":1,"author":"testuser","body":"x"}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/notes", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("invalid subject_type = %d, want 422", w.Code)
	}
}

// ── Service CRUD ─────────────────────────────────────────────────────────────

func TestCatalogServiceCRUD(t *testing.T) {
	s, _ := newCatalogTestServer(t)

	body := `{"name":"aligner","label":"Aligner","description":"Alignment service","unit":"forge-aligner"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/services", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create service = %d: %s", w.Code, w.Body.String())
	}
	var sv serviceJSON
	decodeJSON(t, w.Body, &sv)
	if sv.Name != "aligner" || sv.Label != "Aligner" {
		t.Errorf("created service = %+v", sv)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/services", nil))
	if w.Code != 200 {
		t.Fatalf("list services = %d", w.Code)
	}
	var list []serviceJSON
	decodeJSON(t, w.Body, &list)
	if len(list) < 1 {
		t.Errorf("expected ≥1 service, got %d", len(list))
	}

	// Update.
	body = `{"name":"aligner","label":"Updated","description":"Updated desc","unit":"forge-aligner"}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/services/"+itoa(sv.ID), bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("update service = %d: %s", w.Code, w.Body.String())
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/services/"+itoa(sv.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete service = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogServiceValidation(t *testing.T) {
	s, _ := newCatalogTestServer(t)
	// Invalid name (uppercase).
	body := `{"name":"UPPERCASE","label":"x"}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/services", bytes.NewBufferString(body)))
	if w.Code != 422 {
		t.Fatalf("invalid name = %d, want 422", w.Code)
	}
}

// ── Enum list endpoints ──────────────────────────────────────────────────────

func TestCatalogEnumLists(t *testing.T) {
	s, _ := newCatalogTestServer(t)

	// Quantizations (seeded by migration).
	w := do(t, s, authedRequest("GET", "/api/v1/catalog/quantizations", nil))
	if w.Code != 200 {
		t.Fatalf("list quantizations = %d", w.Code)
	}
	var quants []store.Quantization
	decodeJSON(t, w.Body, &quants)
	if len(quants) == 0 {
		t.Error("expected non-empty quantizations")
	}

	// Formats.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/formats", nil))
	if w.Code != 200 {
		t.Fatalf("list formats = %d", w.Code)
	}
	var formats []store.Format
	decodeJSON(t, w.Body, &formats)
	if len(formats) == 0 {
		t.Error("expected non-empty formats")
	}

	// Engines.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/engines", nil))
	if w.Code != 200 {
		t.Fatalf("list engines = %d", w.Code)
	}
	var engines []engineJSON
	decodeJSON(t, w.Body, &engines)
	if len(engines) == 0 {
		t.Error("expected non-empty engines")
	}
}

// ── Filesystem browse ────────────────────────────────────────────────────────

func TestCatalogModelFiles(t *testing.T) {
	// Create a temp dir with a dummy GGUF file.
	dir := t.TempDir()
	ggufPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(ggufPath, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a shard set.
	if err := os.WriteFile(filepath.Join(dir, "big-00001-of-00003.gguf"), []byte("shard1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big-00002-of-00003.gguf"), []byte("shard2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big-00003-of-00003.gguf"), []byte("shard3"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-GGUF file (should be ignored).
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Paths:  config.Paths{ModelsDir: dir},
	})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
	})
	t.Cleanup(func() { s.Close() })

	w := do(t, s, authedRequest("GET", "/api/v1/models/files", nil))
	if w.Code != 200 {
		t.Fatalf("model files = %d: %s", w.Code, w.Body.String())
	}
	var files []modelFileJSON
	decodeJSON(t, w.Body, &files)

	// Expect 2 entries: "model.gguf" + "big" (shard set collapsed to one).
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}
	for _, f := range files {
		if f.IsShardSet && !strings.HasPrefix(f.Path, "big") {
			t.Errorf("shard set path = %q, expected 'big*'", f.Path)
		}
		if !f.IsShardSet && f.Path != "model.gguf" {
			t.Errorf("non-shard path = %q, expected 'model.gguf'", f.Path)
		}
	}
}

func TestCatalogModelFilesEmptyDir(t *testing.T) {
	s, _ := newCatalogTestServer(t)
	// ModelsDir is "" in the test config → empty list.
	w := do(t, s, authedRequest("GET", "/api/v1/models/files", nil))
	if w.Code != 200 {
		t.Fatalf("model files empty = %d", w.Code)
	}
	var files []modelFileJSON
	decodeJSON(t, w.Body, &files)
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

// ── InvalidateConfig hook ────────────────────────────────────────────────────

func TestCatalogInvalidateConfigHook(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	invalidated := false
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Catalog:   db.Catalog(),
		InvalidateConfig: func() {
			invalidated = true
		},
	})
	t.Cleanup(func() { s.Close() })

	// Create a model → should trigger InvalidateConfig.
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/models",
		bytes.NewBufferString(`{"name":"hook-test"}`)))
	if w.Code != 201 {
		t.Fatalf("create model = %d: %s", w.Code, w.Body.String())
	}
	if !invalidated {
		t.Error("InvalidateConfig was not called after model create")
	}
}

// ── Nil catalog (not wired) ──────────────────────────────────────────────────

func TestCatalogNilDeps(t *testing.T) {
	s := newTestServer(t) // no Catalog wired
	// List endpoints return empty arrays, not 501.
	w := do(t, s, authedRequest("GET", "/api/v1/catalog/models", nil))
	if w.Code != 200 {
		t.Fatalf("nil catalog list = %d, want 200", w.Code)
	}
	var list []modelJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d", len(list))
	}

	// Mutation endpoints return 503.
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models",
		bytes.NewBufferString(`{"name":"test"}`)))
	if w.Code != 503 {
		t.Errorf("nil catalog create = %d, want 503", w.Code)
	}
}

// ── Icon upload: PUT /api/v1/catalog/models/{id}/icon (Sprint A1, 2026-07-31) ──
//
// Regression coverage for the frontend Content-Type bug: with paths.icons_dir
// unset (the real ForgeHost config), a valid multipart upload must land as a
// data-URL in models.logo and return it in the response.

func TestCatalogModelIconUpload(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	png := []byte("\x89PNG\r\n\x1a\n-fake-but-whitelisted-payload")

	// Mirror a real browser file upload: filename + the file's true MIME type
	// (CreateFormFile would send application/octet-stream, which is not what a
	// browser's multipart encoder emits for a picked PNG).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="icon.png"`},
		"Content-Type":        {"image/png"},
	})
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}

	r := authedRequest("PUT", "/api/v1/catalog/models/"+itoa(pq.modelID)+"/icon", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := do(t, s, r)
	if w.Code != 200 {
		t.Fatalf("icon upload = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Logo string `json:"logo"`
	}
	decodeJSON(t, w.Body, &resp)
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(resp.Logo, prefix) {
		t.Fatalf("logo = %q, want data:image/png;base64,…", resp.Logo)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(resp.Logo, prefix))
	if err != nil {
		t.Fatalf("decode data URL: %v", err)
	}
	if !bytes.Equal(decoded, png) {
		t.Errorf("round-tripped icon bytes differ: got %d bytes, want %d", len(decoded), len(png))
	}

	// The model row must carry the data-URL logo now.
	ctx := t.Context()
	m, err := db.Catalog().GetModel(ctx, pq.modelID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if m.Logo != resp.Logo {
		t.Errorf("stored logo mismatch")
	}

	// A whitelisted-type violation is rejected 422.
	var bad bytes.Buffer
	mw2 := multipart.NewWriter(&bad)
	part2, err := mw2.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part2.Write([]byte("not an image")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw2.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	r2 := authedRequest("PUT", "/api/v1/catalog/models/"+itoa(pq.modelID)+"/icon", &bad)
	r2.Header.Set("Content-Type", mw2.FormDataContentType())
	w2 := do(t, s, r2)
	if w2.Code != 422 {
		t.Errorf("text upload = %d, want 422", w2.Code)
	}
}

// ── Helper ───────────────────────────────────────────────────────────────────

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

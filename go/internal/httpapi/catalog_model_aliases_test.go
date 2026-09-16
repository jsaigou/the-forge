// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"testing"
)

// TestCatalogModelAliasCRUD covers the model_aliases CRUD handlers (T3,
// per-request thinking control, 2026-09-14) — a thin HTTP wrapper over
// already-unit-tested store.Catalog methods, so this exists to catch wiring
// mistakes (route registration, JSON shape, validation) that store-level
// tests can't see, mirroring TestCatalogConfigCRUD's pattern.
func TestCatalogModelAliasCRUD(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	cfgBody := `{"name":"gemma4-26b-a4b","variant_id":` + itoa(pq.variantID) +
		`,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) +
		`,"build_id":` + itoa(pq.buildID) +
		`,"n_ctx":262144,"parallel":1}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(cfgBody)))
	if w.Code != 201 {
		t.Fatalf("create target config = %d: %s", w.Code, w.Body.String())
	}
	var cfg configJSON
	decodeJSON(t, w.Body, &cfg)

	// Create.
	aliasBody := `{"name":"gemma4-26b-a4b-nothink","config_id":` + itoa(cfg.ID) +
		`,"request_defaults":{"reasoning_effort":"none"}}`
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/model-aliases", bytes.NewBufferString(aliasBody)))
	if w.Code != 201 {
		t.Fatalf("create alias = %d: %s", w.Code, w.Body.String())
	}
	var a modelAliasJSON
	decodeJSON(t, w.Body, &a)
	if a.Name != "gemma4-26b-a4b-nothink" || a.ConfigID != cfg.ID || a.Visibility != "visible" {
		t.Errorf("created alias = %+v", a)
	}
	if a.RequestDefaults["reasoning_effort"] != "none" {
		t.Errorf("request_defaults = %+v", a.RequestDefaults)
	}

	// Validation: config_id must reference a real config.
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/model-aliases", bytes.NewBufferString(`{"name":"bad-alias","config_id":999999}`)))
	if w.Code != 422 {
		t.Errorf("create with bad config_id = %d, want 422", w.Code)
	}

	// Get.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/model-aliases/"+itoa(a.ID), nil))
	if w.Code != 200 {
		t.Fatalf("get alias = %d", w.Code)
	}

	// List.
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/model-aliases", nil))
	if w.Code != 200 {
		t.Fatalf("list aliases = %d", w.Code)
	}
	var list []modelAliasJSON
	decodeJSON(t, w.Body, &list)
	if len(list) != 1 {
		t.Errorf("expected 1 alias, got %d", len(list))
	}

	// Update.
	updateBody := `{"name":"gemma4-26b-a4b-nothink","config_id":` + itoa(cfg.ID) +
		`,"request_defaults":{"reasoning_effort":"low"},"visibility":"hidden"}`
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/model-aliases/"+itoa(a.ID), bytes.NewBufferString(updateBody)))
	if w.Code != 200 {
		t.Fatalf("update alias = %d: %s", w.Code, w.Body.String())
	}
	var a2 modelAliasJSON
	decodeJSON(t, w.Body, &a2)
	if a2.Visibility != "hidden" || a2.RequestDefaults["reasoning_effort"] != "low" {
		t.Errorf("updated alias = %+v", a2)
	}

	// Delete.
	w = do(t, s, authedRequest("DELETE", "/api/v1/catalog/model-aliases/"+itoa(a.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete alias = %d: %s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/model-aliases/"+itoa(a.ID), nil))
	if w.Code != 404 {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

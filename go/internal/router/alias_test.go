// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// TestChatCompletions_ModelAlias_ResolvesAndForcesDefaults is the full HTTP
// path for T3 (per-request thinking control): a request naming an alias
// (not a real catalog config) loads the alias's TARGET config and gets the
// alias's request_defaults forced onto the outgoing body — even overwriting
// a client-sent value, unlike Config.ReasoningEffortDefault's client-wins
// precedence. This is the exact mechanism gemma4-26b-a4b-nothink is meant
// to become: podcast_creator can't send reasoning_effort itself, so the
// alias forces it.
func TestChatCompletions_ModelAlias_ResolvesAndForcesDefaults(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	targetID := seedCatalogConfig(t, db.Catalog(), "gemma4-26b-a4b", 262144, "visible")
	if err := db.Catalog().UpdateConfigChatTemplateCaps(ctx, targetID, map[string]bool{
		"supports_reasoning_effort": true,
	}); err != nil {
		t.Fatalf("UpdateConfigChatTemplateCaps: %v", err)
	}
	if _, err := db.Catalog().CreateModelAlias(ctx, store.ModelAlias{
		Name:            "gemma4-26b-a4b-nothink",
		ConfigID:        targetID,
		RequestDefaults: map[string]any{"reasoning_effort": "none"},
	}); err != nil {
		t.Fatalf("CreateModelAlias: %v", err)
	}

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		Catalog:      cat,
		StoreCatalog: db.Catalog(),
		Sched:        &fixedSlotSched{slot: "a3"},
		Slots:        map[string]int{"a3": port},
		Auth:         &stubAuth{validToken: "x"},
	})

	// The client explicitly asks for "high" — the alias must still force
	// "none", proving forced defaults win over a client-sent value.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma4-26b-a4b-nothink","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if received["reasoning_effort"] != "none" {
		t.Errorf("upstream reasoning_effort = %v, want none (alias's forced default must win over the client's own 'high')", received["reasoning_effort"])
	}
}

// TestChatCompletions_ModelAlias_Hidden404s: a hidden alias must not be
// reachable, same as a genuinely unknown model name.
func TestChatCompletions_ModelAlias_Hidden404s(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	targetID := seedCatalogConfig(t, db.Catalog(), "base-config", 32768, "visible")
	if _, err := db.Catalog().CreateModelAlias(ctx, store.ModelAlias{
		Name: "hidden-alias", ConfigID: targetID, Visibility: "hidden",
	}); err != nil {
		t.Fatalf("CreateModelAlias: %v", err)
	}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"hidden-alias","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestChatCompletions_ModelAlias_TargetHiddenNotRoutable: an alias whose
// target config has since been hidden must not route either — the alias's
// own visibility is not the only gate.
func TestChatCompletions_ModelAlias_TargetHiddenNotRoutable(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	targetID := seedCatalogConfig(t, db.Catalog(), "now-hidden-config", 32768, "hidden")
	if _, err := db.Catalog().CreateModelAlias(ctx, store.ModelAlias{
		Name: "orphaned-alias", ConfigID: targetID,
	}); err != nil {
		t.Fatalf("CreateModelAlias: %v", err)
	}

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"orphaned-alias","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestBuildModelsResponse_ListsVisibleAliases: an alias must appear in
// /v1/models under its own name (so an existing consumer that can only be
// pointed at one fixed model string keeps working), carrying the target
// config's own context length.
func TestBuildModelsResponse_ListsVisibleAliases(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()

	targetID := seedCatalogConfig(t, cat, "gemma4-26b-a4b", 262144, "visible")
	if _, err := cat.CreateModelAlias(ctx, store.ModelAlias{
		Name: "gemma4-26b-a4b-nothink", ConfigID: targetID,
		RequestDefaults: map[string]any{"reasoning_effort": "none"},
	}); err != nil {
		t.Fatalf("CreateModelAlias: %v", err)
	}
	if _, err := cat.CreateModelAlias(ctx, store.ModelAlias{
		Name: "hidden-alias", ConfigID: targetID, Visibility: "hidden",
	}); err != nil {
		t.Fatalf("CreateModelAlias (hidden): %v", err)
	}

	resp := BuildModelsResponse(ctx, cat, nil)
	byID := map[string]ModelEntry{}
	for _, e := range resp.Data {
		byID[e.ID] = e
	}

	entry, ok := byID["gemma4-26b-a4b-nothink"]
	if !ok {
		t.Fatal("alias 'gemma4-26b-a4b-nothink' not listed in /v1/models")
	}
	if entry.ContextLength != 262144 {
		t.Errorf("alias ContextLength = %d, want 262144 (from its target config)", entry.ContextLength)
	}
	if entry.OwnedBy != "forge-local" {
		t.Errorf("alias OwnedBy = %q, want forge-local", entry.OwnedBy)
	}
	// The target config itself must still appear as its own separate entry.
	if _, ok := byID["gemma4-26b-a4b"]; !ok {
		t.Error("target config 'gemma4-26b-a4b' not listed alongside its alias")
	}
	if _, ok := byID["hidden-alias"]; ok {
		t.Error("hidden alias must not be listed")
	}
}

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

// TestChatCompletions_ReasoningEffort_NativePassthrough exercises the full
// HTTP path (T2, per-request thinking control): a config whose probed
// chat_template_caps declares native reasoning_effort support gets the
// client's requested level set directly on the outgoing upstream request.
func TestChatCompletions_ReasoningEffort_NativePassthrough(t *testing.T) {
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

	cfgID := seedCatalogConfig(t, db.Catalog(), "kintsugi-model", 32768, "visible")
	if err := db.Catalog().UpdateConfigChatTemplateCaps(ctx, cfgID, map[string]bool{
		"supports_reasoning_effort": true,
	}); err != nil {
		t.Fatalf("UpdateConfigChatTemplateCaps: %v", err)
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

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kintsugi-model","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if received["reasoning_effort"] != "low" {
		t.Errorf("upstream reasoning_effort = %v, want low", received["reasoning_effort"])
	}
}

// TestChatCompletions_ReasoningEffort_ConfigDefaultAppliesWhenClientSilent
// proves a config's curated ReasoningEffortDefault takes effect end-to-end
// when the client sends no reasoning_effort of its own — the mechanism that
// replaces today's launch-time flags.
func TestChatCompletions_ReasoningEffort_ConfigDefaultAppliesWhenClientSilent(t *testing.T) {
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

	cfgID := seedCatalogConfig(t, db.Catalog(), "gemma-model", 32768, "visible")
	cfg, err := db.Catalog().GetConfig(ctx, cfgID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ReasoningEffortDefault = "none"
	cfg.ChatTemplateCapsOverride = map[string]bool{"supports_enable_thinking": true}
	if err := db.Catalog().UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	// Real probed data (supports_reasoning_effort false, matching gemma4-e4b-qat
	// live on ForgeHost) — the override above is what supplies the enable_thinking
	// lever, since no probe covers it.
	if err := db.Catalog().UpdateConfigChatTemplateCaps(ctx, cfgID, map[string]bool{
		"supports_reasoning_effort": false,
	}); err != nil {
		t.Fatalf("UpdateConfigChatTemplateCaps: %v", err)
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

	// No reasoning_effort in the client's own request.
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma-model","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	kwargs, ok := received["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing from upstream request: %+v", received)
	}
	if kwargs["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false (config default 'none' translated)", kwargs["enable_thinking"])
	}
	if _, present := received["reasoning_effort"]; present {
		t.Error("reasoning_effort must not reach a build that doesn't understand it")
	}
}

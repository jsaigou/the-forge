// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/store"
)

func newHFTestServer(t *testing.T, ident authz.Identity, handler http.HandlerFunc) (*Server, *store.DB) {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dir := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{ModelsDir: dir}}
	hfClient := &hf.Client{HTTP: srv.Client(), BaseURL: srv.URL}
	dl := hfdownload.New(hfdownload.Deps{
		Store: db, HF: hfClient, Cfg: func() *config.Config { return cfg }, Logf: t.Logf,
	})

	s := newTestServerWith(t, ident)
	s.deps.HFDownload = dl
	s.deps.HFClient = hfClient
	s.deps.Settings = db.Settings()
	s.deps.Catalog = db.Catalog()
	return s, db
}

func TestHFRoutesUnwiredReturn503(t *testing.T) {
	s := newTestServerWith(t, authz.Identity{Name: "admin", Role: authz.RoleAdmin})
	for _, req := range []*http.Request{
		authedRequest("GET", "/api/v1/hf/search?q=x", nil),
		authedRequest("GET", "/api/v1/hf/tree?repo=org/model", nil),
		authedRequest("POST", "/api/v1/hf/preflight", bytes.NewReader([]byte(`{}`))),
		authedRequest("GET", "/api/v1/hf/downloads", nil),
	} {
		w := do(t, s, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 when unwired", req.Method, req.URL.Path, w.Code)
		}
	}
}

func TestHFSearchAndTree(t *testing.T) {
	s, _ := newHFTestServer(t, authz.Identity{Name: "op", Role: authz.RoleOperator}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case bytes.Contains([]byte(r.URL.Path), []byte("/tree/")):
			_, _ = w.Write([]byte(`[{"type":"file","path":"model-Q4_K_M.gguf","size":4000000000}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":"org/model","author":"org","downloads":100,"likes":5,"tags":["gguf"],"gated":false}]`))
		}
	})

	w := do(t, s, authedRequest("GET", "/api/v1/hf/search?q=qwen", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search status = %d, body=%s", w.Code, w.Body.String())
	}
	var searchResp hfSearchResponse
	decodeJSON(t, w.Body, &searchResp)
	if len(searchResp.Results) != 1 || searchResp.Results[0].ID != "org/model" {
		t.Errorf("search results = %+v", searchResp.Results)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/hf/tree?repo=org/model", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("tree status = %d, body=%s", w.Code, w.Body.String())
	}
	var treeResp hfTreeResponse
	decodeJSON(t, w.Body, &treeResp)
	if len(treeResp.Candidates) != 1 {
		t.Fatalf("tree candidates = %+v", treeResp.Candidates)
	}
}

func TestHFDownloadStartRequiresOperatorRole(t *testing.T) {
	s, _ := newHFTestServer(t, authz.Identity{Name: "viewer", Role: authz.RoleViewer}, nil)
	body := `{"repo":"org/model","files":[{"filename":"m.gguf","size_bytes":100}]}`
	w := do(t, s, authedRequest("POST", "/api/v1/hf/downloads", bytes.NewReader([]byte(body))))
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer POST /hf/downloads = %d, want 403", w.Code)
	}
}

func TestHFDownloadStartAndGetRoundTrip(t *testing.T) {
	payload := []byte("not a real gguf but the handler always 200s it")
	s, db := newHFTestServer(t, authz.Identity{Name: "op", Role: authz.RoleOperator}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	_ = db

	body := `{"repo":"testorg/testmodel","files":[{"filename":"m.gguf","size_bytes":47}]}`
	w := do(t, s, authedRequest("POST", "/api/v1/hf/downloads", bytes.NewReader([]byte(body))))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body=%s", w.Code, w.Body.String())
	}
	var started hfDownloadJSON
	decodeJSON(t, w.Body, &started)
	if started.ID == 0 || started.Repo != "testorg/testmodel" {
		t.Fatalf("started = %+v", started)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/hf/downloads/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", w.Code, w.Body.String())
	}
	var got hfDownloadJSON
	decodeJSON(t, w.Body, &got)
	if len(got.Files) != 1 {
		t.Errorf("get = %+v, want one file", got)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/hf/downloads", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var list hfDownloadsListResponse
	decodeJSON(t, w.Body, &list)
	if len(list.Downloads) != 1 {
		t.Errorf("list = %+v, want one job", list.Downloads)
	}
}

func TestHFDownloadGetNotFound(t *testing.T) {
	s, _ := newHFTestServer(t, authz.Identity{Name: "op", Role: authz.RoleOperator}, nil)
	w := do(t, s, authedRequest("GET", "/api/v1/hf/downloads/999", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("get missing job = %d, want 404", w.Code)
	}
}

func TestHFDownloadStartValidatesConfigName(t *testing.T) {
	s, db := newHFTestServer(t, authz.Identity{Name: "op", Role: authz.RoleOperator}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a real gguf but the handler always 200s it"))
	})

	ctx := context.Background()
	cat := db.Catalog()
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "existing"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "8b"})
	ggufFormat, _ := cat.FormatByName(ctx, "GGUF")
	artID, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: varID, FormatID: ggufFormat.ID, ArtifactType: "weight", FilePath: "old.gguf", FileSizeBytes: 1})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	if _, err := cat.CreateConfig(ctx, store.Config{Name: "existing-8b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID, Status: "unverified", Visibility: "visible"}); err != nil {
		t.Fatalf("seed CreateConfig: %v", err)
	}

	// An unknown config_name must fail fast, before any download work begins.
	body := `{"repo":"testorg/testmodel","files":[{"filename":"m.gguf","size_bytes":47}],"config_name":"does-not-exist"}`
	w := do(t, s, authedRequest("POST", "/api/v1/hf/downloads", bytes.NewReader([]byte(body))))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown config_name status = %d, body=%s, want 422", w.Code, w.Body.String())
	}

	// A real config_name must be accepted and pass straight through to the job.
	body = `{"repo":"testorg/testmodel","files":[{"filename":"m.gguf","size_bytes":47}],"config_name":"existing-8b"}`
	w = do(t, s, authedRequest("POST", "/api/v1/hf/downloads", bytes.NewReader([]byte(body))))
	if w.Code != http.StatusAccepted {
		t.Fatalf("valid config_name status = %d, body=%s, want 202", w.Code, w.Body.String())
	}
}

func TestHFTokenMaskedRoundTrip(t *testing.T) {
	s, _ := newHFTestServer(t, authz.Identity{Name: "admin", Role: authz.RoleAdmin}, nil)

	// Not configured yet.
	w := do(t, s, authedRequest("GET", "/api/v1/hf/token", nil))
	var got hfTokenResponse
	decodeJSON(t, w.Body, &got)
	if got.Configured {
		t.Fatal("token should not be configured before any PUT")
	}

	// Set a real token.
	setBody, _ := json.Marshal(hfTokenBody{Token: "hf_realtokenvalue1234567890"})
	w = do(t, s, authedRequest("PUT", "/api/v1/hf/token", bytes.NewReader(setBody)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", w.Code, w.Body.String())
	}
	var putResp hfTokenResponse
	decodeJSON(t, w.Body, &putResp)
	if putResp.Token == "hf_realtokenvalue1234567890" {
		t.Fatal("PUT response must never echo the real token, even right after setting it")
	}
	if !putResp.Configured {
		t.Fatal("Configured must be true after setting a real token")
	}

	// GET returns the same masked form.
	w = do(t, s, authedRequest("GET", "/api/v1/hf/token", nil))
	decodeJSON(t, w.Body, &got)
	if got.Token != putResp.Token || !got.Configured {
		t.Errorf("GET after PUT = %+v, want it to match the PUT response's masked value", got)
	}

	// Round-tripping the MASKED value back through PUT must not clobber
	// the real token — the masked-value-means-unchanged contract.
	roundTripBody, _ := json.Marshal(hfTokenBody{Token: got.Token})
	w = do(t, s, authedRequest("PUT", "/api/v1/hf/token", bytes.NewReader(roundTripBody)))
	if w.Code != http.StatusOK {
		t.Fatalf("round-trip PUT status = %d", w.Code)
	}
	var afterRoundTrip hfTokenResponse
	decodeJSON(t, w.Body, &afterRoundTrip)
	if afterRoundTrip.Token != got.Token {
		t.Errorf("round-tripping the masked value changed it: before=%q after=%q — a real token may have been clobbered", got.Token, afterRoundTrip.Token)
	}
}

func TestHFTokenWriteRequiresAdmin(t *testing.T) {
	s, _ := newHFTestServer(t, authz.Identity{Name: "op", Role: authz.RoleOperator}, nil)
	body, _ := json.Marshal(hfTokenBody{Token: "hf_x"})
	w := do(t, s, authedRequest("PUT", "/api/v1/hf/token", bytes.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Errorf("operator PUT /hf/token = %d, want 403 (admin only)", w.Code)
	}
}

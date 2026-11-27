// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func chatTo(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

const imageRequest = `{"model":"%s","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`

func imageBody(model string) string { return strings.Replace(imageRequest, "%s", model, 1) }

func TestImageToTextOnlyConfigIsRefusedBeforeDispatch(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := db.Catalog()
	// Model is vision-capable on paper but this config has no mmproj: text only (ModalityReasonNoMMProj).
	seedCatalogConfig(t, cat, "textonly", 8192, "visible", seedConfigOpts{modelModalities: []string{"text", "vision"}})
	seedCatalogConfig(t, cat, "withvision", 8192, "visible", seedConfigOpts{modelModalities: []string{"text", "vision"}, withMMProj: true})
	seedCatalogConfig(t, cat, "secret", 8192, "hidden", seedConfigOpts{modelModalities: []string{"text", "vision"}})
	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), StoreCatalog: cat, Auth: &stubAuth{validToken: "x"}})

	rec := chatTo(t, srv, imageBody("textonly"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error":"unsupported_modality"`) ||
		!strings.Contains(rec.Body.String(), `"modality":"vision"`) {
		t.Fatalf("image to a text-only config: %d %s, want 400 unsupported_modality (vision)", rec.Code, rec.Body.String())
	}

	// Text to the same config is not gated (it reaches the scheduler check, which is unwired here).
	rec = chatTo(t, srv, `{"model":"textonly","messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(rec.Body.String(), "unsupported_modality") {
		t.Errorf("a text request was refused: %s", rec.Body.String())
	}
	// A config with its mmproj is not gated.
	rec = chatTo(t, srv, imageBody("withvision"))
	if strings.Contains(rec.Body.String(), "unsupported_modality") {
		t.Errorf("an image request to a vision config was refused: %s", rec.Body.String())
	}
	// A name the catalog does not know is not gated; it gets the ordinary answer.
	rec = chatTo(t, srv, imageBody("nope"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error":"model_not_found"`) {
		t.Errorf("unknown model: %d %s, want 404 model_not_found", rec.Code, rec.Body.String())
	}
}

func TestHiddenModelAnswersModelNotRoutable(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := db.Catalog()
	seedCatalogConfig(t, cat, "secret", 8192, "hidden")
	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), StoreCatalog: cat, Auth: &stubAuth{validToken: "x"}})

	rec := chatTo(t, srv, `{"model":"secret","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error":"model_not_routable"`) ||
		!strings.Contains(rec.Body.String(), "hidden") {
		t.Errorf("hidden config: %d %s, want 404 model_not_routable", rec.Code, rec.Body.String())
	}
	rec = chatTo(t, srv, `{"model":"does-not-exist","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"error":"model_not_found"`) {
		t.Errorf("unknown: %d %s, want 404 model_not_found", rec.Code, rec.Body.String())
	}
	// A hidden image request is a hidden-model answer, not a modality one.
	rec = chatTo(t, srv, imageBody("secret"))
	if !strings.Contains(rec.Body.String(), "model_not_routable") {
		t.Errorf("hidden + image: %s, want model_not_routable", rec.Body.String())
	}
}

func TestRequestModalities(t *testing.T) {
	cases := []struct {
		name          string
		body          map[string]any
		vision, audio bool
	}{
		{"string content", map[string]any{"messages": []any{map[string]any{"content": "hi"}}}, false, false},
		{"text part", map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "text"}}}}}, false, false},
		{"image part", map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}}}, true, false},
		{"audio part", map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "input_audio"}}}}}, false, true},
	}
	for _, c := range cases {
		if v, a := requestModalities(c.body); v != c.vision || a != c.audio {
			t.Errorf("%s: got vision=%v audio=%v, want %v %v", c.name, v, a, c.vision, c.audio)
		}
	}
}

// flakyVariantCatalog is the real catalog whose Variant lookups fail, as a read error would.
type flakyVariantCatalog struct{ store.Catalog }

func (flakyVariantCatalog) GetVariant(context.Context, int64) (store.Variant, error) {
	return store.Variant{}, errors.New("disk I/O error")
}

// An unresolvable catalog never blocks a request (the gate fails open), but the failure is no longer silent: it is logged.
func TestModalityGateFailsOpenAndLogsWhenTheCatalogCannotAnswer(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := db.Catalog()
	seedCatalogConfig(t, cat, "textonly", 8192, "visible", seedConfigOpts{modelModalities: []string{"text", "vision"}})
	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), StoreCatalog: flakyVariantCatalog{cat}, Auth: &stubAuth{validToken: "x"}})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	rec := chatTo(t, srv, imageBody("textonly"))
	if strings.Contains(rec.Body.String(), "unsupported_modality") {
		t.Errorf("the gate refused a request it could not evaluate: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "modality gate") || !strings.Contains(logs.String(), "disk I/O error") {
		t.Errorf("the failed lookup was not logged: %q", logs.String())
	}
}

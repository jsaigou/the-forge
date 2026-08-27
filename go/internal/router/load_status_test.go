// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// loadStatusSched is a minimal sched.Scheduler fake returning a canned
// LoadState, for exercising the GET /v1/load-status handler in isolation.
type loadStatusSched struct {
	sched.Stub
	state    sched.LoadState
	gotModel string
}

func (s *loadStatusSched) LoadStatus(model string) sched.LoadState {
	s.gotModel = model
	return s.state
}

func TestLoadStatus_Passthrough(t *testing.T) {
	fake := &loadStatusSched{state: sched.LoadState{
		Model: "llama", State: "queued", QueuePosition: 2, Reason: sched.ReasonIdleThresholdNotMet,
	}}
	cfg := testCfg(nil, nil)
	srv := NewWithDeps(Deps{Cfg: cfg, Auth: &stubAuth{validToken: "x"}, Sched: fake})

	req := httptest.NewRequest("GET", "/v1/load-status?model=llama", nil)
	req.RemoteAddr = "100.64.0.1:1234" // tailnet
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if fake.gotModel != "llama" {
		t.Fatalf("model passed to scheduler = %q, want llama", fake.gotModel)
	}
	body := w.Body.String()
	for _, want := range []string{`"state":"queued"`, `"queue_position":2`, `"reason":"idle_threshold_not_met"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, missing %q", body, want)
		}
	}
}

func TestLoadStatus_RequiresModel(t *testing.T) {
	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), Auth: &stubAuth{validToken: "x"}, Sched: &loadStatusSched{}})
	req := httptest.NewRequest("GET", "/v1/load-status", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestLoadStatus_NotConfigured(t *testing.T) {
	srv := NewWithDeps(Deps{Cfg: testCfg(nil, nil), Auth: &stubAuth{validToken: "x"}}) // no Sched
	req := httptest.NewRequest("GET", "/v1/load-status?model=llama", nil)
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestLoadStatus_TailnetAuth(t *testing.T) {
	srv := NewWithDeps(Deps{
		Cfg:   testCfg(nil, nil),
		Auth:  &stubAuth{validToken: "sk-router-aaaaaaaaaaaa-xxxxxxxxxxxxxxxx"},
		Sched: &loadStatusSched{state: sched.LoadState{Model: "llama", State: "idle"}},
	})

	req := httptest.NewRequest("GET", "/v1/load-status?model=llama", nil)
	req.RemoteAddr = "8.8.8.8:1234" // non-tailnet, no bearer
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-tailnet no-bearer = %d, want 401", w.Code)
	}
}

// failingCatalogSched fails every load with a canned RefusalReason,
// exercising catalogChain's threading of that reason into the
// catalog_load_failed 502 body (proxy.go) via LoadStatus.
type failingCatalogSched struct {
	sched.Stub
	reason sched.RefusalReason
}

func (s *failingCatalogSched) EnsureLoaded(_ context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	return sched.Ticket{TicketID: "t", Model: req.Model, Status: "failed"}, nil
}

func (s *failingCatalogSched) LoadStatus(model string) sched.LoadState {
	return sched.LoadState{Model: model, State: "failed", Reason: s.reason, Message: "no evictable slot"}
}

func TestChatCompletions_CatalogLoadFailed_SurfacesRefusalReason(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedCatalogConfig(t, db.Catalog(), "gemma4-26b-mtp", 0, "visible")

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		StoreCatalog: db.Catalog(),
		Sched:        &failingCatalogSched{reason: sched.ReasonNoEvictableIdle},
		Auth:         &stubAuth{validToken: "x"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gemma4-26b-mtp","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error":"catalog_load_failed"`) {
		t.Fatalf("body = %s, want catalog_load_failed", body)
	}
	if !strings.Contains(body, `"reason":"no_evictable_slot_idle"`) {
		t.Fatalf("body = %s, want the RefusalReason surfaced as a structured field", body)
	}
}

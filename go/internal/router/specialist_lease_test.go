// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// P5 (docs/speech-routing-plan.md decision 7): a request sent through viaPool holds the pool's lease while the member answers and gives it back when the
// response has ended, for every function. The pool never evicts a member that holds one, so an in-flight request cannot lose its engine.

func TestPool_ViaPoolHoldsALeaseWhileTheMemberAnswersAndReleasesAfter(t *testing.T) {
	e, _ := newAudioEnv(t)
	var pool *fakePool
	heldDuring := -1
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, _, _, _ := pool.snapshot()
		heldDuring = h
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"pool-member"}`))
	}))
	t.Cleanup(member.Close)
	pool = &fakePool{target: member.URL, leases: true}
	e.srv.deps.Specialists = pool

	for _, path := range []string{"/v1/embeddings", "/v1/align"} {
		heldDuring = -1
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "100.64.0.1:1234"
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body)
		}
		if heldDuring != 1 {
			t.Errorf("%s: %d leases held while the member answered, want 1", path, heldDuring)
		}
		if held, _, _, _, _ := pool.snapshot(); held != 0 {
			t.Errorf("%s: %d leases still held after the response", path, held)
		}
	}
	if _, acquired, released, _, touched := pool.snapshot(); acquired != 2 || released != 2 || touched != 0 {
		t.Errorf("acquired=%d released=%d touched=%d, want 2, 2 and no separate touch (a release restarts the idle clock)", acquired, released, touched)
	}
}

// An upstream that cannot be reached still gives the lease back.
func TestPool_ViaPoolReleasesTheLeaseWhenTheMemberIsUnreachable(t *testing.T) {
	e, _ := newAudioEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	pool := &fakePool{target: url, leases: true}
	e.srv.deps.Specialists = pool
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"hi"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s, want 502", rr.Code, rr.Body)
	}
	if held, acquired, released, _, _ := pool.snapshot(); held != 0 || acquired != 1 || released != 1 {
		t.Errorf("held=%d acquired=%d released=%d, want 0, 1, 1", held, acquired, released)
	}
}

// A client that goes away while the member is still working does not leave the lease behind.
func TestPool_ViaPoolReleasesTheLeaseWhenTheClientDisconnects(t *testing.T) {
	e, _ := newAudioEnv(t)
	started, unblock := make(chan struct{}), make(chan struct{})
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}))
	t.Cleanup(func() { close(unblock); member.Close() })
	pool := &fakePool{target: member.URL, leases: true}
	e.srv.deps.Specialists = pool

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"hi"}`)).WithContext(ctx)
	req.RemoteAddr = "100.64.0.1:1234"
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.srv.Handler().ServeHTTP(rr, req); close(done) }()
	<-started
	if held, _, _, _, _ := pool.snapshot(); held != 1 {
		t.Fatalf("held = %d while the request is in flight, want 1", held)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not return after the client went away")
	}
	if held, _, released, _, _ := pool.snapshot(); held != 0 || released != 1 {
		t.Errorf("after the client left: held=%d released=%d, want 0 and 1", held, released)
	}
}

// A response that takes longer than the lease keeps renewing it, and stops once the request is over.
func TestPool_ViaPoolRenewsTheLeaseWhileTheResponseIsOpenAndStopsAfter(t *testing.T) {
	old := leaseRenewEvery
	leaseRenewEvery = 15 * time.Millisecond
	t.Cleanup(func() { leaseRenewEvery = old })
	e, _ := newAudioEnv(t)
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(member.Close)
	pool := &fakePool{target: member.URL, leases: true}
	e.srv.deps.Specialists = pool
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"hi"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	e.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	_, _, _, renewedDuring, _ := pool.snapshot()
	if renewedDuring < 3 {
		t.Errorf("renewed %d times during a 250 ms response with a 15 ms period, want several", renewedDuring)
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, _, renewedAfter, _ := pool.snapshot(); renewedAfter != renewedDuring {
		t.Errorf("renewal went on after the request ended (%d then %d)", renewedDuring, renewedAfter)
	}
}

// A pooled catalog config's lease is given back when the chain finishes (the Backend's Done hook), not only touched.
func TestPool_PooledCatalogConfigReleasesItsLeaseWhenTheRequestFinishes(t *testing.T) {
	e := newD1Env(t)
	var pool *fakePool
	heldDuring := -1
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool.mu.Lock()
		heldDuring = pool.held
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"d1-test","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":7,"output_tokens":0},"from":"pool-member"}`))
	}))
	t.Cleanup(member.Close)
	port := portFromURL(t, member.URL)
	e.srv.deps.Catalog.(*fakeCatalog).setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	pool = &fakePool{configs: map[string]int{"d1-test": port}, leases: true}
	e.srv.deps.Specialists = pool
	if rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`); rec.Code != 200 {
		t.Fatalf("pooled config: %d %s", rec.Code, rec.Body)
	}
	if heldDuring != 1 {
		t.Errorf("%d leases held while the member answered, want 1", heldDuring)
	}
	if held, acquired, released, _, _ := pool.snapshot(); held != 0 || acquired != 1 || released != 1 {
		t.Errorf("held=%d acquired=%d released=%d, want 0, 1, 1", held, acquired, released)
	}
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
)

// subscribeSpy is a bus.Subscriber that records how many response bytes had already been written when
// Subscribe was called.
type subscribeSpy struct {
	rec        *httptest.ResponseRecorder
	bytesAtSub int
	called     chan struct{}
}

func (s *subscribeSpy) Subscribe(ctx context.Context) <-chan bus.Event {
	s.bytesAtSub = s.rec.Body.Len()
	close(s.called)
	return make(chan bus.Event)
}

// TestSSESubscribesBeforeInitialSnapshot pins the ordering fix: handleSSE must register its bus
// subscription BEFORE it writes the initial status_update. A client that has read the initial event may
// immediately trigger an action; with the old order (snapshot, then Subscribe) an event published in
// between was lost to that client — seen as a 25 s stall + ": keepalive" in TestSSEBusEventsDeliverOverRealWire on a
// slow CI runner. Asserting the order directly keeps this deterministic (no timing involved).
func TestSSESubscribesBeforeInitialSnapshot(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	spy := &subscribeSpy{rec: rec, called: make(chan struct{})}
	s.deps.Events = spy

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleSSE(rec, req)
	}()
	select {
	case <-spy.called:
	case <-time.After(5 * time.Second):
		t.Fatal("handleSSE never called Subscribe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleSSE did not return after the client disconnected")
	}
	if spy.bytesAtSub != 0 {
		t.Fatalf("%d bytes were already written when Subscribe was called; Subscribe must come before the initial snapshot", spy.bytesAtSub)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the initial status_update snapshot was never written")
	}
}

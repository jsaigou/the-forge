// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type fakeSettings struct {
	mu sync.Mutex
	kv map[string][]byte
}

func newFakeSettings() *fakeSettings { return &fakeSettings{kv: make(map[string][]byte)} }

func (f *fakeSettings) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.kv[key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeSettings) Set(_ context.Context, key string, val []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = val
	return nil
}

type fakePublisher struct {
	mu     sync.Mutex
	events []string
}

func (f *fakePublisher) Publish(name string, _ any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, name)
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func TestGate_EnterExit(t *testing.T) {
	pub := &fakePublisher{}
	g := New(newFakeSettings(), pub, time.Now, nil)

	st, err := g.Enter(EnterRequest{Reason: "test repair", EnteredBy: "testuser", Duration: time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if !st.Active || st.LeaseID == "" {
		t.Fatalf("Enter did not produce an active leased window: %+v", st)
	}
	if pub.count() != 1 {
		t.Fatalf("expected 1 published event after Enter, got %d", pub.count())
	}

	blocked, msg := g.Blocked(context.Background())
	if !blocked || msg == "" {
		t.Fatalf("expected Blocked=true with a message, got %v %q", blocked, msg)
	}

	if _, err := g.Enter(EnterRequest{Reason: "second"}); err != ErrAlreadyActive {
		t.Fatalf("expected ErrAlreadyActive for a second Enter, got %v", err)
	}

	prev, err := g.Exit(st.LeaseID, false)
	if err != nil {
		t.Fatalf("Exit: %v", err)
	}
	if prev.LeaseID != st.LeaseID {
		t.Fatalf("Exit returned the wrong prior state")
	}
	if pub.count() != 2 {
		t.Fatalf("expected 2 published events after Exit, got %d", pub.count())
	}
	if g.Status().Active {
		t.Fatal("window still reports active after Exit")
	}
}

func TestGate_ExitWrongLeaseRejectedUnlessForced(t *testing.T) {
	g := New(newFakeSettings(), nil, time.Now, nil)
	st, err := g.Enter(EnterRequest{Reason: "r", Duration: time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}

	if _, err := g.Exit("not-the-lease", false); err == nil {
		t.Fatal("expected Exit with the wrong lease to fail")
	}
	if !g.Status().Active {
		t.Fatal("a rejected Exit must not have closed the window")
	}

	if _, err := g.Exit("not-the-lease", true); err != nil {
		t.Fatalf("forced Exit should succeed regardless of lease: %v", err)
	}
	if g.Status().Active {
		t.Fatal("window still active after forced Exit")
	}
	_ = st
}

func TestGate_ExitNotActive(t *testing.T) {
	g := New(newFakeSettings(), nil, time.Now, nil)
	if _, err := g.Exit("anything", false); err != ErrNotActive {
		t.Fatalf("expected ErrNotActive, got %v", err)
	}
}

func TestGate_TTLExpiry(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	pub := &fakePublisher{}
	g := New(newFakeSettings(), pub, func() time.Time { return clock() }, nil)

	if _, err := g.Enter(EnterRequest{Reason: "r", Duration: time.Minute}); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if blocked, _ := g.Blocked(context.Background()); !blocked {
		t.Fatal("expected blocked immediately after Enter")
	}

	now = now.Add(2 * time.Minute)
	if blocked, _ := g.Blocked(context.Background()); blocked {
		t.Fatal("expected the window to have auto-expired past its TTL")
	}
	if g.Status().Active {
		t.Fatal("Status still reports active past TTL")
	}
	if pub.count() != 2 { // enter + expire
		t.Fatalf("expected 2 published events (enter, expire), got %d", pub.count())
	}
}

func TestGate_DurationClampedToMax(t *testing.T) {
	g := New(newFakeSettings(), nil, time.Now, nil)
	st, err := g.Enter(EnterRequest{Reason: "r", Duration: 100 * time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	got := time.Unix(st.ExpiresAt, 0).Sub(time.Unix(st.EnteredAt, 0))
	if got != DefaultMaxDuration {
		t.Fatalf("expected duration clamped to %v, got %v", DefaultMaxDuration, got)
	}
}

func TestGate_WithLeaseBypassesBlock(t *testing.T) {
	g := New(newFakeSettings(), nil, time.Now, nil)
	st, err := g.Enter(EnterRequest{Reason: "r", Duration: time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}

	if blocked, _ := g.Blocked(context.Background()); !blocked {
		t.Fatal("expected an unrelated caller to be blocked")
	}

	leased := WithLease(context.Background(), st.LeaseID)
	if blocked, _ := g.Blocked(leased); blocked {
		t.Fatal("expected the lease-holding caller to pass through unblocked")
	}

	other := WithLease(context.Background(), "some-other-lease")
	if blocked, _ := g.Blocked(other); !blocked {
		t.Fatal("expected a mismatched lease to still be blocked")
	}
}

func TestGate_PersistsAcrossNewInstance(t *testing.T) {
	settings := newFakeSettings()
	g1 := New(settings, nil, time.Now, nil)
	st, err := g1.Enter(EnterRequest{Reason: "r", Duration: time.Hour})
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}

	g2 := New(settings, nil, time.Now, nil)
	got := g2.Status()
	if !got.Active || got.LeaseID != st.LeaseID {
		t.Fatalf("expected a fresh Gate to load persisted state, got %+v", got)
	}
}

func TestGate_ReconcileOnBootForceExitsAndAudits(t *testing.T) {
	settings := newFakeSettings()
	g1 := New(settings, nil, time.Now, nil)
	if _, err := g1.Enter(EnterRequest{Reason: "r", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter: %v", err)
	}

	pub := &fakePublisher{}
	g2 := New(settings, pub, time.Now, nil)
	if !g2.Status().Active {
		t.Fatal("precondition: fresh instance should have loaded the active window")
	}

	var auditReason string
	g2.ReconcileOnBoot(func(reason string) { auditReason = reason })

	if g2.Status().Active {
		t.Fatal("ReconcileOnBoot did not force-exit the orphaned window")
	}
	if auditReason == "" {
		t.Fatal("expected ReconcileOnBoot to call the audit callback with a reason")
	}
	if pub.count() != 1 {
		t.Fatalf("expected ReconcileOnBoot to publish exactly one changed event, got %d", pub.count())
	}
}

func TestGate_ReconcileOnBootNoopWhenInactive(t *testing.T) {
	pub := &fakePublisher{}
	g := New(newFakeSettings(), pub, time.Now, nil)
	called := false
	g.ReconcileOnBoot(func(string) { called = true })
	if called {
		t.Fatal("ReconcileOnBoot must not audit when nothing was active")
	}
	if pub.count() != 0 {
		t.Fatalf("expected no published events, got %d", pub.count())
	}
}

func TestGate_NilSettingsInMemoryOnly(t *testing.T) {
	g := New(nil, nil, time.Now, nil)
	if _, err := g.Enter(EnterRequest{Reason: "r", Duration: time.Hour}); err != nil {
		t.Fatalf("Enter with nil settings should still work in-memory: %v", err)
	}
	if !g.Status().Active {
		t.Fatal("expected in-memory state to still report active")
	}
}

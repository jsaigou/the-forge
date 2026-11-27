package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexremote"
)

func TestParseTenants(t *testing.T) {
	got, err := parseTenants(" haru , sonata=sonata-v2 ,")
	if err != nil || len(got) != 2 || got["haru"] != "haru" || got["sonata"] != "sonata-v2" {
		t.Fatalf("parse = %v, %v", got, err)
	}
	for name, spec := range map[string]string{"empty": "", "only commas": " , ,", "no name": "=pool", "twice": "a,a=b"} {
		if _, err := parseTenants(spec); err == nil {
			t.Errorf("%s: %q was accepted", name, spec)
		}
	}
}

type recLocator struct {
	mu       sync.Mutex
	base     string
	asked    []string
	released []string
}

func (l *recLocator) Locate(_ context.Context, engine string) (duplexremote.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, engine)
	return duplexremote.Lease{BaseURL: l.base, ID: "lease-" + engine, Expires: time.Now().Add(10 * time.Minute)}, nil
}
func (l *recLocator) Renew(context.Context, string) error { return nil }
func (l *recLocator) Release(_ context.Context, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, id)
}
func (l *recLocator) snapshot() (asked, released []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.asked...), append([]string(nil), l.released...)
}

// The whole path: a client asks for a tenant, the service finds that tenant's pool engine through the locator, the session runs on it, and closing
// the session gives the lease back. An unknown tenant is refused at start with the list of what is offered.
func TestPoolBackedSessionLeasesItsEngineAndReleasesIt(t *testing.T) {
	eng := duplexremote.NewMockEngine(duplexremote.MockEngineOptions{Tenant: "haru"})
	engSrv := httptest.NewServer(eng.Handler())
	t.Cleanup(engSrv.Close)
	loc := &recLocator{base: engSrv.URL}
	newFor := poolDrivers(duplexremote.Config{StepTimeout: time.Second}, loc, map[string]string{"haru": "haru-pool"})
	e := newTestEnv(t, Options{NewDriverFor: newFor, DefaultTenant: "haru"})

	conn := e.mustDial(t, nil)
	sendStart(t, conn, `,"tenant":"haru"`)
	ready := expectReady(t, conn)
	if ready["tenant"] != "haru" {
		t.Errorf("ready tenant = %v", ready["tenant"])
	}
	sendAudio(t, conn, 0, 7)
	readAudio(t, conn)
	if asked, released := loc.snapshot(); len(asked) != 1 || asked[0] != "haru-pool" || len(released) != 0 {
		t.Fatalf("asked %v released %v: want the pool engine located once and held while the session runs", asked, released)
	}
	_ = conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, released := loc.snapshot(); len(released) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, released := loc.snapshot(); len(released) != 1 || released[0] != "lease-haru-pool" {
		t.Fatalf("released %v: closing the session must give the lease back", released)
	}

	conn2 := e.mustDial(t, nil)
	sendStart(t, conn2, `,"tenant":"nope"`)
	expectError(t, conn2, duplex.ErrTenantUnavailable)
	if asked, _ := loc.snapshot(); len(asked) != 1 {
		t.Errorf("an unknown tenant reached the locator: %v", asked)
	}
}

func TestPoolDriversNameWhatIsOffered(t *testing.T) {
	newFor := poolDrivers(duplexremote.Config{}, &recLocator{}, map[string]string{"b": "b", "a": "a"})
	_, err := newFor("zzz")
	var de *duplex.Error
	if !errors.As(err, &de) || de.Code != duplex.ErrTenantUnavailable || !strings.Contains(de.Message, "a, b") {
		t.Fatalf("err = %v, want tenant_unavailable listing a, b", err)
	}
}

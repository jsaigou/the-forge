package duplexremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// ADR-0029: the session leases its engine for as long as it lives. These tests pin the lifecycle (locate, renew, release), what happens when the
// lease is lost, and the mapping of the daemon's error codes.

type fakeLocator struct {
	mu        sync.Mutex
	lease     Lease
	locateErr error
	renewErr  func(n int) error
	located   []string
	renewed   int
	released  []string
}

func (f *fakeLocator) Locate(_ context.Context, engine string) (Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.located = append(f.located, engine)
	return f.lease, f.locateErr
}
func (f *fakeLocator) Renew(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed++
	if f.renewErr != nil {
		return f.renewErr(f.renewed)
	}
	return nil
}
func (f *fakeLocator) Release(_ context.Context, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
}
func (f *fakeLocator) counts() (renewed int, released []string, located []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewed, append([]string(nil), f.released...), append([]string(nil), f.located...)
}

// leased starts a mock engine and a Tenant whose engine is found through a fake locator.
func leased(t *testing.T, loc *fakeLocator, mutate func(*Config)) (*MockEngine, *Tenant) {
	t.Helper()
	m := NewMockEngine(MockEngineOptions{Tenant: "haru"})
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	loc.lease.BaseURL = srv.URL
	if loc.lease.ID == "" {
		loc.lease.ID = "lease-1"
	}
	if loc.lease.Expires.IsZero() {
		loc.lease.Expires = time.Now().Add(10 * time.Minute)
	}
	cfg := Config{Tenant: "haru", Locator: loc, StepTimeout: 400 * time.Millisecond, RenewInterval: 20 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	tn := New(cfg)
	t.Cleanup(func() { _ = tn.Close() })
	return m, tn
}

func TestLocator_StartsOnTheLeasedEngineAndReleasesAtClose(t *testing.T) {
	loc := &fakeLocator{}
	m, tn := leased(t, loc, func(c *Config) { c.Engine = "haru-pool" })
	// The engine is not running when the service starts, so Load must not need it.
	if info, err := tn.Load(context.Background()); err != nil || info.Name != "haru" || !info.Capabilities.Text {
		t.Fatalf("Load with a locator: %+v, %v", info, err)
	}
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", Text: true, SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Step(context.Background(), testFrame(1), 0); err != nil {
		t.Fatal(err)
	}
	_, released, located := loc.counts()
	if len(located) != 1 || located[0] != "haru-pool" || len(released) != 0 {
		t.Fatalf("located %v released %v: want one locate of haru-pool and nothing released while the session lives", located, released)
	}
	waitActive(t, m, 1)
	_ = s.Close()
	_ = s.Close() // idempotent: one release
	if _, released, _ := loc.counts(); len(released) != 1 || released[0] != "lease-1" {
		t.Fatalf("released %v, want exactly lease-1 once", released)
	}
	waitActive(t, m, 0)
}

func TestLocator_TheEngineNameDefaultsToTheTenant(t *testing.T) {
	loc := &fakeLocator{}
	_, tn := leased(t, loc, nil)
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, located := loc.counts(); len(located) != 1 || located[0] != "haru" {
		t.Fatalf("located %v, want [haru]", located)
	}
}

func TestLocator_TheLeaseIsRenewedWhileTheSessionLivesAndNotAfter(t *testing.T) {
	loc := &fakeLocator{}
	_, tn := leased(t, loc, nil)
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if n, _, _ := loc.counts(); n < 3 {
		t.Fatalf("renewed %d times in 150 ms at a 20 ms interval; the lease is not being kept", n)
	}
	_ = s.Close()
	time.Sleep(60 * time.Millisecond)
	after, _, _ := loc.counts()
	time.Sleep(100 * time.Millisecond)
	if again, _, _ := loc.counts(); again != after {
		t.Fatalf("renewals continued after Close (%d then %d)", after, again)
	}
}

func TestLocator_ALostLeaseEndsTheSessionAtTheNextStep(t *testing.T) {
	loc := &fakeLocator{renewErr: func(n int) error {
		if n >= 2 {
			return ErrLeaseGone
		}
		return nil
	}}
	_, tn := leased(t, loc, nil)
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.Now().Add(2 * time.Second)
	var stepErr error
	for seq := uint32(0); time.Now().Before(deadline); seq++ {
		if _, stepErr = s.Step(context.Background(), testFrame(int(seq)), seq); stepErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wantCode(t, stepErr, duplex.ErrTenantUnavailable)
	var de *duplex.Error
	if errors.As(stepErr, &de) && !de.Fatal {
		t.Error("a lost lease must be fatal: the engine under the session is no longer protected")
	}
}

func TestLocator_TransientRenewFailuresAreSurvivedUntilTheLeaseExpires(t *testing.T) {
	// The daemon is briefly unreachable: while the lease is still good the session carries on.
	loc := &fakeLocator{lease: Lease{Expires: time.Now().Add(400 * time.Millisecond)}, renewErr: func(n int) error {
		if n <= 3 {
			return errors.New("daemon unreachable")
		}
		return nil
	}}
	_, tn := leased(t, loc, nil)
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(120 * time.Millisecond)
	if _, err := s.Step(context.Background(), testFrame(1), 0); err != nil {
		t.Fatalf("a transient renew failure ended the session: %v", err)
	}
}

func TestLocator_ARenewalThatNeverSucceedsEndsTheSessionOnceTheLeaseHasLapsed(t *testing.T) {
	loc := &fakeLocator{lease: Lease{Expires: time.Now().Add(150 * time.Millisecond)}, renewErr: func(int) error { return errors.New("daemon unreachable") }}
	_, tn := leased(t, loc, nil)
	s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(400 * time.Millisecond)
	_, stepErr := s.Step(context.Background(), testFrame(1), 0)
	wantCode(t, stepErr, duplex.ErrTenantUnavailable)
}

func TestLocator_AFailedStartGivesTheLeaseBack(t *testing.T) {
	loc := &fakeLocator{}
	_, tn := leased(t, loc, func(c *Config) { c.Tenant = "no-such-tenant" }) // the engine has no such tenant: Start fails after the lease was taken
	if _, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1}); err == nil {
		t.Fatal("Start succeeded against an engine with no such tenant")
	}
	if _, released, _ := loc.counts(); len(released) != 1 || released[0] != "lease-1" {
		t.Fatalf("released %v: a failed start must not pin the engine", released)
	}
}

func TestLocator_ALocateFailureIsPassedOnAndReleasesNothing(t *testing.T) {
	want := &duplex.Error{Code: duplex.ErrSessionLimit, Fatal: true, Message: "no room"}
	loc := &fakeLocator{locateErr: want}
	_, tn := leased(t, loc, nil)
	_, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: "s1", SampleRate: duplex.SampleRate, MaxMinutes: 1})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the locator's own error", err)
	}
	if _, released, _ := loc.counts(); len(released) != 0 {
		t.Fatalf("released %v with nothing leased", released)
	}
}

// --- the daemon locator -------------------------------------------------------------------------------------------------------------------

func fakeDaemon(t *testing.T, handler http.HandlerFunc) (*DaemonLocator, *[]string) {
	t.Helper()
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return &DaemonLocator{BaseURL: srv.URL, Key: "sk-forge-" + strings.Repeat("a", 24)}, &seen
}

func TestDaemonLocator_LocateParsesTheLeaseAndSendsTheKeyAsAHeader(t *testing.T) {
	exp := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	d, seen := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["engine"] != "haru" {
			t.Errorf("engine = %q", b["engine"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"engine": "haru", "slot": "s2", "base_url": "http://127.0.0.1:8101", "lease_id": "abc", "expires_at": exp.Format(time.RFC3339)})
	})
	l, err := d.Locate(context.Background(), "haru")
	if err != nil || l.BaseURL != "http://127.0.0.1:8101" || l.ID != "abc" || !l.Expires.Equal(exp) {
		t.Fatalf("lease %+v err %v", l, err)
	}
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "POST /api/v1/duplex/resolve auth=Bearer sk-forge-") {
		t.Fatalf("calls %v", *seen)
	}
}

func TestDaemonLocator_LocateMapsTheDaemonsCodes(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		code   duplex.ErrorCode
	}{
		"no such engine": {503, `{"code":"duplex_no_engine","error":"x"}`, duplex.ErrTenantUnavailable},
		"turned off":     {503, `{"code":"duplex_engine_off","error":"x"}`, duplex.ErrTenantUnavailable},
		"pool full":      {503, `{"code":"specialist_pool_full","error":"x"}`, duplex.ErrSessionLimit},
		"lease limit":    {503, `{"code":"duplex_lease_limit","error":"x"}`, duplex.ErrSessionLimit},
		"took too long":  {503, `{"code":"specialist_unavailable","error":"x"}`, duplex.ErrModelNotReady},
		"bad key":        {401, `{"error":"nope"}`, duplex.ErrAuth},
		"forbidden":      {403, `{"error":"nope"}`, duplex.ErrAuth},
		"something else": {500, `{"error":"boom"}`, duplex.ErrTenantUnavailable},
	}
	for name, tc := range cases {
		d, _ := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := d.Locate(context.Background(), "haru")
		var de *duplex.Error
		if !errors.As(err, &de) || de.Code != tc.code || !de.Fatal {
			t.Errorf("%s: %v, want fatal %s", name, err, tc.code)
		}
		if err != nil && strings.Contains(err.Error(), "sk-forge") {
			t.Errorf("%s: the key is in the error: %v", name, err)
		}
	}
}

func TestDaemonLocator_AnUnusableAnswerIsAnErrorAndAMissingExpiryGivesTheLeaseBack(t *testing.T) {
	d, seen := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/release") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"base_url": "http://127.0.0.1:8101", "lease_id": "abc"}) // no expires_at
	})
	if _, err := d.Locate(context.Background(), "haru"); err == nil {
		t.Fatal("a lease without an expiry has no renewal schedule: Locate must fail")
	}
	if len(*seen) != 2 || !strings.Contains((*seen)[1], "/api/v1/duplex/release") {
		t.Fatalf("calls %v: the lease taken without an expiry must be released", *seen)
	}
}

func TestDaemonLocator_RenewAndRelease(t *testing.T) {
	status := http.StatusNoContent
	d, _ := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	if err := d.Renew(context.Background(), "abc"); err != nil {
		t.Fatalf("renew 204: %v", err)
	}
	status = http.StatusGone
	if err := d.Renew(context.Background(), "abc"); !errors.Is(err, ErrLeaseGone) {
		t.Fatalf("renew 410: %v, want ErrLeaseGone", err)
	}
	status = http.StatusInternalServerError
	if err := d.Renew(context.Background(), "abc"); err == nil || errors.Is(err, ErrLeaseGone) {
		t.Fatalf("renew 500: %v, want a transient error, not ErrLeaseGone", err)
	}
	d.Release(context.Background(), "abc") // best effort, never panics or returns
}

func TestDaemonLocator_ACancelledLocateIsCancelledNotUnavailable(t *testing.T) {
	d, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a dropped client only once the body has been read
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := d.Locate(ctx, "haru")
	wantCode(t, err, duplex.ErrCancelled)
}

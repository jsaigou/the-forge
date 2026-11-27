// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
)

var _ DuplexPool = (*specialist.Manager)(nil) // the real pool must satisfy what the endpoints ask of it

// duplexFakePool is a pool that also resolves duplex engines, with scripted answers.
type duplexFakePool struct {
	*fakeSpecialistPool
	target   specialist.Target
	err      error
	asked    []string
	renewed  []string
	released []string
	live     map[string]bool
}

func (p *duplexFakePool) ResolveDuplex(_ context.Context, name string) (specialist.Target, error) {
	p.asked = append(p.asked, name)
	return p.target, p.err
}
func (p *duplexFakePool) RenewLease(id string) bool {
	p.renewed = append(p.renewed, id)
	return p.live[id]
}
func (p *duplexFakePool) ReleaseSpeech(id string) bool {
	p.released = append(p.released, id)
	return p.live[id]
}

func newDuplexServer(t *testing.T) (*Server, *duplexFakePool) {
	t.Helper()
	s, base := newSpecialistServer(t, authz.RoleOperator)
	p := &duplexFakePool{fakeSpecialistPool: base, live: map[string]bool{"known": true},
		target: specialist.Target{Occupant: "haru", Slot: "s2", BaseURL: "http://127.0.0.1:8101", Port: 8101, LeaseID: "known", Expires: time.Now().Add(10 * time.Minute)}}
	s.deps.Specialists = p
	return s, p
}

func TestDuplexResolve_AnswersWithTheEngineAndALease(t *testing.T) {
	s, p := newDuplexServer(t)
	code, out, _ := spCall(t, s, "POST", "/api/v1/duplex/resolve", `{"engine":"haru"}`)
	if code != 200 || out["engine"] != "haru" || out["base_url"] != "http://127.0.0.1:8101" || out["lease_id"] != "known" || out["slot"] != "s2" || out["expires_at"] == "" {
		t.Fatalf("resolve: %d %v", code, out)
	}
	if len(p.asked) != 1 || p.asked[0] != "haru" {
		t.Errorf("the pool was asked %v", p.asked)
	}
	// No engine named: the pool decides (the function's default).
	if code, _, _ := spCall(t, s, "POST", "/api/v1/duplex/resolve", `{}`); code != 200 || p.asked[1] != "" {
		t.Errorf("no engine named: %d, asked %v", code, p.asked)
	}
}

func TestDuplexResolve_Validation(t *testing.T) {
	s, _ := newDuplexServer(t)
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	for name, body := range map[string]string{"too long": `{"engine":"` + string(long) + `"}`, "not json": `{`} {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/duplex/resolve", body); code < 400 || code >= 500 {
			t.Errorf("%s: %d %v, want a 4xx", name, code, out)
		}
	}
}

// The split that matters to forge-duplex is hard (no engine, turned off: do not retry) versus transient (loading, pool full: retry shortly).
func TestDuplexResolve_ErrorsHaveStableCodes(t *testing.T) {
	cases := map[string]struct {
		err   error
		code  string
		retry bool
		http  int
	}{
		"no default":     {specialist.ErrNoMatch, "duplex_no_engine", false, 503},
		"no such engine": {specialist.ErrNotFound, "duplex_no_engine", false, 503},
		"disabled":       {&specialist.UnavailableError{Occupant: "haru", Reason: specialist.ReasonDisabled}, "duplex_engine_off", false, 503},
		"stopped":        {&specialist.UnavailableError{Occupant: "haru", Reason: specialist.ReasonStopped}, "duplex_engine_off", false, 503},
		"too many":       {specialist.ErrTooManyLeases, "duplex_lease_limit", true, 503},
		"pool full":      {&specialist.PoolFullError{Occupant: "haru", RetryAfter: 3 * time.Second}, "specialist_pool_full", true, 503},
		"still loading":  {&specialist.UnavailableError{Occupant: "haru", Reason: "it is still starting"}, "specialist_unavailable", false, 503},
		"timed out":      {context.DeadlineExceeded, "specialist_unavailable", true, 503},
	}
	for name, tc := range cases {
		s, p := newDuplexServer(t)
		p.err = tc.err
		code, out, hdr := spCall(t, s, "POST", "/api/v1/duplex/resolve", `{"engine":"haru"}`)
		if code != tc.http || out["code"] != tc.code {
			t.Errorf("%s: HTTP %d code %v, want %d %s", name, code, out["code"], tc.http, tc.code)
		}
		if tc.retry && hdr.Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", name)
		}
	}
}

func TestDuplexRenewAndRelease(t *testing.T) {
	s, p := newDuplexServer(t)
	if code, _, _ := spCall(t, s, "POST", "/api/v1/duplex/renew", `{"lease_id":"known"}`); code != 204 {
		t.Errorf("renew a live lease: %d, want 204", code)
	}
	// A lease that is gone must say so: the session has to end, because the engine under it is no longer protected.
	code, out, _ := spCall(t, s, "POST", "/api/v1/duplex/renew", `{"lease_id":"stale"}`)
	if code != 410 || out["code"] != "duplex_lease_gone" {
		t.Errorf("renew a gone lease: %d %v, want 410 duplex_lease_gone", code, out)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/duplex/renew", `{}`); code < 400 || code >= 500 {
		t.Errorf("renew without an id: %d, want a 4xx", code)
	}
	// Release is idempotent: an unknown lease is not an error.
	for _, id := range []string{"known", "stale"} {
		if code, _, _ := spCall(t, s, "POST", "/api/v1/duplex/release", `{"lease_id":"`+id+`"}`); code != 204 {
			t.Errorf("release %s: %d, want 204", id, code)
		}
	}
	if len(p.released) != 2 || len(p.renewed) != 2 {
		t.Errorf("renewed %v released %v", p.renewed, p.released)
	}
}

func TestDuplexEndpointsNeedThePoolToDoDuplex(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleOperator) // the plain fake pool has no ResolveDuplex
	if code, out, _ := spCall(t, s, "POST", "/api/v1/duplex/resolve", `{}`); code != 503 || out["code"] != "not_wired" {
		t.Errorf("resolve without the pool: %d %v, want 503 not_wired", code, out)
	}
}

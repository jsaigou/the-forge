// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
)

var _ SpeechPool = (*specialist.Manager)(nil) // the real pool must satisfy what the endpoints ask of it

// speechFakePool is a pool that also does speech resolution, with scripted answers.
type speechFakePool struct {
	*fakeSpecialistPool
	lease    specialist.SpeechLease
	err      error
	asked    []specialist.SpeechRequest
	released []string
}

func (p *speechFakePool) ResolveSpeech(_ context.Context, req specialist.SpeechRequest) (specialist.SpeechLease, error) {
	p.asked = append(p.asked, req)
	return p.lease, p.err
}
func (p *speechFakePool) ReleaseSpeech(id string) bool {
	p.released = append(p.released, id)
	return id == "known"
}

func newSpeechServer(t *testing.T, role authz.Role) (*Server, *speechFakePool) {
	t.Helper()
	s, base := newSpecialistServer(t, role)
	p := &speechFakePool{fakeSpecialistPool: base, lease: specialist.SpeechLease{ID: "lease1", Engine: "base-a", Slot: "s4", Mode: "base", Kind: specialist.KindZonos3Server,
		BaseURL: "http://127.0.0.1:8103", Port: 8103, Expires: time.Now().Add(10 * time.Minute), Loaded: true}}
	s.deps.Specialists = p
	return s, p
}

func TestSpeechResolve_AnswersWithTheEngineAndALease(t *testing.T) {
	s, p := newSpeechServer(t, authz.RoleOperator)
	code, out, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"base","engine":"base-a"}`)
	if code != 200 || out["engine"] != "base-a" || out["base_url"] != "http://127.0.0.1:8103" || out["lease_id"] != "lease1" || out["loaded"] != true || out["mode"] != "base" || out["kind"] != "zonos3-server" {
		t.Fatalf("resolve: %d %v (the answer must name the engine's kind: forge-tts picks its wire protocol from it)", code, out)
	}
	if len(p.asked) != 1 || p.asked[0].Mode != "base" || p.asked[0].Engine != "base-a" {
		t.Errorf("the pool was asked %+v", p.asked)
	}
	// An empty engine means the voice is bound to none: the kind default applies (the pool decides).
	if code, _, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"kokoro"}`); code != 200 || p.asked[1].Engine != "" {
		t.Errorf("no engine named: %d, asked %+v", code, p.asked)
	}
}

func TestSpeechResolve_Validation(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleOperator)
	for name, body := range map[string]string{"unknown kind": `{"mode":"opera"}`, "no kind": `{}`, "empty": ``, "not json": `{`} {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", body); code < 400 || code >= 500 {
			t.Errorf("%s: %d %v, want a 4xx", name, code, out)
		}
	}
}

// The split that matters to the caller: transient (try again shortly) versus hard (retrying changes nothing).
func TestSpeechResolve_ErrorsKeepStableCodes(t *testing.T) {
	s, p := newSpeechServer(t, authz.RoleOperator)
	for name, tc := range map[string]struct {
		err       error
		code      string
		transient any
		retry     bool
	}{
		"pool full":      {&specialist.PoolFullError{Occupant: "x", RetryAfter: 3 * time.Second}, "specialist_pool_full", nil, true},
		"loading":        {&specialist.UnavailableError{Occupant: "x", Reason: "failed to start: boom"}, "specialist_unavailable", nil, false},
		"disabled":       {&specialist.UnavailableError{Occupant: "base-a", Reason: specialist.ReasonDisabled}, "speech_engine_disabled", false, false},
		"no engine":      {&specialist.NoSpeechEngineError{Mode: "base", Reason: "no engine of that kind is registered"}, "speech_no_engine", false, false},
		"lease limit":    {specialist.ErrTooManyLeases, "speech_lease_limit", true, true},
		"stopped":        {&specialist.UnavailableError{Occupant: "base-a", Reason: specialist.ReasonStopped}, "speech_engine_stopped", false, false},
		"engine removed": {specialist.ErrNotFound, "speech_no_engine", false, false},
		"caller gave up": {context.Canceled, "specialist_unavailable", true, true},
		"too slow":       {context.DeadlineExceeded, "specialist_unavailable", true, true},
		"something else": {errors.New("disk on fire"), "internal", nil, false},
	} {
		p.err = tc.err
		code, out, hdr := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"base"}`)
		if out["code"] != tc.code {
			t.Errorf("%s: code = %v (HTTP %d), want %s", name, out["code"], code, tc.code)
		}
		if tc.code != "internal" && code != 503 {
			t.Errorf("%s: HTTP %d, want 503", name, code)
		}
		if tc.transient != nil {
			if params, _ := out["params"].(map[string]any); params["transient"] != tc.transient {
				t.Errorf("%s: transient = %v, want %v", name, params["transient"], tc.transient)
			}
		}
		if tc.retry && hdr.Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", name)
		}
	}
	// A disabled engine is named in the answer.
	p.err = &specialist.UnavailableError{Occupant: "base-a", Reason: specialist.ReasonDisabled}
	if _, out, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"base","engine":"base-a"}`); !strings.Contains(out["error"].(string), "base-a") {
		t.Errorf("the disabled engine is not named: %v", out)
	}
}

func TestSpeechRelease_IsIdempotent(t *testing.T) {
	s, p := newSpeechServer(t, authz.RoleOperator)
	for _, id := range []string{"known", "known", "never-existed"} {
		if code, _, _ := spCall(t, s, "POST", "/api/v1/speech/release", `{"lease_id":"`+id+`"}`); code != http.StatusNoContent {
			t.Errorf("release %s: %d, want 204 whether or not the lease existed", id, code)
		}
	}
	if len(p.released) != 3 {
		t.Errorf("released %v", p.released)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/speech/release", `{}`); code != 422 {
		t.Errorf("no lease_id: %d, want 422", code)
	}
}

func TestSpeechEndpoints_NeedAPoolThatDoesSpeech(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleOperator) // the plain fake pool has no ResolveSpeech
	code, out, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"base"}`)
	if code != 503 || out["code"] != "not_wired" {
		t.Errorf("no speech pool: %d %v, want 503 not_wired", code, out)
	}
}

func TestSpeechEndpoints_RolesAndDefaults(t *testing.T) {
	// A viewer can do neither call: resolving loads engines.
	v, _ := newSpeechServer(t, authz.RoleViewer)
	for _, c := range [][3]string{{"POST", "/api/v1/speech/resolve", `{"mode":"base"}`}, {"POST", "/api/v1/speech/release", `{"lease_id":"x"}`},
		{"GET", "/api/v1/speech/defaults", ""}, {"PUT", "/api/v1/speech/defaults", `{}`}} {
		if code, _, _ := spCall(t, v, c[0], c[1], c[2]); code != 403 {
			t.Errorf("a viewer got %d on %s %s, want 403", code, c[0], c[1])
		}
	}
	// An operator may read the defaults but not change them.
	o, _ := newSpeechServer(t, authz.RoleOperator)
	if code, _, _ := spCall(t, o, "GET", "/api/v1/speech/defaults", ""); code != 200 {
		t.Errorf("operator read: %d", code)
	}
	if code, _, _ := spCall(t, o, "PUT", "/api/v1/speech/defaults", `{}`); code != 403 {
		t.Errorf("operator write: %d, want 403", code)
	}
}

func TestSpeechDefaults_ValidatedAndStored(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleAdmin)
	mk := func(name, alias string) {
		body := `{"name":"` + name + `","kind":"qwen-tts-server","functions":["speech"],"policy":"on_demand","enabled":true,
		 "params":{"model":"/opt/forge/models/a.gguf","codec":"/opt/forge/models/c.gguf","alias":"` + alias + `"}}`
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", body); code != 201 {
			t.Fatalf("setup %s: %d %v", name, code, out)
		}
	}
	mk("base-a", "base")
	mk("design-a", "voicedesign")
	// The read lists, per kind, the engines that could be chosen.
	code, out, _ := spCall(t, s, "GET", "/api/v1/speech/defaults", "")
	engines, _ := out["engines"].(map[string]any)
	if code != 200 || len(engines["base"].([]any)) != 1 || len(engines["voicedesign"].([]any)) != 1 || len(engines["customvoice"].([]any)) != 0 {
		t.Fatalf("defaults read: %d %v", code, out)
	}
	// engine_info names each engine with its kind, label and state, for a caller that offers a choice.
	info, _ := out["engine_info"].([]any)
	if len(info) != 2 {
		t.Fatalf("engine_info = %v, want 2 entries", out["engine_info"])
	}
	first, _ := info[0].(map[string]any)
	if first["name"] != "base-a" || first["mode"] != "base" || first["enabled"] != true || first["state"] == "" {
		t.Errorf("engine_info[0] = %v", first)
	}
	bad := map[string]string{
		"engine of another kind": `{"base":"design-a"}`,
		"unknown engine":         `{"base":"ghost"}`,
		"unknown kind":           `{"opera":"base-a"}`,
	}
	for name, body := range bad {
		if code, out, _ := spCall(t, s, "PUT", "/api/v1/speech/defaults", body); code != 422 {
			t.Errorf("%s: %d %v, want 422", name, code, out)
		}
	}
	if code, out, _ := spCall(t, s, "PUT", "/api/v1/speech/defaults", `{"base":"base-a","voicedesign":"design-a","customvoice":""}`); code != 200 {
		t.Fatalf("valid defaults: %d %v", code, out)
	}
	_, got, _ := spCall(t, s, "GET", "/api/v1/speech/defaults", "")
	d, _ := got["defaults"].(map[string]any)
	if d["base"] != "base-a" || d["voicedesign"] != "design-a" || len(d) != 2 {
		t.Errorf("stored defaults = %v, want base and voicedesign only (an empty value clears a kind)", d)
	}
}

// End to end through the real auth: a speech-scoped key reaches the two speech calls and nothing else, and only from the host it is bound to.
func TestSpeechScopedKey_ReachesOnlyTheTwoCallsFromLoopback(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	key, err := auth.MintScopedKey(t.Context(), "forge-tts", "", authz.RoleOperator, "127.0.0.1", time.Now().Add(24*time.Hour), authz.ScopeSpeech)
	if err != nil {
		t.Fatal(err)
	}
	call := func(remote, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// From the host: the two speech calls pass the scope (this minimal server has no pool, so they stop at not_wired, which is past the scope check).
	for _, c := range [][2]string{{"POST", "/api/v1/speech/resolve"}, {"POST", "/api/v1/speech/release"}, {"GET", "/api/v1/speech/defaults"}} {
		if code, out := call("127.0.0.1:5555", c[0], c[1], `{"mode":"base","lease_id":"x"}`); code == 403 || out["code"] == "scope_denied" {
			t.Errorf("speech key on %s %s: %d %v, want past the scope", c[0], c[1], code, out)
		}
	}
	// Everything else is refused before any handler runs, whatever the role.
	for _, c := range [][3]string{{"GET", "/api/v1/status", ""}, {"GET", "/api/v1/specialists", ""}, {"POST", "/api/v1/specialists/load", `{"occupant":"x"}`},
		{"PUT", "/api/v1/speech/defaults", `{}`}, {"GET", "/api/v1/keys", ""}} {
		if code, out := call("127.0.0.1:5555", c[0], c[1], c[2]); code != http.StatusForbidden || out["code"] != "scope_denied" {
			t.Errorf("speech key %s %s: %d %v, want 403 scope_denied", c[0], c[1], code, out)
		}
	}
	// From anywhere else the key does not authenticate at all (bound to loopback).
	if code, _ := call("100.64.0.9:5555", "POST", "/api/v1/speech/resolve", `{"mode":"base"}`); code != http.StatusUnauthorized {
		t.Errorf("speech key from a tailnet address: %d, want 401", code)
	}
}

func TestSpeechResolve_EngineNameIsBounded(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleOperator)
	long := strings.Repeat("x", 129)
	if code, _, _ := spCall(t, s, "POST", "/api/v1/speech/resolve", `{"mode":"base","engine":"`+long+`"}`); code != 422 {
		t.Errorf("a 129 character engine name: %d, want 422", code)
	}
}

// Only a hard failure is worth an audit row; a retry loop on a transient one must not write a row per attempt.
func TestIsHardSpeechError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		hard bool
	}{
		"no engine":  {&specialist.NoSpeechEngineError{Mode: "base"}, true},
		"removed":    {specialist.ErrNotFound, true},
		"disabled":   {&specialist.UnavailableError{Reason: specialist.ReasonDisabled}, true},
		"stopped":    {&specialist.UnavailableError{Reason: specialist.ReasonStopped}, true},
		"loading":    {&specialist.UnavailableError{Reason: "failed to start: x"}, false},
		"pool full":  {&specialist.PoolFullError{}, false},
		"cancelled":  {context.Canceled, false},
		"lease full": {specialist.ErrTooManyLeases, false},
	} {
		if got := isHardSpeechError(tc.err); got != tc.hard {
			t.Errorf("%s: isHardSpeechError = %v, want %v", name, got, tc.hard)
		}
	}
}

// An empty body or null replaces every default with nothing: that has to be said explicitly with {}.
func TestSpeechDefaults_AnEmptyBodyDoesNotClearEverything(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", `{"name":"base-a","kind":"qwen-tts-server","functions":["speech"],"policy":"on_demand","enabled":true,
	 "params":{"model":"/opt/forge/models/a.gguf","codec":"/opt/forge/models/c.gguf","alias":"base"}}`); code != 201 {
		t.Fatalf("setup: %d %v", code, out)
	}
	if code, _, _ := spCall(t, s, "PUT", "/api/v1/speech/defaults", `{"base":"base-a"}`); code != 200 {
		t.Fatalf("set: %d", code)
	}
	for name, body := range map[string]string{"empty": ``, "null": `null`} {
		if code, out, _ := spCall(t, s, "PUT", "/api/v1/speech/defaults", body); code != 422 {
			t.Errorf("%s body: %d %v, want 422", name, code, out)
		}
	}
	if _, got, _ := spCall(t, s, "GET", "/api/v1/speech/defaults", ""); got["defaults"].(map[string]any)["base"] != "base-a" {
		t.Errorf("a refused empty body still changed the defaults: %v", got)
	}
	if code, _, _ := spCall(t, s, "PUT", "/api/v1/speech/defaults", `{}`); code != 200 {
		t.Errorf("an explicit {}: %d, want 200", code)
	}
	if _, got, _ := spCall(t, s, "GET", "/api/v1/speech/defaults", ""); len(got["defaults"].(map[string]any)) != 0 {
		t.Errorf("an explicit {} did not clear: %v", got)
	}
}

// A caller that gives up must not cancel the engine start it was waiting for, and must not be left holding a lease nobody will release.
type ctxRecordingPool struct {
	*speechFakePool
	ctxErrDuringResolve error
}

func (p *ctxRecordingPool) ResolveSpeech(ctx context.Context, req specialist.SpeechRequest) (specialist.SpeechLease, error) {
	p.ctxErrDuringResolve = ctx.Err()
	return p.speechFakePool.ResolveSpeech(ctx, req)
}

func TestSpeechResolve_ACallerThatGaveUpDoesNotCancelTheStartOrKeepTheLease(t *testing.T) {
	s, base := newSpeechServer(t, authz.RoleOperator)
	p := &ctxRecordingPool{speechFakePool: base}
	s.deps.Specialists = p

	gone, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already gone
	req := httptest.NewRequest("POST", "/api/v1/speech/resolve", bytes.NewBufferString(`{"mode":"base"}`)).WithContext(gone)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	req.RemoteAddr = "100.64.0.1:1234"
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if len(base.asked) != 1 {
		t.Fatalf("the pool was asked %d times, want 1", len(base.asked))
	}
	if p.ctxErrDuringResolve != nil {
		t.Errorf("the resolve ran on the caller's context (%v): a caller that gives up would cancel an engine start", p.ctxErrDuringResolve)
	}
	if len(base.released) != 1 || base.released[0] != "lease1" {
		t.Errorf("the lease of a caller that had gone was not given back: released=%v", base.released)
	}
}

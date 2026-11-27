// SPDX-License-Identifier: Apache-2.0

package httpapi

// Matrix tests for the specialist pool HTTP API: role/auth matrix, input validation, the 202 "stopping" paths and concurrency. They reuse
// newSpecialistServer/spCall/asrBody from specialist_handlers_test.go but swap in a fully locked pool fake (matrixPool) so concurrent tests are
// race-free.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// matrixPool is a SpecialistPool whose every field is mutex-guarded. Unload blocks on gate (when set) until it is closed.
type matrixPool struct {
	mu       sync.Mutex
	slots    []specialist.SlotState
	loaded   []string
	unloaded []string
	gate     chan struct{}
	once     sync.Once
}

func (p *matrixPool) release() {
	p.once.Do(func() {
		if p.gate != nil {
			close(p.gate)
		}
	})
}

func (p *matrixPool) setSlots(s []specialist.SlotState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slots = s
}

func (p *matrixPool) Snapshot(context.Context) []specialist.SlotState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]specialist.SlotState(nil), p.slots...)
}

func (p *matrixPool) Load(_ context.Context, name, slot string) (specialist.SlotState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loaded = append(p.loaded, name+"@"+slot)
	return specialist.SlotState{Slot: "s1", Occupant: name, State: specialist.StateRunning}, nil
}

func (p *matrixPool) Unload(_ context.Context, slot string) error {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unloaded = append(p.unloaded, slot)
	return nil
}

func (p *matrixPool) Forget(string)                             {}
func (p *matrixPool) PooledConfig(context.Context, string) bool { return false }

func (p *matrixPool) loadedList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.loaded...)
}

func (p *matrixPool) unloadedList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.unloaded...)
}

// matrixServer is newSpecialistServer with the locked pool and a slot-log source wired in.
func matrixServer(t *testing.T, role authz.Role) (*Server, *matrixPool) {
	t.Helper()
	s, _ := newSpecialistServer(t, role)
	p := &matrixPool{slots: []specialist.SlotState{
		{Slot: "s1", Port: 8100, State: specialist.StateEmpty},
		{Slot: "s2", Port: 8101, State: specialist.StateEmpty},
	}}
	t.Cleanup(p.release) // never leave a background stop blocked
	s.deps.Specialists = p
	s.deps.SlotLog = func(_ context.Context, unit string, _ int) string { return "ok " + unit }
	return s, p
}

// rawCall sends a request with an arbitrary Authorization header ("" = none).
func rawCall(s *Server, method, path, body, authHeader string) (int, string) {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type matrixEndpoint struct {
	name         string
	method, path string // path may contain {NEW}, replaced by a freshly added slot name
	body         string
	adminWant    int
	gated        bool // behind requireAssurance(page.settings)
	// setup prepares an admin server; it returns the name substituted for {NEW}.
	setup func(t *testing.T, s *Server) string
}

func createASR(t *testing.T, s *Server) string {
	t.Helper()
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("setup create asr: %d %v", code, out)
	}
	return ""
}

// createAlwaysOnASR is createASR followed by making the member always-on: only an always-on member may be a function's default.
func createAlwaysOnASR(t *testing.T, s *Server) string {
	t.Helper()
	createASR(t, s)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/policy", `{"policy":"always_on","home_slot":"s1"}`); code != 200 {
		t.Fatalf("setup make asr always-on: %d %v", code, out)
	}
	return ""
}

func addSlot(t *testing.T, s *Server) string {
	t.Helper()
	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", "")
	if code != 201 {
		t.Fatalf("setup add slot: %d %v", code, out)
	}
	return out["slot"].(string)
}

func matrixEndpoints() []matrixEndpoint {
	return []matrixEndpoint{
		{name: "list", method: "GET", path: "/api/v1/specialists", adminWant: 200},
		{name: "load", method: "POST", path: "/api/v1/specialists/load", body: `{"occupant":"asr"}`, adminWant: 200},
		{name: "unload", method: "POST", path: "/api/v1/specialists/unload", body: `{"slot":"s1"}`, adminWant: 200},
		{name: "occupant create", method: "POST", path: "/api/v1/specialists/occupants", body: asrBody, adminWant: 201, gated: true},
		{name: "occupant update", method: "PUT", path: "/api/v1/specialists/occupants/asr", body: asrBody, adminWant: 200, gated: true, setup: createASR},
		{name: "occupant delete", method: "DELETE", path: "/api/v1/specialists/occupants/asr", adminWant: 204, gated: true, setup: createASR},
		{name: "policy", method: "POST", path: "/api/v1/specialists/occupants/asr/policy", body: `{"policy":"on_demand"}`, adminWant: 200, gated: true, setup: createASR},
		{name: "behavior", method: "POST", path: "/api/v1/specialists/occupants/asr/behavior", body: `{"behavior":"on_demand"}`, adminWant: 200, gated: true, setup: createASR},
		{name: "slot add", method: "POST", path: "/api/v1/specialists/slots", adminWant: 201, gated: true},
		{name: "slot remove", method: "DELETE", path: "/api/v1/specialists/slots/{NEW}", adminWant: 204, gated: true,
			setup: func(t *testing.T, s *Server) string { return addSlot(t, s) }},
		{name: "slot log", method: "GET", path: "/api/v1/specialists/slots/s1/log", adminWant: 200},
		{name: "default", method: "PUT", path: "/api/v1/specialists/defaults/transcription", body: `{"occupant":"asr"}`, adminWant: 200, gated: true, setup: createAlwaysOnASR},
	}
}

func (e matrixEndpoint) resolve(t *testing.T, s *Server) string {
	t.Helper()
	repl := ""
	if e.setup != nil {
		repl = e.setup(t, s)
	}
	return strings.ReplaceAll(e.path, "{NEW}", repl)
}

func TestMatrix_RoleMatrix(t *testing.T) {
	for _, e := range matrixEndpoints() {
		t.Run(e.name+"/admin", func(t *testing.T) {
			s, _ := matrixServer(t, authz.RoleAdmin)
			path := e.resolve(t, s)
			if code, out, _ := spCall(t, s, e.method, path, e.body); code != e.adminWant {
				t.Errorf("admin %s %s = %d %v, want %d", e.method, path, code, out, e.adminWant)
			}
		})
		t.Run(e.name+"/viewer", func(t *testing.T) {
			s, _ := matrixServer(t, authz.RoleViewer)
			path := strings.ReplaceAll(e.path, "{NEW}", "s9")
			want := 403
			if e.name == "list" {
				want = 200
			}
			if code, out, _ := spCall(t, s, e.method, path, e.body); code != want {
				t.Errorf("viewer %s %s = %d %v, want %d", e.method, path, code, out, want)
			}
		})
		t.Run(e.name+"/no_token", func(t *testing.T) {
			s, _ := matrixServer(t, authz.RoleAdmin)
			path := strings.ReplaceAll(e.path, "{NEW}", "s9")
			if code, _ := rawCall(s, e.method, path, e.body, ""); code != 401 {
				t.Errorf("no token: %s %s = %d, want 401", e.method, path, code)
			}
		})
		t.Run(e.name+"/garbage_token", func(t *testing.T) {
			s, _ := matrixServer(t, authz.RoleAdmin)
			path := strings.ReplaceAll(e.path, "{NEW}", "s9")
			for _, h := range []string{"Bearer not-a-token", "Bearer sk-forge-", "Basic abc"} {
				if code, _ := rawCall(s, e.method, path, e.body, h); code != 401 {
					t.Errorf("auth %q: %s %s = %d, want 401", h, e.method, path, code)
				}
			}
		})
	}
}

// The settings step-up: a session-style identity at network assurance is refused with 403 step_up_required on the endpoints that change what the
// pool is made of, and passes after a recent password step-up. (Bearer API keys bypass the policy by design; the stub identity has no KeyID, so it
// is evaluated like a session.) Load, unload and the log need admin only.
func TestMatrix_SettingsStepUp(t *testing.T) {
	policyServer := func(t *testing.T, assurance authz.Assurance, at time.Time) *Server {
		s, _ := matrixServer(t, authz.RoleAdmin)
		s.deps.PolicyStore = authz.NewPolicyStore(authz.SettingsAdapter{
			GetFn: func(ctx context.Context, key string) ([]byte, error) { return s.deps.Settings.Get(ctx, key) },
			SetFn: func(ctx context.Context, key string, value []byte) error { return s.deps.Settings.Set(ctx, key, value) },
		})
		s.deps.Auth = &stubAuth{identity: authz.Identity{Name: "t", Role: authz.RoleAdmin, Assurance: assurance, AssuranceAt: at}}
		return s
	}
	for _, e := range matrixEndpoints() {
		t.Run(e.name+"/network_assurance", func(t *testing.T) {
			s := policyServer(t, authz.AssuranceNetwork, time.Time{})
			path := strings.ReplaceAll(e.path, "{NEW}", "s9")
			code, body := rawCall(s, e.method, path, e.body, "Bearer sk-forge-a6a0da5609b8-testsecret123456")
			if e.gated {
				if code != 403 || !strings.Contains(body, "step_up_required") {
					t.Errorf("%s %s = %d %s, want 403 step_up_required", e.method, path, code, body)
				}
			} else if code == 403 && strings.Contains(body, "step_up_required") {
				t.Errorf("%s %s must not need the settings step-up: %d %s", e.method, path, code, body)
			}
		})
	}
	t.Run("password_step_up_passes", func(t *testing.T) {
		s := policyServer(t, authz.AssurancePassword, time.Now())
		code, body := rawCall(s, "POST", "/api/v1/specialists/occupants", asrBody, "Bearer sk-forge-a6a0da5609b8-testsecret123456")
		if code != 201 {
			t.Errorf("after step-up: %d %s", code, body)
		}
	})
}

func TestMatrix_BehaviorValidation(t *testing.T) {
	s, _ := matrixServer(t, authz.RoleAdmin)
	createASR(t, s)
	const base = "/api/v1/specialists/occupants/asr/behavior"
	for name, body := range map[string]string{
		"missing body":     "",
		"empty object":     `{}`,
		"malformed json":   `{"behavior":`,
		"not json":         `behavior=disabled`,
		"empty string":     `{"behavior":""}`,
		"unknown value":    `{"behavior":"sometimes"}`,
		"wrong case":       `{"behavior":"ALWAYS_ON"}`,
		"wrong type":       `{"behavior":5}`,
		"wrong type array": `{"behavior":["always_on"]}`,
		"null":             `{"behavior":null}`,
		"unknown field":    `{"behavior":"on_demand","extra":1}`,
		"top-level array":  `["on_demand"]`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out, _ := spCall(t, s, "POST", base, body)
			if code != 422 {
				t.Errorf("%s: %d %v, want 422", name, code, out)
			}
			if code >= 500 {
				t.Errorf("%s: server error", name)
			}
		})
	}
	// None of the rejected bodies changed the occupant.
	_, got, _ := spCall(t, s, "GET", "/api/v1/specialists", "")
	o := got["occupants"].([]any)[0].(map[string]any)
	if o["policy"] != "on_demand" || o["enabled"] != true {
		t.Errorf("a rejected body changed the occupant: %v", o)
	}
}

func TestMatrix_PathValues(t *testing.T) {
	s, _ := matrixServer(t, authz.RoleAdmin)
	createASR(t, s)
	names := map[string]string{
		"very long":     strings.Repeat("a", 5000),
		"encoded slash": "a%2Fb",
		"encoded dots":  "..%2F..%2Fetc%2Fpasswd",
		"space":         "a%20b",
		"nul":           "a%00b",
		"semicolon":     "a;b",
		"question":      "a%3Fb",
		"percent":       "a%25b",
		"quote":         "%27%3B%20DROP%20TABLE%20specialist_occupants%3B--",
		"unicode":       "%E3%83%86%E3%82%B9%E3%83%88",
	}
	for label, n := range names {
		for _, ep := range []struct{ method, suffix, body string }{
			{"POST", "/behavior", `{"behavior":"on_demand"}`},
			{"POST", "/policy", `{"policy":"on_demand"}`},
			{"PUT", "", asrBody},
			{"DELETE", "", ""},
		} {
			t.Run(label+"/"+ep.method+ep.suffix, func(t *testing.T) {
				code, out, _ := spCall(t, s, ep.method, "/api/v1/specialists/occupants/"+n+ep.suffix, ep.body)
				if code != 404 {
					t.Errorf("unknown occupant %q: %d %v, want 404", label, code, out)
				}
			})
		}
	}
	t.Run("unknown occupant behavior", func(t *testing.T) {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/ghost/behavior", `{"behavior":"disabled"}`); code != 404 || out["code"] != "not_found" {
			t.Errorf("%d %v", code, out)
		}
	})
	t.Run("default for an unknown function or occupant", func(t *testing.T) {
		if code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/defaults/nonsense", `{"occupant":"asr"}`); code < 400 || code >= 500 {
			t.Errorf("unknown function: %d %v", code, out)
		}
		if code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/defaults/transcription", `{"occupant":"ghost"}`); code < 400 || code >= 500 {
			t.Errorf("unknown occupant: %d %v", code, out)
		}
	})
}

func specialistSlotNames(t *testing.T, s *Server) (names map[string]int) {
	t.Helper()
	all, err := s.deps.Catalog.ListSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names = map[string]int{}
	for _, sl := range all {
		if sl.Pool == store.PoolSpecialist {
			names[sl.Name] = sl.Port
		}
	}
	return names
}

func TestMatrix_SlotAddCapAndRemove(t *testing.T) {
	s, pool := matrixServer(t, authz.RoleAdmin)

	for i := 0; i < 40; i++ {
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", "")
		if code == 201 {
			continue
		}
		if code != 409 || out["code"] != "specialist_slot_limit" {
			t.Fatalf("add #%d: %d %v, want 201 or 409 specialist_slot_limit", i, code, out)
		}
		break
	}
	names := specialistSlotNames(t, s)
	if len(names) != 12 {
		t.Fatalf("after hitting the cap there are %d specialist slots, want 12: %v", len(names), names)
	}
	for i := 1; i <= 12; i++ {
		if port, ok := names[fmt.Sprintf("s%d", i)]; !ok || port != 8099+i {
			t.Errorf("slot s%d port = %d (present=%v), want %d", i, port, ok, 8099+i)
		}
	}
	// The 13th (and any after) is a clean 409.
	for i := 0; i < 2; i++ {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", ""); code != 409 || out["code"] != "specialist_slot_limit" {
			t.Errorf("13th slot: %d %v", code, out)
		}
	}

	t.Run("llm slot", func(t *testing.T) {
		if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/a1", ""); code != 404 {
			t.Errorf("remove LLM slot: %d", code)
		}
	})
	t.Run("missing slot", func(t *testing.T) {
		for _, n := range []string{"s99", "nope", "S1", strings.Repeat("x", 3000), "s1%2F"} {
			if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/"+n, ""); code != 404 {
				t.Errorf("remove %q: %d, want 404", n, code)
			}
		}
	})
	t.Run("occupied slot", func(t *testing.T) {
		pool.setSlots([]specialist.SlotState{{Slot: "s12", Occupant: "asr", State: specialist.StateRunning}})
		if code, out, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/s12", ""); code != 409 || out["code"] != "has_dependents" {
			t.Errorf("occupied: %d %v", code, out)
		}
		pool.setSlots(nil)
	})
	t.Run("home slot of an always-on member", func(t *testing.T) {
		if _, err := s.deps.SpecialistWriter.CreateOccupant(context.Background(), store.Occupant{Name: "homey", Kind: "qwen3-asr-server",
			Functions: []string{store.FnTranscription}, Policy: store.PolicyAlwaysOn, HomeSlot: "s11", Enabled: true,
			Params: map[string]string{"model": "/opt/forge/models/a.gguf"}}); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/s11", ""); code != 409 || out["code"] != "has_dependents" {
			t.Errorf("home slot: %d %v", code, out)
		}
		if _, ok := specialistSlotNames(t, s)["s11"]; !ok {
			t.Error("a refused removal still deleted the slot")
		}
	})
	t.Run("empty slot then re-add reuses the freed name", func(t *testing.T) {
		if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/s10", ""); code != 204 {
			t.Fatalf("remove s10: %d", code)
		}
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", "")
		if code != 201 || out["slot"] != "s10" || int(out["port"].(float64)) != 8109 {
			t.Errorf("re-add: %d %v, want s10/8109", code, out)
		}
	})
}

func TestMatrix_LoadUnloadFields(t *testing.T) {
	s, pool := matrixServer(t, authz.RoleAdmin)
	for name, c := range map[string][2]string{
		"load missing body":      {"/api/v1/specialists/load", ""},
		"load empty object":      {"/api/v1/specialists/load", `{}`},
		"load empty occupant":    {"/api/v1/specialists/load", `{"occupant":""}`},
		"load unknown field":     {"/api/v1/specialists/load", `{"occupant":"a","bogus":1}`},
		"load wrong type":        {"/api/v1/specialists/load", `{"occupant":7}`},
		"load malformed":         {"/api/v1/specialists/load", `{"occupant"`},
		"unload missing body":    {"/api/v1/specialists/unload", ""},
		"unload empty object":    {"/api/v1/specialists/unload", `{}`},
		"unload empty slot":      {"/api/v1/specialists/unload", `{"slot":""}`},
		"unload unknown field":   {"/api/v1/specialists/unload", `{"slot":"s1","bogus":1}`},
		"unload wrong type":      {"/api/v1/specialists/unload", `{"slot":["s1"]}`},
		"unload malformed":       {"/api/v1/specialists/unload", `[`},
		"unload null":            {"/api/v1/specialists/unload", `null`},
		"load null (zero value)": {"/api/v1/specialists/load", `null`},
	} {
		t.Run(name, func(t *testing.T) {
			if code, out, _ := spCall(t, s, "POST", c[0], c[1]); code != 422 {
				t.Errorf("%d %v, want 422", code, out)
			}
		})
	}
	if l, u := pool.loadedList(), pool.unloadedList(); len(l) != 0 || len(u) != 0 {
		t.Errorf("a rejected request reached the pool: loaded=%v unloaded=%v", l, u)
	}
}

func TestMatrix_SlotLogLines(t *testing.T) {
	s, _ := matrixServer(t, authz.RoleAdmin)
	var mu sync.Mutex
	var asked []int
	s.deps.SlotLog = func(_ context.Context, _ string, n int) string {
		mu.Lock()
		asked = append(asked, n)
		mu.Unlock()
		// A pathological source: far more than any cap would allow.
		return strings.Repeat("a log line that is fairly long and never ends\n", 5000)
	}
	for _, q := range []string{"", "?lines=0", "?lines=-5", "?lines=999999", "?lines=abc", "?lines=", "?lines=1.5", "?lines=201", "?lines=99999999999999999999", "?lines=7", "?lines=200"} {
		t.Run("lines"+q, func(t *testing.T) {
			mu.Lock()
			asked = nil
			mu.Unlock()
			code, out, _ := spCall(t, s, "GET", "/api/v1/specialists/slots/s1/log"+q, "")
			if code != 200 {
				t.Fatalf("%d %v", code, out)
			}
			mu.Lock()
			got := append([]int(nil), asked...)
			mu.Unlock()
			if len(got) != 1 || got[0] < 1 || got[0] > 200 {
				t.Errorf("source asked for %v lines, want one request within 1..200", got)
			}
			if q == "" || q == "?lines=0" || q == "?lines=-5" || q == "?lines=abc" {
				if got[0] != 40 {
					t.Errorf("default = %d, want 40", got[0])
				}
			}
			if q == "?lines=7" && got[0] != 7 {
				t.Errorf("explicit 7 = %d", got[0])
			}
			if q == "?lines=200" && got[0] != 200 {
				t.Errorf("explicit 200 = %d", got[0])
			}
			text, _ := out["lines"].(string)
			if n := len([]rune(text)); n > 8001 {
				t.Errorf("returned %d runes, the cap is 8000", n)
			}
		})
	}
}

func TestMatrix_BehaviorAlwaysOnNoFreeSlot(t *testing.T) {
	t.Run("every slot occupied", func(t *testing.T) {
		s, pool := matrixServer(t, authz.RoleAdmin)
		createASR(t, s)
		pool.setSlots([]specialist.SlotState{
			{Slot: "s1", Occupant: "x", State: specialist.StateRunning},
			{Slot: "s2", Occupant: "y", State: specialist.StateStarting},
		})
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`); code != 200 {
			t.Fatalf("disable: %d %v", code, out)
		}
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"always_on"}`)
		if code != 409 || out["code"] != "specialist_no_free_slot" {
			t.Errorf("%d %v, want 409 specialist_no_free_slot", code, out)
		}
		if l := pool.loadedList(); len(l) != 0 {
			t.Errorf("a refused enable loaded something: %v", l)
		}
	})
	t.Run("other members stopping in every slot", func(t *testing.T) {
		s, pool := matrixServer(t, authz.RoleAdmin)
		createASR(t, s)
		pool.setSlots([]specialist.SlotState{
			{Slot: "s1", Occupant: "x", State: specialist.StateStopping},
			{Slot: "s2", Occupant: "y", State: specialist.StateStopping},
		})
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"always_on"}`)
		if code != 409 || out["code"] != "specialist_no_free_slot" {
			t.Errorf("%d %v, want 409 specialist_no_free_slot", code, out)
		}
	})
	t.Run("own slot stopping keeps its slot", func(t *testing.T) {
		s, pool := matrixServer(t, authz.RoleAdmin)
		createASR(t, s)
		pool.setSlots([]specialist.SlotState{
			{Slot: "s1", Occupant: "x", State: specialist.StateRunning},
			{Slot: "s2", Occupant: "asr", State: specialist.StateStopping},
		})
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"always_on"}`)
		if code >= 500 {
			t.Fatalf("server error: %d %v", code, out)
		}
		if code == 200 && out["home_slot"] != "s2" {
			t.Errorf("home = %v, want its own slot s2", out["home_slot"])
		}
	})
}

// A slow stop: the gate holds Unload, specialistUnloadWait is a few ms, so the answer is 202 deterministically; releasing the gate lets the
// detached stop finish.
func TestMatrix_SlowStopAnswers202(t *testing.T) {
	old := specialistUnloadWait
	specialistUnloadWait = 5 * time.Millisecond
	t.Cleanup(func() { specialistUnloadWait = old })

	t.Run("unload", func(t *testing.T) {
		s, pool := matrixServer(t, authz.RoleAdmin)
		pool.gate = make(chan struct{})
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/unload", `{"slot":"s1"}`)
		if code != http.StatusAccepted || out["state"] != "stopping" || out["slot"] != "s1" {
			t.Fatalf("%d %v, want 202 stopping", code, out)
		}
		if u := pool.unloadedList(); len(u) != 0 {
			t.Fatalf("the stop finished while still gated: %v", u)
		}
		pool.release()
		waitPoolCond(t, func() bool { return len(pool.unloadedList()) == 1 })
	})

	t.Run("behavior disabled", func(t *testing.T) {
		s, pool := matrixServer(t, authz.RoleAdmin)
		createASR(t, s)
		ctx := context.Background()
		if err := s.deps.SpecialistWriter.SetStopped(ctx, "asr", true); err != nil {
			t.Fatal(err)
		}
		pool.setSlots([]specialist.SlotState{{Slot: "s1", Occupant: "asr", State: specialist.StateRunning}, {Slot: "s2", State: specialist.StateEmpty}})
		pool.gate = make(chan struct{})
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`)
		if code != http.StatusAccepted || out["state"] != "stopping" || out["slot"] != "s1" || out["enabled"] != false {
			t.Fatalf("%d %v, want 202 stopping, enabled=false", code, out)
		}
		if o, _ := s.deps.SpecialistStore.OccupantByName(ctx, "asr"); !o.Stopped {
			t.Error("stop flag cleared before the stop finished")
		}
		pool.release()
		waitPoolCond(t, func() bool {
			o, err := s.deps.SpecialistStore.OccupantByName(ctx, "asr")
			return err == nil && !o.Stopped && len(pool.unloadedList()) == 1
		})
	})

	t.Run("fast stop is 200", func(t *testing.T) {
		s, _ := matrixServer(t, authz.RoleAdmin)
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/unload", `{"slot":"s1"}`)
		if code != 200 || out["state"] != "unloaded" {
			t.Errorf("%d %v", code, out)
		}
	})
}

// 20 concurrent adds: names/ports must be unique and inside s1..s12 / 8100..8111; a loser may only get a clean 409, never a 500.
func TestMatrix_ConcurrentSlotAddIsUnique(t *testing.T) {
	// Regression (was a bug, fixed with slotAddMu and a read-back): concurrent POST /slots read the same free slot, and store CreateSlot is INSERT OR IGNORE returning the existing id, so
	// several callers get 201 for the SAME slot name instead of one 201 and clean 409s.
	s, _ := matrixServer(t, authz.RoleAdmin)
	before := specialistSlotNames(t, s)

	const n = 20
	type res struct {
		code int
		out  map[string]any
	}
	results := make([]res, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/v1/specialists/slots", nil)
			req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
			req.RemoteAddr = "100.64.0.1:1234"
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			results[i] = res{rec.Code, out}
		}(i)
	}
	close(start)
	wg.Wait()

	seenName := map[string]int{}
	seenPort := map[int]int{}
	created := 0
	for i, r := range results {
		switch r.code {
		case 201:
			created++
			name := r.out["slot"].(string)
			port := int(r.out["port"].(float64))
			var num int
			if _, err := fmt.Sscanf(name, "s%d", &num); err != nil || num < 1 || num > 12 || port != 8099+num {
				t.Errorf("request %d: slot %q port %d out of range", i, name, port)
			}
			seenName[name]++
			seenPort[port]++
		case 409:
			if r.out["code"] != "operation_failed" && r.out["code"] != "specialist_slot_limit" {
				t.Errorf("request %d: 409 with unexpected code %v", i, r.out["code"])
			}
		default:
			t.Errorf("request %d: status %d %v (want 201 or a clean 409)", i, r.code, r.out)
		}
	}
	for name, c := range seenName {
		if c > 1 {
			t.Errorf("slot %s was reported created %d times (duplicate 201)", name, c)
		}
	}
	for port, c := range seenPort {
		if c > 1 {
			t.Errorf("port %d was handed out %d times", port, c)
		}
	}
	after := specialistSlotNames(t, s)
	if len(after) > 12 {
		t.Errorf("%d specialist slots exist, cap is 12", len(after))
	}
	if got, want := len(after)-len(before), created; got != want {
		t.Errorf("%d 201 responses but %d slots actually added", created, got)
	}
	ports := map[int]string{}
	for name, p := range after {
		if other, dup := ports[p]; dup {
			t.Errorf("slots %s and %s share port %d", other, name, p)
		}
		ports[p] = name
	}
}

// Flip one occupant's behavior from many goroutines; afterwards the stored occupant must be in a valid state and no call may 500.
func TestMatrix_ConcurrentBehaviorFlips(t *testing.T) {
	s, _ := matrixServer(t, authz.RoleAdmin)
	createASR(t, s)
	behaviors := []string{"always_on", "on_demand", "disabled"}
	const n = 30
	codes := make([]int, n)
	bodies := make([]string, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], bodies[i] = rawCall(s, "POST", "/api/v1/specialists/occupants/asr/behavior",
				`{"behavior":"`+behaviors[i%3]+`"}`, "Bearer sk-forge-a6a0da5609b8-testsecret123456")
		}(i)
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("flip %d (%s): %d %s, want 200", i, behaviors[i%3], c, bodies[i])
		}
	}
	o, err := s.deps.SpecialistStore.OccupantByName(context.Background(), "asr")
	if err != nil {
		t.Fatal(err)
	}
	if o.Policy != store.PolicyAlwaysOn && o.Policy != store.PolicyOnDemand {
		t.Errorf("invalid policy %q", o.Policy)
	}
	if o.Enabled && o.Policy == store.PolicyAlwaysOn && o.HomeSlot != "s1" && o.HomeSlot != "s2" {
		t.Errorf("enabled always-on member without a valid home slot: %+v", o)
	}
	if o.Stopped {
		t.Errorf("stop flag left set after enable/disable flips: %+v", o)
	}
}

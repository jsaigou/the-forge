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
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSpecialistPool records what the API asks of the runtime pool.
type fakeSpecialistPool struct {
	slots     []specialist.SlotState
	loadErr   error
	loaded    []string
	unloaded  []string
	forgotten []string
	pooled    map[string]bool
	// release, when set, makes Unload block until it is closed (a member mid-generation that takes long to stop).
	release chan struct{}
	mu      sync.Mutex
}

func (f *fakeSpecialistPool) unloadedSlots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unloaded...)
}

func (f *fakeSpecialistPool) Snapshot(context.Context) []specialist.SlotState { return f.slots }
func (f *fakeSpecialistPool) Load(_ context.Context, name, slot string) (specialist.SlotState, error) {
	if f.loadErr != nil {
		return specialist.SlotState{}, f.loadErr
	}
	f.loaded = append(f.loaded, name+"@"+slot)
	return specialist.SlotState{Slot: "s1", Occupant: name, State: specialist.StateRunning}, nil
}
func (f *fakeSpecialistPool) PooledConfig(_ context.Context, c string) bool { return f.pooled[c] }
func (f *fakeSpecialistPool) Forget(name string)                            { f.forgotten = append(f.forgotten, name) }
func (f *fakeSpecialistPool) Unload(_ context.Context, slot string) error {
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloaded = append(f.unloaded, slot)
	return nil
}

func newSpecialistServer(t *testing.T, role authz.Role) (*Server, *fakeSpecialistPool) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	events := bus.New()
	cfg, err := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Paths:  config.Paths{ModelsDir: "/opt/forge/models"},
		Ports:  map[string]int{"stt": 8084, "embedding": 8083, "aligner": 8086},
		SpecialistSlots: map[string]config.Slot{
			"s1": {Unit: "forge-pool@s1", Port: 8100, Label: "S1", Order: 1},
			"s2": {Unit: "forge-pool@s2", Port: 8101, Label: "S2", Order: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := &fakeSpecialistPool{slots: []specialist.SlotState{
		{Slot: "s1", Port: 8100, State: specialist.StateEmpty},
		{Slot: "s2", Port: 8101, State: specialist.StateEmpty},
	}}
	s := New(Deps{
		Snapshots: collector.NewStatic(nil), Engine: &engine.Stub{}, Sched: &sched.Stub{},
		Auth:   &stubAuth{identity: authz.Identity{Name: "t", Role: role}},
		Events: events, Publish: events, Config: func() *config.Config { return cfg }, Hostname: "test-host",
		Specialists: pool, SpecialistStore: db.Specialist(), SpecialistWriter: db.Specialist(), Settings: db.Settings(), Catalog: db.Catalog(),
	})
	t.Cleanup(func() { s.Close() })
	return s, pool
}

func spCall(t *testing.T, s *Server, method, path, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

const asrBody = `{"name":"asr","kind":"qwen3-asr-server","functions":["transcription"],"policy":"on_demand","enabled":true,
 "memory_bytes":1073741824,"params":{"model":"/opt/forge/models/qwen3-asr/q8.gguf","threads":"4"}}`

func TestSpecialistAPI_CRUDPolicyDefaultsAndGet(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)

	code, got, _ := spCall(t, s, "GET", "/api/v1/specialists", "")
	if code != 200 || len(got["slots"].([]any)) != 2 || len(got["occupants"].([]any)) != 0 || len(got["kinds"].([]any)) != 7 {
		t.Fatalf("empty pool: %d %v", code, got)
	}

	// Registry validation is the gate: unknown kind, a path outside the model roots, a parameter the kind does not have.
	for name, body := range map[string]string{
		"unknown kind":       `{"name":"x","kind":"bash","functions":[],"policy":"on_demand","enabled":true}`,
		"path outside":       `{"name":"x","kind":"qwen3-asr-server","functions":[],"policy":"on_demand","enabled":true,"params":{"model":"/etc/passwd"}}`,
		"extra param":        `{"name":"x","kind":"kokoro","functions":[],"policy":"on_demand","enabled":true,"params":{"cmd":"rm -rf /"}}`,
		"always-on, no home": `{"name":"x","kind":"kokoro","functions":["speech"],"policy":"always_on","enabled":true}`,
	} {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", body); code != 422 || out["code"] != "validation_failed" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}

	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 || out["name"] != "asr" {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 409 || out["code"] != "already_exists" {
		t.Errorf("duplicate: %d %v", code, out)
	}

	// The toggle is a narrow write: it changes the policy and nothing else.
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/policy", `{"policy":"always_on","home_slot":"s2"}`); code != 200 ||
		out["policy"] != "always_on" || out["home_slot"] != "s2" || out["memory_bytes"].(float64) != 1073741824 || out["params"].(map[string]any)["threads"] != "4" {
		t.Fatalf("policy toggle: %d %v", code, out)
	}
	// Two always-on members cannot share a home slot.
	kokoro := `{"name":"kokoro","kind":"kokoro","functions":["speech"],"policy":"always_on","home_slot":"s2","enabled":true}`
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", kokoro); code != 422 {
		t.Errorf("shared home slot: %d %v", code, out)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/policy", `{"policy":"sometimes"}`); code != 422 {
		t.Errorf("bad policy: %d", code)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/ghost/policy", `{"policy":"on_demand"}`); code != 404 {
		t.Errorf("policy of a missing occupant: %d", code)
	}

	// Defaults: only for a function the occupant serves.
	if code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/defaults/transcription", `{"occupant":"asr"}`); code != 200 || out["defaults"].(map[string]any)["transcription"] != "asr" {
		t.Fatalf("set default: %d %v", code, out)
	}
	if code, _, _ := spCall(t, s, "PUT", "/api/v1/specialists/defaults/speech", `{"occupant":"asr"}`); code != 422 {
		t.Errorf("default for a function the occupant does not serve: %d", code)
	}

	// Load and unload go to the runtime pool; a running occupant cannot be deleted from under its slot.
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/load", `{"occupant":"asr","slot":"s1"}`); code != 200 || len(pool.loaded) != 1 || pool.loaded[0] != "asr@s1" {
		t.Fatalf("load: %d %v %v", code, out, pool.loaded)
	}
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyAlwaysOn, LastUse: time.Now().Add(-30 * time.Second)}
	if code, out, _ := spCall(t, s, "DELETE", "/api/v1/specialists/occupants/asr", ""); code != 409 || out["code"] != "has_dependents" {
		t.Errorf("delete while running: %d %v", code, out)
	}
	_, got, _ = spCall(t, s, "GET", "/api/v1/specialists", "")
	slot := got["slots"].([]any)[0].(map[string]any)
	if slot["state"] != "running" || slot["idle_seconds"].(float64) < 29 || slot["label"] != "S1" {
		t.Errorf("slot JSON = %v", slot)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/specialists/unload", `{"slot":"s1"}`); code != 200 || len(pool.unloaded) != 1 {
		t.Errorf("unload: %d %v", code, pool.unloaded)
	}
	// An operator-stopped always-on member holds no process; it can be deleted, and the pool is told to forget its slot record.
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateStopped, Policy: store.PolicyAlwaysOn}
	if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/occupants/asr", ""); code != 204 {
		t.Errorf("delete of a stopped member: %d", code)
	}
	if len(pool.forgotten) != 1 || pool.forgotten[0] != "asr" {
		t.Errorf("the pool was not told to forget the deleted occupant: %v", pool.forgotten)
	}
	if _, out, _ := spCall(t, s, "GET", "/api/v1/specialists", ""); len(out["defaults"].(map[string]any)) != 0 {
		t.Errorf("a default survived its occupant: %v", out["defaults"])
	}
}

func TestSpecialistAPI_PoolErrorsKeepTheirCodes(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	pool.loadErr = &specialist.PoolFullError{Occupant: "asr", Holders: []string{"s1 a (always_on, running)"}, RetryAfter: 12 * time.Second}
	code, out, hdr := spCall(t, s, "POST", "/api/v1/specialists/load", `{"occupant":"asr"}`)
	if code != 503 || out["code"] != "specialist_pool_full" || hdr.Get("Retry-After") != "12" {
		t.Errorf("pool full: %d %v Retry-After=%q", code, out, hdr.Get("Retry-After"))
	}
	pool.loadErr = &specialist.UnavailableError{Occupant: "asr", Reason: "failed: exited 1"}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/load", `{"occupant":"asr"}`); code != 503 || out["code"] != "specialist_unavailable" {
		t.Errorf("unavailable: %d %v", code, out)
	}
	pool.loadErr = specialist.ErrNotFound
	if code, _, _ := spCall(t, s, "POST", "/api/v1/specialists/load", `{"occupant":"ghost"}`); code != 404 {
		t.Errorf("missing occupant: %d", code)
	}
}

func TestSpecialistAPI_ViewersReadAdminsWrite(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleViewer)
	if code, _, _ := spCall(t, s, "GET", "/api/v1/specialists", ""); code != 200 {
		t.Errorf("a viewer cannot read the pool: %d", code)
	}
	for _, c := range [][3]string{
		{"POST", "/api/v1/specialists/load", `{"occupant":"a"}`},
		{"POST", "/api/v1/specialists/unload", `{"slot":"s1"}`},
		{"POST", "/api/v1/specialists/occupants", asrBody},
		{"PUT", "/api/v1/specialists/defaults/speech", `{"occupant":"a"}`},
		{"POST", "/api/v1/specialists/occupants/a/policy", `{"policy":"on_demand"}`},
		{"DELETE", "/api/v1/specialists/occupants/a", ""},
	} {
		if code, _, _ := spCall(t, s, c[0], c[1], c[2]); code != 403 {
			t.Errorf("a viewer got %d on %s %s, want 403", code, c[0], c[1])
		}
	}
}

func infraRow(t *testing.T, s *Server, name string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/infra-services", nil)
	req.Header.Set("Authorization", "Bearer sk-forge-a6a0da5609b8-testsecret123456")
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("infra-services: %v (%s)", err, rec.Body)
	}
	for _, r := range out.Services {
		if r["name"] == name {
			return r
		}
	}
	t.Fatalf("no %q row in %v", name, out.Services)
	return nil
}

// ADR-0022: once the pool answers a fixed service's function, its infra row follows the pool member (unit, port, state), and its start and stop
// buttons drive the pool instead of the old unit.
func TestInfraRows_FollowThePoolMember(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	ctx := context.Background()

	if r := infraRow(t, s, "STT"); r["unit"] != "forge-stt" || r["port"].(float64) != 8084 {
		t.Fatalf("with no pool default the row must be the old one: %v", r)
	}
	if _, err := s.deps.SpecialistWriter.CreateOccupant(ctx, store.Occupant{Name: "asr", Kind: "qwen3-asr-server", Functions: []string{store.FnTranscription},
		Policy: store.PolicyAlwaysOn, HomeSlot: "s1", Enabled: true, Params: map[string]string{"model": "/opt/forge/models/a.gguf"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.deps.SpecialistWriter.SetDefault(ctx, store.FnTranscription, "asr"); err != nil {
		t.Fatal(err)
	}

	// Registered, default set, but not in any slot: honest "not loaded", never green.
	r := infraRow(t, s, "STT")
	if r["active"] != false || !strings.Contains(r["detail"].(string), "specialist pool · asr · empty") {
		t.Errorf("not loaded: %v", r)
	}

	pool.slots[0] = specialist.SlotState{Slot: "s1", Port: 8100, Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyAlwaysOn}
	r = infraRow(t, s, "STT")
	if r["active"] != true || r["unit"] != "forge-pool@s1" || r["port"].(float64) != 8100 || r["detail"] != "specialist pool · s1 · asr" {
		t.Errorf("running member: %v", r)
	}
	pool.slots[0].State, pool.slots[0].Err = specialist.StateFailed, "exited 1"
	if r = infraRow(t, s, "STT"); r["active"] != false || !strings.Contains(r["detail"].(string), "failed: exited 1") {
		t.Errorf("failed member: %v", r)
	}
	// The other fixed rows are untouched.
	if r := infraRow(t, s, "Embedding"); r["unit"] != "forge-embedding" {
		t.Errorf("Embedding changed without a pool default: %v", r)
	}

	// Stop and start go through the pool.
	pool.slots[0].State = specialist.StateRunning
	if code, out, _ := spCall(t, s, "POST", "/api/v1/stt/stop", ""); code != 200 || len(pool.unloaded) != 1 || pool.unloaded[0] != "s1" {
		t.Errorf("stop: %d %v unloaded=%v", code, out, pool.unloaded)
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/stt/start", ""); code != 200 || len(pool.loaded) != 1 || pool.loaded[0] != "asr@s1" {
		t.Errorf("start: %d %v loaded=%v", code, out, pool.loaded)
	}
}

// fakeSpecialistCatalog resolves catalog configs for the occupant API's validation, like the daemon's merged-config resolver.
type fakeSpecialistCatalog struct {
	specs map[string]specialist.LaunchSpec
}

func (f fakeSpecialistCatalog) LaunchSpec(_ context.Context, name string) (specialist.LaunchSpec, error) {
	sp, ok := f.specs[name]
	if !ok {
		return specialist.LaunchSpec{}, errors.New("no catalog config named " + name)
	}
	return sp, nil
}
func (f fakeSpecialistCatalog) LaunchRow(context.Context, string, string, int) (store.SlotLaunch, error) {
	return store.SlotLaunch{}, nil
}
func (f fakeSpecialistCatalog) EstimateBytes(context.Context, string) int64 { return 7 << 30 }

// ADR-0022 step 2: a catalog config may be registered as a specialist if it exists and the pool can run it; it has one owner; and its memory
// defaults to the catalog's own figure.
func TestSpecialistAPI_CatalogConfigOccupant(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	good := specialist.LaunchSpec{Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000}
	bad := good
	bad.Bin = "/usr/local/bin/llama-server"
	rocmBad := good
	rocmBad.Args = []string{"--port", "9"}
	s.deps.SpecialistCatalog = fakeSpecialistCatalog{specs: map[string]specialist.LaunchSpec{"d1-2b": good, "weird": bad, "owned-args": rocmBad}}

	body := func(name, config string) string {
		return `{"name":"` + name + `","kind":"catalog-config","functions":["decisions"],"policy":"on_demand","enabled":true,"params":{"config":"` + config + `"}}`
	}
	for name, b := range map[string]string{
		"unknown config":          body("x", "no-such"),
		"binary outside the pool": body("x", "weird"),
		"args set the port":       body("x", "owned-args"),
		"missing config param":    `{"name":"x","kind":"catalog-config","functions":[],"policy":"on_demand","enabled":true,"params":{}}`,
	} {
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", b); code != 422 || out["code"] != "validation_failed" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", body("d1-member", "d1-2b"))
	if code != 201 || out["memory_bytes"].(float64) != float64(7<<30) {
		t.Fatalf("create: %d %v (memory should default to the catalog's 7 GiB)", code, out)
	}
	// One owner: a second occupant may not host the same config.
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", body("d1-again", "d1-2b")); code != 422 || !strings.Contains(out["fields"].(map[string]any)["params"].(string), "already hosted by d1-member") {
		t.Errorf("second owner: %d %v", code, out)
	}
	// Editing the owner itself is fine.
	if code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/occupants/d1-member", `{"name":"d1-member","kind":"catalog-config","functions":["decisions"],"policy":"on_demand","enabled":true,"memory_bytes":9663676416,"params":{"config":"d1-2b"}}`); code != 200 {
		t.Errorf("editing the owner: %d %v", code, out)
	}
}

func TestSpecialistAPI_Behavior(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	post := func(b string) (int, map[string]any) {
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", b)
		return code, out
	}

	if code, _ := post(`{"behavior":"sometimes"}`); code != 422 {
		t.Errorf("bad behavior: %d", code)
	}
	if code, _, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/ghost/behavior", `{"behavior":"disabled"}`); code != 404 {
		t.Errorf("missing occupant: %d", code)
	}

	// Always on picks a free slot, records it as home, and loads the member there.
	code, out := post(`{"behavior":"always_on"}`)
	if code != 200 || out["policy"] != "always_on" || out["home_slot"] != "s1" || out["enabled"] != true {
		t.Fatalf("always on: %d %v", code, out)
	}
	if len(pool.loaded) != 1 || pool.loaded[0] != "asr@s1" {
		t.Errorf("always on did not load into the chosen slot: %v", pool.loaded)
	}

	// Disabled unloads the running member and keeps the registration.
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyAlwaysOn}
	code, out = post(`{"behavior":"disabled"}`)
	if code != 200 || out["enabled"] != false || len(pool.unloaded) != 1 || pool.unloaded[0] != "s1" {
		t.Fatalf("disabled: %d %v unloaded=%v", code, out, pool.unloaded)
	}
	if _, got, _ := spCall(t, s, "GET", "/api/v1/specialists", ""); len(got["occupants"].([]any)) != 1 {
		t.Errorf("disabling dropped the registration: %v", got["occupants"])
	}

	// With s1 taken, re-enabling as Always on lands on the next free slot; with none free it says so.
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "other", State: specialist.StateRunning}
	if code, out = post(`{"behavior":"always_on"}`); code != 200 || out["home_slot"] != "s2" {
		t.Fatalf("re-enable picks the next free slot: %d %v", code, out)
	}
	pool.slots[1] = specialist.SlotState{Slot: "s2", Occupant: "other2", State: specialist.StateRunning}
	post(`{"behavior":"disabled"}`)
	if code, out = post(`{"behavior":"always_on"}`); code != 409 || out["code"] != "specialist_no_free_slot" {
		t.Errorf("no free slot: %d %v", code, out)
	}
	// A member whose slot record is stopped (the earlier disable left it there) is not running: enabling must load it again, in its own slot.
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateStopped, Policy: store.PolicyAlwaysOn}
	pool.slots[1] = specialist.SlotState{Slot: "s2", State: specialist.StateEmpty}
	pool.loaded = nil
	if code, out = post(`{"behavior":"always_on"}`); code != 200 || out["home_slot"] != "s1" || len(pool.loaded) != 1 || pool.loaded[0] != "asr@s1" {
		t.Errorf("enabling a stopped member did not load it: %d %v loaded=%v", code, out, pool.loaded)
	}
	// On demand needs no slot at all.
	if code, out = post(`{"behavior":"on_demand"}`); code != 200 || out["policy"] != "on_demand" || out["enabled"] != true {
		t.Errorf("on demand: %d %v", code, out)
	}
}

func TestSpecialistAPI_SlotLogIsRedactedAndAdminOnly(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	s.deps.SlotLog = func(_ context.Context, unit string, n int) string {
		return "loading model\napi_key=abc123secret\nunit=" + unit + "\n"
	}
	code, out, _ := spCall(t, s, "GET", "/api/v1/specialists/slots/s1/log", "")
	text, _ := out["lines"].(string)
	if code != 200 || strings.Contains(text, "abc123secret") || !strings.Contains(text, "forge-pool@s1") {
		t.Errorf("log: %d %q", code, text)
	}
	if code, _, _ := spCall(t, s, "GET", "/api/v1/specialists/slots/s9/log", ""); code != 404 {
		t.Errorf("unknown slot: %d", code)
	}
	v, _ := newSpecialistServer(t, authz.RoleViewer)
	v.deps.SlotLog = s.deps.SlotLog
	if code, _, _ := spCall(t, v, "GET", "/api/v1/specialists/slots/s1/log", ""); code != 403 {
		t.Errorf("viewer read the log: %d", code)
	}
}

func TestLoad_RefusesAConfigThePoolOwns(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	pool.pooled = map[string]bool{"d1-2b": true}
	code, out, _ := spCall(t, s, "POST", "/api/v1/load", `{"mode":"d1-2b","slot":"a1"}`)
	if code != 409 || out["code"] != "specialist_owned_config" {
		t.Errorf("load of a pooled config: %d %v", code, out)
	}
}

func TestSpecialistAPI_AddAndRemoveSlots(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	reloads := 0
	s.deps.ReloadConfig = func() { reloads++ }

	// The test DB is seeded with the migration's slots; adding keeps counting up from what exists and uses the next port.
	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", "")
	if code != 201 || reloads != 1 {
		t.Fatalf("add: %d %v reloads=%d", code, out, reloads)
	}
	name := out["slot"].(string)
	port := int(out["port"].(float64))
	if port < 8100 || port > 8111 || out["label"] != strings.ToUpper(name) {
		t.Errorf("slot = %v", out)
	}
	// It is a specialist slot, and an occupied one cannot be removed.
	pool.slots = append(pool.slots, specialist.SlotState{Slot: name, Occupant: "asr", State: specialist.StateRunning})
	if code, o, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/"+name, ""); code != 409 || o["code"] != "has_dependents" {
		t.Errorf("remove occupied: %d %v", code, o)
	}
	pool.slots = pool.slots[:len(pool.slots)-1]
	if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/"+name, ""); code != 204 || reloads != 2 {
		t.Errorf("remove empty: %d reloads=%d", code, reloads)
	}
	if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/"+name, ""); code != 404 {
		t.Errorf("remove twice: %d", code)
	}
	// LLM slots are never removable through this endpoint.
	if code, _, _ := spCall(t, s, "DELETE", "/api/v1/specialists/slots/a1", ""); code != 404 {
		t.Errorf("remove an LLM slot: %d", code)
	}
	// The cap.
	for i := 0; i < 20; i++ {
		if code, o, _ := spCall(t, s, "POST", "/api/v1/specialists/slots", ""); code == 409 {
			if o["code"] != "specialist_slot_limit" {
				t.Errorf("cap code = %v", o)
			}
			return
		}
	}
	t.Error("slots were added without limit")
}

// A slow stop (a member mid-generation) must not hold the HTTP call past the client's timeout: the unload answers 202 "stopping" after a bounded
// wait, the stop keeps going on a context detached from the request, and the slot reads "stopping" until it ends.
func TestSpecialistAPI_UnloadSlowStopAnswers202Stopping(t *testing.T) {
	old := specialistUnloadWait
	specialistUnloadWait = 50 * time.Millisecond
	t.Cleanup(func() { specialistUnloadWait = old })
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	pool.release = make(chan struct{})
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyOnDemand}

	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/unload", `{"slot":"s1"}`)
	if code != http.StatusAccepted || out["state"] != "stopping" || out["slot"] != "s1" {
		t.Fatalf("slow unload: %d %v (want 202 stopping)", code, out)
	}
	if got := pool.unloadedSlots(); len(got) != 0 {
		t.Fatalf("the stop finished before it was released: %v", got)
	}
	// The pool reports the slot as stopping while the stop runs; the API passes that through.
	pool.slots[0].State = specialist.StateStopping
	_, got, _ := spCall(t, s, "GET", "/api/v1/specialists", "")
	if st := got["slots"].([]any)[0].(map[string]any)["state"]; st != "stopping" {
		t.Errorf("GET during the stop: state = %v, want stopping", st)
	}
	close(pool.release) // the stop was not abandoned: it completes after the response was sent
	waitPoolCond(t, func() bool { return len(pool.unloadedSlots()) == 1 })
}

// Behavior=disabled with a slow stop: 202 stopping, and the operator-stop flag is cleared only AFTER the stop (so the stop's own record cannot
// overwrite it).
func TestSpecialistAPI_BehaviorDisabledSlowStopAnswers202(t *testing.T) {
	old := specialistUnloadWait
	specialistUnloadWait = 50 * time.Millisecond
	t.Cleanup(func() { specialistUnloadWait = old })
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	ctx := context.Background()
	if err := s.deps.SpecialistWriter.SetStopped(ctx, "asr", true); err != nil {
		t.Fatal(err)
	}
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyOnDemand}
	pool.release = make(chan struct{})

	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`)
	if code != http.StatusAccepted || out["state"] != "stopping" || out["slot"] != "s1" || out["enabled"] != false {
		t.Fatalf("disabled, slow stop: %d %v (want 202 stopping)", code, out)
	}
	if o, _ := s.deps.SpecialistStore.OccupantByName(ctx, "asr"); !o.Stopped {
		t.Error("the stop flag was cleared before the stop finished")
	}
	close(pool.release)
	waitPoolCond(t, func() bool {
		o, err := s.deps.SpecialistStore.OccupantByName(ctx, "asr")
		return err == nil && !o.Stopped && len(pool.unloadedSlots()) == 1
	})
}

func waitPoolCond(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in 2 s")
}

// A second unload of a slot that is already stopping must say "stopping", not claim the slot is unloaded.
func TestSpecialistAPI_UnloadOfAStoppingSlotAnswersStopping(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateStopping, Policy: store.PolicyOnDemand}
	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/unload", `{"slot":"s1"}`)
	if code != 202 || out["state"] != specialist.StateStopping {
		t.Fatalf("unload of a stopping slot: %d %v, want 202 stopping", code, out)
	}
	if len(pool.unloaded) != 0 {
		t.Errorf("a second stop was started on a slot that is already stopping: %v", pool.unloaded)
	}
}

// Disabling a member frees its slot: the pool is told to forget the member's record once its stop has finished.
func TestSpecialistAPI_DisableForgetsTheMembersSlotRecord(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateRunning, Policy: store.PolicyAlwaysOn}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`); code != 200 {
		t.Fatalf("disable: %d %v", code, out)
	}
	if len(pool.unloaded) != 1 || len(pool.forgotten) != 1 || pool.forgotten[0] != "asr" {
		t.Errorf("unloaded=%v forgotten=%v, want the slot stopped and the member forgotten", pool.unloaded, pool.forgotten)
	}
}

// A speech engine is an ordinary pool occupant now: the front door asks the daemon for it on every request (and holds a lease while it runs), so any
// of the three behaviors is allowed, on every endpoint that can set one, and nothing is applied to the forge-tts process.
func TestSpecialistAPI_ASpeechEngineMayBeOnDemandAlwaysOnOrDisabled(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", `{"name":"tts-a","kind":"qwen-tts-server","functions":["speech"],"policy":"always_on","home_slot":"s1","enabled":true,
	 "params":{"model":"/opt/forge/models/a.gguf","codec":"/opt/forge/models/c.gguf","alias":"base"}}`); code != 201 {
		t.Fatalf("setup: %d %v", code, out)
	}
	for name, c := range map[string][3]string{
		"behavior": {"POST", "/api/v1/specialists/occupants/tts-a/behavior", `{"behavior":"on_demand"}`},
		"policy":   {"POST", "/api/v1/specialists/occupants/tts-a/policy", `{"policy":"on_demand"}`},
		"update": {"PUT", "/api/v1/specialists/occupants/tts-a", `{"name":"tts-a","kind":"qwen-tts-server","functions":["speech"],"policy":"on_demand","enabled":true,
		 "params":{"model":"/opt/forge/models/a.gguf","codec":"/opt/forge/models/c.gguf","alias":"base"}}`},
	} {
		if code, out, _ := spCall(t, s, c[0], c[1], c[2]); code != 200 {
			t.Errorf("%s: %d %v, want 200", name, code, out)
		}
		if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/tts-a/behavior", `{"behavior":"always_on"}`); code != 200 {
			t.Errorf("%s then always_on: %d %v", name, code, out)
		}
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/tts-a/behavior", `{"behavior":"disabled"}`); code != 200 {
		t.Errorf("disabled: %d %v", code, out)
	}
}

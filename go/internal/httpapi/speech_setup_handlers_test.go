// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

// catalogPool is a pool whose slots are the catalog's, as the real one's are.
type catalogPool struct {
	*fakeSpecialistPool
	db *store.DB
}

func (p catalogPool) Snapshot(ctx context.Context) []specialist.SlotState {
	slots, err := p.db.Catalog().ListSlots(ctx)
	if err != nil {
		panic(err)
	}
	out := []specialist.SlotState{}
	for _, s := range slots {
		if s.Pool == store.PoolSpecialist {
			out = append(out, specialist.SlotState{Slot: s.Name, Port: s.Port, State: specialist.StateEmpty})
		}
	}
	return out
}

// newSpeechSetupServer is a server on an empty database: no engines and no engine slots.
func newSpeechSetupServer(t *testing.T, role authz.Role) (*Server, *store.DB, string) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := t.TempDir()
	events := bus.New()
	cfg := func() *config.Config {
		slots := map[string]config.Slot{}
		for _, s := range (catalogPool{db: db}).Snapshot(context.Background()) {
			slots[s.Slot] = config.Slot{Unit: "forge-pool@" + s.Slot, Port: s.Port}
		}
		return &config.Config{Paths: config.Paths{ModelsDir: "/var/lib/forge/models"}, SpecialistSlots: slots}
	}
	s := New(Deps{
		Snapshots: collector.NewStatic(nil), Engine: &engine.Stub{}, Sched: &sched.Stub{},
		Auth:   &stubAuth{identity: authz.Identity{Name: "t", Role: role, KeyID: "k"}},
		Events: events, Publish: events, Config: cfg, Hostname: "test-host", SpeechRoot: root,
		Specialists: catalogPool{fakeSpecialistPool: &fakeSpecialistPool{}, db: db}, SpecialistStore: db.Specialist(), SpecialistWriter: db.Specialist(),
		Settings: db.Settings(), Catalog: db.Catalog(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db, root
}

func fakeEngineFiles(t *testing.T, root, id string) {
	t.Helper()
	r, ok := speechsetup.RecipeByID(id)
	if !ok {
		t.Fatalf("no recipe %s", id)
	}
	for _, f := range r.Files {
		p := path.Join(root, f.Path)
		if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, f.Size); err != nil {
			t.Fatal(err)
		}
	}
}

func capability(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	code, out, _ := spCall(t, s, "GET", "/api/v1/speech/readiness", "")
	if code != 200 {
		t.Fatalf("readiness: %d %v", code, out)
	}
	for _, c := range out["capabilities"].([]any) {
		if m := c.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no capability %s: %v", id, out)
	return nil
}

func TestSpeechSetup_FromAnEmptyDatabaseToARegisteredDefault(t *testing.T) {
	s, db, root := newSpeechSetupServer(t, authz.RoleAdmin)
	if c := capability(t, s, "preset"); c["state"] != "not_installed" || c["recipe"] != "kokoro" {
		t.Fatalf("nothing there: %v", c)
	}
	code, out, _ := spCall(t, s, "POST", "/api/v1/speech/recipes/kokoro/register", "")
	if code != 409 || out["code"] != "speech_files_incomplete" {
		t.Fatalf("no files: %d %v", code, out)
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/speech/recipes/nope/register", ""); code != 404 || out["code"] != "speech_recipe_unknown" {
		t.Fatalf("unknown: %d %v", code, out)
	}

	fakeEngineFiles(t, root, speechsetup.RecipeKokoro)
	if c := capability(t, s, "preset"); c["state"] != "not_registered" || c["recipe"] != "kokoro" {
		t.Fatalf("files here: %v", c)
	}
	_, out, _ = spCall(t, s, "GET", "/api/v1/speech/recipes", "")
	states := map[any]any{}
	for _, r := range out["recipes"].([]any) {
		states[r.(map[string]any)["id"]] = r.(map[string]any)["state"]
	}
	if out["root"] != root || states["kokoro"] != "present" || states["zonos"] != "absent" {
		t.Fatalf("recipes: %v", out)
	}
	for _, r := range out["recipes"].([]any) {
		m := r.(map[string]any)
		command, _ := m["install_command"].(string)
		// Only an engine that is absent and can be built gets a command.
		if (command != "") != (m["installable"] == true && m["state"] == "absent") {
			t.Errorf("%s: installable=%v state=%v command=%q", m["id"], m["installable"], m["state"], command)
		}
	}

	code, out, _ = spCall(t, s, "POST", "/api/v1/speech/recipes/kokoro/register", "")
	if code != 200 || out["created"] != true || out["slot_added"] != "s1" || out["default_set"] != true || out["restart_required"] != true {
		t.Fatalf("register: %d %v", code, out)
	}
	o, err := db.Specialist().OccupantByName(context.Background(), "kokoro")
	if err != nil || o.Policy != store.PolicyAlwaysOn || o.HomeSlot != "s1" || !o.Enabled {
		t.Fatalf("engine: %+v %v", o, err)
	}
	defaults, err := s.readSpeechDefaults(context.Background())
	if err != nil || defaults["kokoro"] != "kokoro" {
		t.Fatalf("defaults: %v %v", defaults, err)
	}
	if got := s.resolvedRouterConfig(context.Background()).TTSURL; got != speechsetup.DefaultFrontDoorURL {
		t.Fatalf("tts_url: %q", got)
	}
	if c := capability(t, s, "preset"); c["usable"] != true || c["engine"] != "kokoro" {
		t.Fatalf("after: %v", c)
	}
	code, out, _ = spCall(t, s, "POST", "/api/v1/speech/recipes/kokoro/register", "")
	if code != 200 || out["created"] != false || out["restart_required"] != false || out["slot_added"] != nil {
		t.Fatalf("again: %d %v", code, out)
	}

	// A file that disappears is reported against the engine.
	if err := os.Remove(path.Join(root, "kokoro/server.py")); err != nil {
		t.Fatal(err)
	}
	if c := capability(t, s, "preset"); c["state"] != "files_missing" || c["usable"] != false {
		t.Fatalf("file gone: %v", c)
	}
}

func TestSpeechSetup_RegisteringNeedsAnAdmin(t *testing.T) {
	s, _, root := newSpeechSetupServer(t, authz.RoleOperator)
	fakeEngineFiles(t, root, speechsetup.RecipeKokoro)
	if code, out, _ := spCall(t, s, "POST", "/api/v1/speech/recipes/kokoro/register", ""); code != 403 {
		t.Fatalf("operator: %d %v", code, out)
	}
	if code, _, _ := spCall(t, s, "GET", "/api/v1/speech/recipes", ""); code != 200 {
		t.Fatalf("an operator may look: %d", code)
	}
}

func TestSpeechSetupScope(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/speech/recipes", true},
		{"GET", "/api/v1/speech/readiness", true},
		{"GET", "/api/v1/external-tools", true},
		{"PUT", "/api/v1/external-tools", false},
		{"POST", "/api/v1/speech/recipes/zonos/register", true},
		{"DELETE", "/api/v1/keys/self", true},
		{"POST", "/api/v1/speech/recipes//register", false},
		{"POST", "/api/v1/speech/recipes/a/b/register", false},
		{"POST", "/api/v1/speech/recipes/../register", false},
		{"PUT", "/api/v1/speech/defaults", false},
		{"POST", "/api/v1/specialists/slots", false},
		{"POST", "/api/v1/specialists/occupants", false},
		{"GET", "/api/v1/keys", false},
	} {
		if got := scopeAllows(authz.ScopeSpeechSetup, tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: %v", tc.method, tc.path, got)
		}
	}
	for _, scope := range []string{authz.ScopeSpeech, authz.ScopeDuplex, authz.ScopeCatalog} {
		if scopeAllows(scope, "POST", "/api/v1/speech/recipes/kokoro/register") {
			t.Errorf("the %s scope must not register engines", scope)
		}
	}
}

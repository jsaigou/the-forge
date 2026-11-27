// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

func serverWithVoiceSettings(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Settings:  db.Settings(),
		Audit:     db.Audit(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db
}

// putRouterTTSURL points the voice list at a fake forge-tts, the way Settings -> Routing does (infra.router.tts_url).
func putRouterTTSURL(t *testing.T, db *store.DB, url string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"tts_url": url})
	if err := db.Settings().Set(t.Context(), "infra.router", raw); err != nil {
		t.Fatal(err)
	}
}

// GET /api/v1/voice/list: every voice gets its kind (derived from forge-tts's type) and the engine forge-tts says it is bound to, empty when it has none.
func TestVoiceList_CarriesKindAndEngine(t *testing.T) {
	s, db := serverWithVoiceSettings(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voices" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"voices":[
		 {"id":"af_heart","name":"Heart","type":"kokoro","language":"English","tier":"fast","builtin":true},
		 {"id":"sana","name":"Sana","type":"clone","engine":"qwen3-tts-base","language":"English"},
		 {"id":"susan","name":"Susan","type":"design","language":"English"},
		 {"id":"serena","name":"Serena","type":"customvoice","engine":"my-custom","language":"English"},
		 {"id":"odd","name":"Odd","type":"mystery"}]}`))
	}))
	defer up.Close()
	putRouterTTSURL(t, db, up.URL)

	w := do(t, s, authedRequest("GET", "/api/v1/voice/list", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Voices []map[string]any `json:"voices"`
	}
	decodeJSON(t, w.Body, &got)
	type ke struct{ kind, engine string }
	want := map[string]ke{
		"af_heart": {"kokoro", ""},
		"sana": {"base", "qwen3-tts-base"}, // a clone voice is "base"; the engine is the one the voice is bound to
		"susan":    {"voicedesign", ""},        // no engine: the default for its kind applies
		"serena":   {"customvoice", "my-custom"},
		"odd":      {"mystery", ""}, // an unknown type is passed through as its own kind rather than hidden
	}
	if len(got.Voices) != len(want) {
		t.Fatalf("voices = %v", got.Voices)
	}
	for _, v := range got.Voices {
		id, _ := v["id"].(string)
		k, _ := v["kind"].(string)
		e, _ := v["engine"].(string)
		if w := want[id]; k != w.kind || e != w.engine {
			t.Errorf("%s: kind=%q engine=%q, want kind=%q engine=%q", id, k, e, w.kind, w.engine)
		}
	}
}

func TestVoiceList_ErrorsAreNotSwallowed(t *testing.T) {
	s, db := serverWithVoiceSettings(t)
	// No TTS URL configured.
	putRouterTTSURL(t, db, "")
	if w := do(t, s, authedRequest("GET", "/api/v1/voice/list", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no tts_url: %d, want 503", w.Code)
	}
	// forge-tts answers 500, then something that is not the voice list.
	for _, c := range []struct {
		status int
		body   string
	}{{500, "boom"}, {200, "not json"}} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		putRouterTTSURL(t, db, up.URL)
		if w := do(t, s, authedRequest("GET", "/api/v1/voice/list", nil)); w.Code != http.StatusBadGateway {
			t.Errorf("upstream %d %q: %d, want 502", c.status, c.body, w.Code)
		}
		up.Close()
	}
}

// The settings routes of the old four-row model are gone: Voice & Speech settings are the pool's behaviors and the tts.defaults setting.
func TestVoiceSettingsRoutesAreGone(t *testing.T) {
	s, _ := serverWithVoiceSettings(t)
	for _, m := range []string{"GET", "PUT"} {
		if w := do(t, s, authedRequest(m, "/api/v1/voice/settings", nil)); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/voice/settings = %d, want it gone", m, w.Code)
		}
	}
}

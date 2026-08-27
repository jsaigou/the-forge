// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
	"github.com/jsaigou/the-forge/internal/ttsctl"
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

func TestVoiceSettingsGetPut(t *testing.T) {
	s, _ := serverWithVoiceSettings(t)

	w := do(t, s, authedRequest("GET", "/api/v1/voice/settings", nil))
	if w.Code != 200 {
		t.Fatalf("get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got ttsctl.Engines
	decodeJSON(t, w.Body, &got)
	if got.Kokoro.Mode != ttsctl.ModeResident {
		t.Errorf("default kokoro mode = %q, want resident", got.Kokoro.Mode)
	}
	if got.VoiceDesign.Mode != ttsctl.ModeAvailable {
		t.Errorf("default voicedesign mode = %q, want available", got.VoiceDesign.Mode)
	}

	body := `{"kokoro":{"enabled":true,"mode":"resident","unit":"kokoro","url":"http://127.0.0.1:8880"},` +
		`"customvoice":{"enabled":true,"mode":"resident","unit":"forge-tts-custom","url":"http://127.0.0.1:8091"},` +
		`"voicedesign":{"enabled":false,"mode":"disabled","unit":"","url":""},` +
		`"base":{"enabled":true,"mode":"available","unit":"forge-tts-base","url":""}}`
	w = do(t, s, authedRequest("PUT", "/api/v1/voice/settings", bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var updated ttsctl.Engines
	decodeJSON(t, w.Body, &updated)
	if updated.VoiceDesign.Mode != ttsctl.ModeDisabled || updated.VoiceDesign.Enabled {
		t.Errorf("voicedesign after PUT = %+v, want disabled/false", updated.VoiceDesign)
	}
	if updated.CustomVoice.URL != "http://127.0.0.1:8091" {
		t.Errorf("customvoice.url = %q, want http://127.0.0.1:8091", updated.CustomVoice.URL)
	}

	// A GET afterwards must reflect the PUT.
	w = do(t, s, authedRequest("GET", "/api/v1/voice/settings", nil))
	var reread ttsctl.Engines
	decodeJSON(t, w.Body, &reread)
	if reread.VoiceDesign.Mode != ttsctl.ModeDisabled {
		t.Errorf("reread voicedesign mode = %q, want disabled", reread.VoiceDesign.Mode)
	}
}

func TestVoiceSettingsPut_ValidatesAllOrNothing(t *testing.T) {
	s, _ := serverWithVoiceSettings(t)

	bad := `{"kokoro":{"enabled":true,"mode":"resident","unit":"Kokoro Bad!","url":""},` +
		`"customvoice":{"enabled":true,"mode":"bogus-mode","unit":"","url":""},` +
		`"voicedesign":{"enabled":true,"mode":"available","unit":"","url":"not-a-url"},` +
		`"base":{"enabled":true,"mode":"resident","unit":"","url":""}}`
	w := do(t, s, authedRequest("PUT", "/api/v1/voice/settings", bytes.NewBufferString(bad)))
	if w.Code != 422 {
		t.Fatalf("put(bad body) = %d, want 422; body=%s", w.Code, w.Body.String())
	}

	// Nothing should have been written — a GET still shows defaults.
	w = do(t, s, authedRequest("GET", "/api/v1/voice/settings", nil))
	var got ttsctl.Engines
	decodeJSON(t, w.Body, &got)
	if got.Kokoro.Mode != ttsctl.ModeResident || got.Kokoro.Unit != "" {
		t.Errorf("settings after a rejected PUT = %+v, want unchanged defaults", got)
	}
}

func TestVoiceSettingsPut_AppliesLiveWhenProvisionerWired(t *testing.T) {
	s, db := serverWithVoiceSettings(t)
	fs := &fakeVoiceSystemd{}
	s.deps.TTSProvisioner = &ttsctl.Provisioner{Systemd: fs, EnvDir: t.TempDir()}

	body := `{"kokoro":{"enabled":true,"mode":"resident","unit":"kokoro","url":""},` +
		`"customvoice":{"enabled":true,"mode":"resident","unit":"forge-tts-custom","url":""},` +
		`"voicedesign":{"enabled":true,"mode":"available","unit":"","url":""},` +
		`"base":{"enabled":true,"mode":"resident","unit":"forge-tts-base","url":""}}`
	w := do(t, s, authedRequest("PUT", "/api/v1/voice/settings", bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fs.restarts) != 1 || fs.restarts[0] != "forge-tts" {
		t.Fatalf("Provisioner.Apply should have restarted forge-tts, got restarts=%v", fs.restarts)
	}
	if len(fs.started) != 3 {
		t.Fatalf("Provisioner.Apply should have started all 3 resident units, got %v", fs.started)
	}
	_ = db
}

type fakeVoiceSystemd struct {
	started, stopped, restarts []string
}

func (f *fakeVoiceSystemd) Start(_ context.Context, unit string) error {
	f.started = append(f.started, unit)
	return nil
}
func (f *fakeVoiceSystemd) Stop(_ context.Context, unit string) error {
	f.stopped = append(f.stopped, unit)
	return nil
}
func (f *fakeVoiceSystemd) Restart(_ context.Context, unit string) error {
	f.restarts = append(f.restarts, unit)
	return nil
}

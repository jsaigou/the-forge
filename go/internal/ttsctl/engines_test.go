// SPDX-License-Identifier: Apache-2.0

package ttsctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSystemd struct {
	started  []string
	stopped  []string
	restarts []string
}

func (f *fakeSystemd) Start(_ context.Context, unit string) error {
	f.started = append(f.started, unit)
	return nil
}
func (f *fakeSystemd) Stop(_ context.Context, unit string) error {
	f.stopped = append(f.stopped, unit)
	return nil
}
func (f *fakeSystemd) Restart(_ context.Context, unit string) error {
	f.restarts = append(f.restarts, unit)
	return nil
}

func TestRenderEnv_AllCLINoResidentURL(t *testing.T) {
	cfg := DefaultEngines() // Unit/URL empty per the two-layer rule
	env := renderEnv(cfg)
	if !strings.Contains(env, "FORGE_TTS_INFERENCE=cli") {
		t.Fatalf("env = %q, want cli inference when no engine has a resident URL configured", env)
	}
	if strings.Contains(env, "FORGE_TTS_SERVER_") {
		t.Fatalf("env = %q, should not set any FORGE_TTS_SERVER_* without a configured URL", env)
	}
}

func TestRenderEnv_ResidentWithURL(t *testing.T) {
	cfg := Engines{
		Kokoro:      EngineConfig{Enabled: true, Mode: ModeResident, Unit: "kokoro", URL: "http://127.0.0.1:8880"},
		CustomVoice: EngineConfig{Enabled: true, Mode: ModeResident, Unit: "forge-tts-custom", URL: "http://127.0.0.1:8091"},
		VoiceDesign: EngineConfig{Enabled: true, Mode: ModeAvailable},
		Base:        EngineConfig{Enabled: true, Mode: ModeResident, Unit: "forge-tts-base", URL: "http://127.0.0.1:8093"},
	}
	env := renderEnv(cfg)
	for _, want := range []string{
		"FORGE_TTS_INFERENCE=server",
		"FORGE_TTS_SERVER_CUSTOM=http://127.0.0.1:8091",
		"FORGE_TTS_SERVER_BASE=http://127.0.0.1:8093",
		"FORGE_TTS_KOKORO_URL=http://127.0.0.1:8880",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env = %q, missing %q", env, want)
		}
	}
	if strings.Contains(env, "FORGE_TTS_SERVER_DESIGN") {
		t.Fatalf("env = %q, voicedesign is available (no URL), should not set FORGE_TTS_SERVER_DESIGN", env)
	}
}

func TestRenderEnv_DisabledEngines(t *testing.T) {
	cfg := Engines{
		Kokoro:      EngineConfig{Enabled: false, Mode: ModeDisabled},
		CustomVoice: EngineConfig{Enabled: true, Mode: ModeResident},
		VoiceDesign: EngineConfig{Enabled: false, Mode: ModeDisabled},
		Base:        EngineConfig{Enabled: true, Mode: ModeAvailable},
	}
	env := renderEnv(cfg)
	if !strings.Contains(env, "FORGE_TTS_KOKORO_ENABLED=false") {
		t.Fatalf("env = %q, want kokoro disabled", env)
	}
	if !strings.Contains(env, "FORGE_TTS_DISABLED_MODES=voicedesign\n") {
		t.Fatalf("env = %q, want the disabled-modes list to contain exactly voicedesign (customvoice/base are enabled)", env)
	}
}

func TestProvisioner_Apply_StartsAndStopsByMode(t *testing.T) {
	fs := &fakeSystemd{}
	p := &Provisioner{Systemd: fs, EnvDir: t.TempDir()}
	cfg := Engines{
		Kokoro:      EngineConfig{Enabled: true, Mode: ModeResident, Unit: "kokoro"},
		CustomVoice: EngineConfig{Enabled: true, Mode: ModeAvailable, Unit: "forge-tts-custom"},
		VoiceDesign: EngineConfig{Enabled: false, Mode: ModeDisabled, Unit: ""}, // no unit -> untouched
		Base:        EngineConfig{Enabled: false, Mode: ModeDisabled, Unit: "forge-tts-base"},
	}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fs.started) != 1 || fs.started[0] != "kokoro" {
		t.Fatalf("started = %v, want just [kokoro] (only resident engine with a unit)", fs.started)
	}
	wantStopped := map[string]bool{"forge-tts-custom": true, "forge-tts-base": true}
	if len(fs.stopped) != 2 {
		t.Fatalf("stopped = %v, want exactly 2 (customvoice=available, base=disabled)", fs.stopped)
	}
	for _, u := range fs.stopped {
		if !wantStopped[u] {
			t.Fatalf("stopped %q unexpectedly", u)
		}
	}
	if len(fs.restarts) != 1 || fs.restarts[0] != "forge-tts" {
		t.Fatalf("restarts = %v, want [forge-tts]", fs.restarts)
	}

	// Env file actually landed on disk.
	data, err := os.ReadFile(filepath.Join(p.EnvDir, "forge-tts.env"))
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(data), "base") || !strings.Contains(string(data), "FORGE_TTS_DISABLED_MODES=") {
		t.Fatalf("env file = %q, want base listed as disabled", data)
	}
}

// fakeSystemdWithFailure fails Start for exactly one unit, simulating a
// missing polkit grant (e.g. kokoro.service, which the existing forge-*
// glob doesn't cover) — found live 2026-08-27 on ForgeHost.
type fakeSystemdWithFailure struct {
	fakeSystemd
	failStartUnit string
}

func (f *fakeSystemdWithFailure) Start(ctx context.Context, unit string) error {
	if unit == f.failStartUnit {
		return errors.New("Access denied as the requested operation requires interactive authentication")
	}
	return f.fakeSystemd.Start(ctx, unit)
}

func TestProvisioner_Apply_OneUnitFailureDoesNotBlockTheRest(t *testing.T) {
	fs := &fakeSystemdWithFailure{failStartUnit: "kokoro"}
	p := &Provisioner{Systemd: fs, EnvDir: t.TempDir()}
	cfg := Engines{
		Kokoro:      EngineConfig{Enabled: true, Mode: ModeResident, Unit: "kokoro"},
		CustomVoice: EngineConfig{Enabled: true, Mode: ModeResident, Unit: "forge-tts-custom"},
		VoiceDesign: EngineConfig{Enabled: true, Mode: ModeAvailable},
		Base:        EngineConfig{Enabled: true, Mode: ModeResident, Unit: "forge-tts-base"},
	}
	err := p.Apply(context.Background(), cfg)
	if err == nil {
		t.Fatalf("Apply should report the kokoro failure, got nil error")
	}
	if !strings.Contains(err.Error(), "kokoro") {
		t.Fatalf("error = %v, want it to mention kokoro", err)
	}
	// The other two resident units and forge-tts's own restart must still
	// have been attempted despite kokoro's failure.
	wantStarted := map[string]bool{"forge-tts-custom": true, "forge-tts-base": true}
	if len(fs.started) != len(wantStarted) {
		t.Fatalf("started = %v, want exactly %v (kokoro's failed Start doesn't count as started)", fs.started, wantStarted)
	}
	for _, u := range fs.started {
		if !wantStarted[u] {
			t.Fatalf("unexpected unit started: %q", u)
		}
	}
	if len(fs.restarts) != 1 || fs.restarts[0] != "forge-tts" {
		t.Fatalf("restarts = %v, want [forge-tts] (must still restart despite the kokoro failure)", fs.restarts)
	}
}

func TestEngines_WatchedUnits(t *testing.T) {
	cfg := Engines{
		Kokoro:      EngineConfig{Unit: "kokoro"},
		CustomVoice: EngineConfig{Unit: "forge-tts-custom"},
		VoiceDesign: EngineConfig{Unit: ""},
		Base:        EngineConfig{Unit: "forge-tts-base"},
	}
	units := cfg.WatchedUnits()
	if len(units) != 3 {
		t.Fatalf("units = %v, want 3 (empty voicedesign unit skipped)", units)
	}
}

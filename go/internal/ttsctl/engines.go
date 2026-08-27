// SPDX-License-Identifier: Apache-2.0

// Package ttsctl provisions the forge-tts voice/speech stack from the
// tts.engines settings key (Sprint 2, Voice & Speech settings, 2026-08-27).
// forge-tts is a standalone binary (go/cmd/forge-tts) configured purely by
// process env — nothing in the daemon wrote to it before this package. This
// mirrors internal/compressorctl's provisioning shape exactly: the daemon
// (still running as testuser, no privilege escalation) writes an env file to a
// testuser-writable directory and start/stop/restarts the affected systemd units
// over the existing D-Bus adapter. Unlike compressorctl there is no systemd
// template here — forge-tts is one fixed, already-installed unit — so this
// package does not author or template any unit file either.
package ttsctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsaigou/the-forge/internal/tts"
)

// EngineMode is how much a TTS engine variant participates.
type EngineMode string

const (
	// ModeResident keeps the engine's systemd unit running continuously
	// (fast, GPU-resident synthesis; costs standing GPU memory).
	ModeResident EngineMode = "resident"
	// ModeAvailable means the engine still serves requests but with no
	// standing process — for the three Qwen sub-variants this is the
	// existing CLI-reload-per-call fallback (internal/tts/backend_cli.go),
	// already built and requiring no code change here. Kokoro has no such
	// fallback tier of its own; ModeAvailable behaves identically to
	// ModeResident for it (both mean "keep kokoro.service running") — the
	// distinction only has teeth for the Qwen sub-variants.
	ModeAvailable EngineMode = "available"
	// ModeDisabled refuses the mode/engine outright (tts.QwenTTS.Disabled
	// for the three Qwen sub-variants; omitting the Kokoro backend entirely
	// for kokoro) and stops its systemd unit if one is configured.
	ModeDisabled EngineMode = "disabled"
)

// EngineConfig is one engine's settings-store entry. Unit/URL are per-install
// operator data (systemd unit names and localhost ports are environment
// facts, never product knowledge) — the two-layer rule (docs/v5-ops-sprints-
// 2026-08-21.md "Ground rules" #1) ships these empty; an install with no
// resident unit for a mode simply behaves as ModeAvailable regardless of
// what's configured, since there's nothing to start.
type EngineConfig struct {
	Enabled bool       `json:"enabled"`
	Mode    EngineMode `json:"mode"`
	Unit    string     `json:"unit"` // systemd unit to start/stop; "" = daemon-unmanaged
	URL     string     `json:"url"`  // resident backend URL; "" = not configured
}

// Engines is the tts.engines settings-store JSON shape. Keyed by name rather
// than a map so the four slots are fixed and self-documenting; CustomVoice/
// VoiceDesign/Base match tts.VoiceMode's constants exactly (ModeCustomVoice/
// ModeVoiceDesign/ModeBase) — Kokoro isn't a VoiceMode (dual_engine routes to
// it by voice-id namespace, not through QwenTTS at all), so it gets its own
// field rather than forcing a fifth VoiceMode value into existence.
type Engines struct {
	Kokoro      EngineConfig `json:"kokoro"`
	CustomVoice EngineConfig `json:"customvoice"`
	VoiceDesign EngineConfig `json:"voicedesign"`
	Base        EngineConfig `json:"base"`
}

// DefaultEngines mirrors the real, already-deployed ForgeHost topology as of
// this sprint (kokoro + customvoice + base resident, voicedesign available
// via CLI since no resident unit for it has ever been deployed) — but,
// per the two-layer rule, only the enabled/mode shape ships; Unit/URL fields
// always start empty and are filled in by the operator (Settings -> Voice &
// Speech, or `forge smith import-local`-style provisioning) per install.
func DefaultEngines() Engines {
	return Engines{
		Kokoro:      EngineConfig{Enabled: true, Mode: ModeResident},
		CustomVoice: EngineConfig{Enabled: true, Mode: ModeResident},
		VoiceDesign: EngineConfig{Enabled: true, Mode: ModeAvailable},
		Base:        EngineConfig{Enabled: true, Mode: ModeResident},
	}
}

// allUnits returns every configured engine's unit name (skipping engines
// with no unit set), for the daemon's watched-unit list — closing the
// Sprint 0 blind spot durably (the collector's restart-loop alert only
// fires for units it actually probes).
func (e Engines) allUnits() []string {
	var units []string
	for _, eng := range []EngineConfig{e.Kokoro, e.CustomVoice, e.VoiceDesign, e.Base} {
		if eng.Unit != "" {
			units = append(units, eng.Unit)
		}
	}
	return units
}

// WatchedUnits is allUnits, exported for the daemon's extraUnits() seam.
func (e Engines) WatchedUnits() []string { return e.allUnits() }

// wantResident reports whether eng should have its systemd unit running —
// true only for an enabled engine in ModeResident with a configured unit
// (nothing to start otherwise).
func wantResident(eng EngineConfig) bool {
	return eng.Enabled && eng.Mode == ModeResident && eng.Unit != ""
}

// renderEnv builds forge-tts's env file content from cfg (see
// go/cmd/forge-tts/main.go's env-var doc comment for the full contract).
// FORGE_TTS_INFERENCE is "server" iff at least one Qwen sub-variant is both
// resident and has a URL configured (nothing to proxy to otherwise); disabled
// modes are collected into one FORGE_TTS_DISABLED_MODES list, matching
// tts.VoiceMode strings exactly so cmd/forge-tts's parseDisabledModes needs
// no translation table.
func renderEnv(cfg Engines) string {
	var b strings.Builder
	qwenModes := []struct {
		mode tts.VoiceMode
		eng  EngineConfig
		env  string
	}{
		{tts.ModeCustomVoice, cfg.CustomVoice, "FORGE_TTS_SERVER_CUSTOM"},
		{tts.ModeVoiceDesign, cfg.VoiceDesign, "FORGE_TTS_SERVER_DESIGN"},
		{tts.ModeBase, cfg.Base, "FORGE_TTS_SERVER_BASE"},
	}

	anyResident := false
	for _, m := range qwenModes {
		if wantResident(m.eng) && m.eng.URL != "" {
			anyResident = true
			break
		}
	}
	if anyResident {
		fmt.Fprintln(&b, "FORGE_TTS_INFERENCE=server")
		for _, m := range qwenModes {
			if wantResident(m.eng) && m.eng.URL != "" {
				fmt.Fprintf(&b, "%s=%s\n", m.env, m.eng.URL)
			}
		}
	} else {
		fmt.Fprintln(&b, "FORGE_TTS_INFERENCE=cli")
	}

	if !cfg.Kokoro.Enabled || cfg.Kokoro.Mode == ModeDisabled {
		fmt.Fprintln(&b, "FORGE_TTS_KOKORO_ENABLED=false")
	} else if cfg.Kokoro.URL != "" {
		fmt.Fprintf(&b, "FORGE_TTS_KOKORO_URL=%s\n", cfg.Kokoro.URL)
	}

	var disabledModes []string
	for _, m := range qwenModes {
		if !m.eng.Enabled || m.eng.Mode == ModeDisabled {
			disabledModes = append(disabledModes, string(m.mode))
		}
	}
	if len(disabledModes) > 0 {
		fmt.Fprintf(&b, "FORGE_TTS_DISABLED_MODES=%s\n", strings.Join(disabledModes, ","))
	}
	return b.String()
}

// Systemd is the subset of engine.Systemd this package needs.
type Systemd interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
}

// Provisioner writes forge-tts's env file and starts/stops the resident
// sub-engine units to match a desired Engines config.
type Provisioner struct {
	Systemd Systemd
	// EnvDir is the testuser-writable directory backing forge-tts.service's
	// EnvironmentFile=-<EnvDir>/forge-tts.env (e.g. /var/lib/forge/tts).
	EnvDir string
	// Unit is the main forge-tts process unit ("forge-tts" default).
	Unit string
}

func (p *Provisioner) unit() string {
	if p.Unit == "" {
		return "forge-tts"
	}
	return p.Unit
}

func (p *Provisioner) envPath() string {
	return filepath.Join(p.EnvDir, "forge-tts.env")
}

func (p *Provisioner) writeEnv(cfg Engines) error {
	if err := os.MkdirAll(p.EnvDir, 0o700); err != nil {
		return fmt.Errorf("ttsctl: env dir: %w", err)
	}
	if err := os.WriteFile(p.envPath(), []byte(renderEnv(cfg)), 0o644); err != nil {
		return fmt.Errorf("ttsctl: write env: %w", err)
	}
	return nil
}

// Apply writes cfg's env file, starts/stops each configured engine unit to
// match its desired residency, then restarts forge-tts itself so it picks
// up the rewritten env. Engine units are synced before the forge-tts
// restart so a freshly-started resident backend has a chance to come up
// before forge-tts's own restart tries to reach it (best-effort — a
// slow-starting model load can still race this on a cold instance; the
// server backend's existing per-call fallback to CLI covers that window).
//
// Every unit is attempted even if an earlier one fails — found live
// (2026-08-27): the daemon's existing polkit grant is a `forge-*.service`
// glob, which covers forge-tts itself and forge-tts-custom/-base, but NOT a
// non-forge-named unit like kokoro.service. An operator who configures
// kokoro's unit field without a matching polkit rule for it must still get
// forge-tts's own restart and every OTHER engine's start/stop applied —
// aborting the whole call on the first D-Bus permission error would silently
// leave the rest of a valid config never applied. Every failure is
// collected and returned together via errors.Join; the caller (PUT
// /api/v1/voice/settings) surfaces it as a "saved, but..." warning rather
// than treating the whole apply as having not happened.
func (p *Provisioner) Apply(ctx context.Context, cfg Engines) error {
	if err := p.writeEnv(cfg); err != nil {
		return err
	}
	var errs []error
	for _, eng := range []EngineConfig{cfg.Kokoro, cfg.CustomVoice, cfg.VoiceDesign, cfg.Base} {
		if eng.Unit == "" {
			continue
		}
		if wantResident(eng) {
			if err := p.Systemd.Start(ctx, eng.Unit); err != nil {
				errs = append(errs, fmt.Errorf("ttsctl: start %s: %w", eng.Unit, err))
			}
		} else {
			if err := p.Systemd.Stop(ctx, eng.Unit); err != nil {
				errs = append(errs, fmt.Errorf("ttsctl: stop %s: %w", eng.Unit, err))
			}
		}
	}
	if err := p.Systemd.Restart(ctx, p.unit()); err != nil {
		errs = append(errs, fmt.Errorf("ttsctl: restart %s: %w", p.unit(), err))
	}
	return errors.Join(errs...)
}

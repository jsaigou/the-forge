// SPDX-License-Identifier: Apache-2.0

package httpapi

// voice_handlers.go — Tier 1 Sprint 2, Voice & Speech settings (2026-08-27).
// GET/PUT /api/v1/voice/settings against the single `tts.engines` settings
// key (go/internal/ttsctl.Engines). Unlike infra_handlers.go's merge-partial
// pattern, this always reads/writes the whole Engines object — the
// frontend's useSettingsGroup convention already submits everything it
// displayed on every save (same whole-object-replace-when-present rule
// smith_chat.go's settings groups use), so there's no partial-field-loss
// risk to guard against here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/ttsctl"
)

const settingTTSEngines = "tts.engines"

// GET/PUT both use the bare ttsctl.Engines shape at the top level — no
// wrapper object — matching monitorSettings/metricsSettings' existing
// unwrapped convention (api.ts's `get<MonitorSettings>(...)`).
func (s *Server) handleVoiceSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.readVoiceEngines(r))
}

// readVoiceEngines reads tts.engines, falling back to
// ttsctl.DefaultEngines() for any field the stored JSON doesn't set
// (including "never set at all").
func (s *Server) readVoiceEngines(r *http.Request) ttsctl.Engines {
	cfg := ttsctl.DefaultEngines()
	if raw := s.getRawSetting(r.Context(), settingTTSEngines); len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg
}

// voiceTypeToEngine maps forge-tts's per-voice Type (set when a voice is
// registered — go/internal/tts/tts.go's VoiceEntry) onto the engine key this
// repo's tts.engines settings and VOICE_FIELDS use. "clone"/"design" don't
// share a name with their engine ("base"/"voicedesign") on the wire, so this
// mapping can't just be the identity function.
var voiceTypeToEngine = map[string]string{
	"kokoro":      "kokoro",
	"customvoice": "customvoice",
	"design":      "voicedesign",
	"clone":       "base",
}

type voiceListEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Engine     string `json:"engine"`
	Language   string `json:"language"`
	Tier       string `json:"tier,omitempty"`
	Builtin    bool   `json:"builtin"`
	HasSample  bool   `json:"has_sample"`
	SampleText string `json:"sample_text,omitempty"`
}

// handleVoiceList — GET /api/v1/voice/list (operator, read-only). Fetches
// forge-tts's live voice registry through the same router.tts_url setting
// the a0 passthrough (GET /v1/voices on the router) already uses, and adds
// an `engine` field per voice (see voiceTypeToEngine) so the Settings UI can
// group the flat list by engine without duplicating forge-tts's Type
// taxonomy on the frontend.
func (s *Server) handleVoiceList(w http.ResponseWriter, r *http.Request) {
	ttsURL := s.resolvedRouterConfig(r.Context()).TTSURL
	if ttsURL == "" {
		writeError(w, http.StatusServiceUnavailable, "TTS not configured (set router.tts_url under Settings > Routing)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	upstream := strings.TrimRight(ttsURL, "/") + "/voices"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("forge-tts unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("forge-tts returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}

	var upstreamResp struct {
		Voices []voiceListEntry `json:"voices"`
	}
	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		writeError(w, http.StatusBadGateway, "forge-tts returned an unexpected voice-list shape")
		return
	}
	for i := range upstreamResp.Voices {
		if eng, ok := voiceTypeToEngine[upstreamResp.Voices[i].Type]; ok {
			upstreamResp.Voices[i].Engine = eng
		} else {
			upstreamResp.Voices[i].Engine = upstreamResp.Voices[i].Type
		}
	}
	writeJSON(w, http.StatusOK, upstreamResp)
}

func validateEngineMode(mode ttsctl.EngineMode, field string, fields map[string]string) {
	switch mode {
	case ttsctl.ModeResident, ttsctl.ModeAvailable, ttsctl.ModeDisabled:
	default:
		fields[field] = "must be one of resident, available, disabled"
	}
}

func validateEngineUnit(unit, field string, fields map[string]string) {
	if unit != "" && !serviceRE.MatchString(unit) {
		fields[field] = "must be a lowercase systemd-unit-shaped name (a-z0-9_- , starting with a letter), or empty"
	}
}

func validateEngineURL(rawURL, field string, fields map[string]string) {
	if rawURL == "" {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		fields[field] = "must be an absolute http(s) URL, or empty"
	}
}

// handleVoiceSettingsPut validates the entire submitted Engines object
// before writing any of it, then — if a Provisioner is wired — applies it
// live (env file + start/stop the configured units + restart forge-tts)
// synchronously, mirroring how compressor_handlers.go calls
// CompressorProvisioner.Provision directly rather than backgrounding it: the
// operator submitting this form wants to know it actually took effect, not
// a fire-and-forget 200.
func (s *Server) handleVoiceSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body ttsctl.Engines
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	fields := map[string]string{}
	for name, eng := range map[string]ttsctl.EngineConfig{
		"kokoro": body.Kokoro, "customvoice": body.CustomVoice,
		"voicedesign": body.VoiceDesign, "base": body.Base,
	} {
		validateEngineMode(eng.Mode, name+".mode", fields)
		validateEngineUnit(eng.Unit, name+".unit", fields)
		validateEngineURL(eng.URL, name+".url", fields)
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	raw, err := json.Marshal(body)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(r.Context(), settingTTSEngines, raw); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "voice_settings_update", "tts.engines", "")

	if s.deps.TTSProvisioner != nil {
		if err := s.deps.TTSProvisioner.Apply(r.Context(), body); err != nil {
			// The setting is already persisted (source of truth for the next
			// successful apply/restart) — surface the live-apply failure
			// distinctly rather than a bare 500, so an operator knows the
			// setting saved but didn't (yet) take effect on the host.
			writeJSON(w, http.StatusOK, map[string]any{
				"kokoro":      body.Kokoro,
				"customvoice": body.CustomVoice,
				"voicedesign": body.VoiceDesign,
				"base":        body.Base,
				"warning":     "settings saved, but applying them live failed: " + err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

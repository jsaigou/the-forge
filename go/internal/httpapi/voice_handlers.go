// SPDX-License-Identifier: Apache-2.0

package httpapi

// voice_handlers.go — Voice & Speech: GET /api/v1/voice/list, the saved voices with the kind and engine of each. Settings for the engines
// themselves no longer live here (docs/speech-routing-plan.md section 4.5): a speech engine is a pool occupant (behavior on
// /api/v1/specialists/occupants/{name}/behavior) and the default engine per kind is the tts.defaults setting (/api/v1/speech/defaults).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// voiceTypeToKind maps forge-tts's per-voice Type (set when a voice is registered — go/internal/tts/tts.go's VoiceEntry) onto the kind of speech
// (the speech-engine kinds: kokoro, customvoice, voicedesign, base). "clone"/"design" don't share a name with their kind on the wire, so this
// mapping can't just be the identity function.
var voiceTypeToKind = map[string]string{
	"kokoro":      "kokoro",
	"customvoice": "customvoice",
	"design":      "voicedesign",
	"clone":       "base",
}

type voiceListEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Kind is the kind of speech engine that can render the voice (kokoro, customvoice, voicedesign or base), derived from Type.
	Kind string `json:"kind"`
	// Engine is the name of the speech engine the voice is bound to, as forge-tts reports it. Empty when the voice has none, which means the
	// default engine for its kind applies.
	Engine     string `json:"engine"`
	Language   string `json:"language"`
	Tier       string `json:"tier,omitempty"`
	Builtin    bool   `json:"builtin"`
	HasSample  bool   `json:"has_sample"`
	SampleText string `json:"sample_text,omitempty"`
}

// handleVoiceList — GET /api/v1/voice/list (operator, read-only). Fetches forge-tts's live voice registry through the same router.tts_url
// setting the a0 passthrough (GET /v1/voices on the router) already uses, and adds a `kind` per voice (see voiceTypeToKind) so the Settings UI
// can group the flat list by kind without duplicating forge-tts's Type taxonomy on the frontend. `engine` passes through as forge-tts reports it.
func (s *Server) handleVoiceList(w http.ResponseWriter, r *http.Request) {
	ttsURL := s.resolvedRouterConfig(r.Context()).TTSURL
	if ttsURL == "" {
		writeErrorCode(w, http.StatusServiceUnavailable, "tts_not_configured", nil, "TTS not configured (set router.tts_url under Settings > Routing)")
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
		writeErrorCode(w, http.StatusBadGateway, "tts_upstream_failed", map[string]any{"detail": fmt.Sprintf("forge-tts unreachable: %v", err)}, fmt.Sprintf("forge-tts unreachable: %v", err))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeErrorCode(w, http.StatusBadGateway, "tts_upstream_failed", map[string]any{"detail": fmt.Sprintf("forge-tts returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))}, fmt.Sprintf("forge-tts returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}

	var upstreamResp struct {
		Voices []voiceListEntry `json:"voices"`
	}
	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		writeErrorCode(w, http.StatusBadGateway, "tts_upstream_failed", map[string]any{"detail": "forge-tts returned an unexpected voice-list shape"}, "forge-tts returned an unexpected voice-list shape")
		return
	}
	for i := range upstreamResp.Voices {
		if kind, ok := voiceTypeToKind[upstreamResp.Voices[i].Type]; ok {
			upstreamResp.Voices[i].Kind = kind
		} else {
			upstreamResp.Voices[i].Kind = upstreamResp.Voices[i].Type
		}
	}
	writeJSON(w, http.StatusOK, upstreamResp)
}

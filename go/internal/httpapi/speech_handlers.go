// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// Speech engine resolution (docs/speech-routing-plan.md, P1). forge-tts asks per request which engine renders a kind of speech and gets the engine's
// address and a lease; it gives the lease back when the response has been sent. These two calls are all a speech-scoped key may reach.

// SpeechPool is the part of the pool the speech endpoints use. *specialist.Manager implements it; the server asks the pool for it with a type
// assertion so the SpecialistPool interface and its fakes stay as they are.
type SpeechPool interface {
	ResolveSpeech(ctx context.Context, req specialist.SpeechRequest) (specialist.SpeechLease, error)
	ReleaseSpeech(id string) bool
}

func (s *Server) speechPool(w http.ResponseWriter) (SpeechPool, bool) {
	if s.deps.Specialists != nil {
		if sp, ok := s.deps.Specialists.(SpeechPool); ok {
			return sp, true
		}
	}
	writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "speech engine pool"}, "the speech engine pool is not running on this server")
	return nil, false
}

// speechResolveBudget bounds one resolve, an engine start included (the longest health timeout of a launch kind is 120 s).
const speechResolveBudget = 3 * time.Minute

type speechResolveBody struct {
	Mode   string `json:"mode"`
	Engine string `json:"engine"`
}

// handleSpeechResolve picks the engine for one speech request, loads it if needed and takes a lease on it.
func (s *Server) handleSpeechResolve(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.speechPool(w)
	if !ok {
		return
	}
	var b speechResolveBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if !specialist.ValidSpeechMode(b.Mode) {
		writeValidationErrorCodes(w, map[string]string{"mode": "must be one of base, customvoice, voicedesign, kokoro"}, map[string]string{"mode": "must_be_one_of"})
		return
	}
	if len(b.Engine) > 128 {
		writeValidationErrorCodes(w, map[string]string{"engine": "too long (at most 128 characters)"}, map[string]string{"engine": "too_large"})
		return
	}
	// The engine may have to be started, which can take longer than the caller is willing to wait. The start runs on its own context, so a caller that
	// gives up (forge-tts timing out, a client disconnecting) abandons the wait, not the load: cancelling it would kill a nearly finished start and,
	// for an always-on engine, count as a failed one and begin the restart backoff.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), speechResolveBudget)
	defer cancel()
	l, err := sp.ResolveSpeech(ctx, specialist.SpeechRequest{Mode: b.Mode, Engine: b.Engine})
	if err == nil && r.Context().Err() != nil {
		// Nobody is left to use the lease: give it back at once instead of pinning the engine for its whole life.
		sp.ReleaseSpeech(l.ID)
		return
	}
	if err != nil {
		// A cold start and a hard failure (nothing can play this voice, an engine is off) are worth an audit row. A transient failure (still loading, pool
		// full, caller gone) and a warm hit are not: a retry loop would otherwise write a row per attempt.
		if isHardSpeechError(err) {
			s.audit(r, identity(r).Name, "speech_resolve_failed", b.Mode+" "+b.Engine, err.Error())
		}
		s.writeSpeechError(w, err)
		return
	}
	if l.Loaded {
		s.audit(r, identity(r).Name, "speech_resolve_loaded", l.Engine, b.Mode)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": l.Engine, "mode": l.Mode, "kind": l.Kind, "slot": l.Slot, "base_url": l.BaseURL, "lease_id": l.ID,
		"expires_at": isoFormat(l.Expires), "loaded": l.Loaded,
	})
}

type speechReleaseBody struct {
	LeaseID string `json:"lease_id"`
}

// handleSpeechRelease gives a lease back. It is idempotent: an unknown or already released lease is not an error, because the caller has nothing to do
// about it and a lease also expires by itself.
func (s *Server) handleSpeechRelease(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.speechPool(w)
	if !ok {
		return
	}
	var b speechReleaseBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.LeaseID == "" {
		writeValidationErrorCodes(w, map[string]string{"lease_id": "required"}, map[string]string{"lease_id": "required"})
		return
	}
	sp.ReleaseSpeech(b.LeaseID)
	w.WriteHeader(http.StatusNoContent)
}

// isHardSpeechError reports whether retrying cannot change the answer (nothing can play the voice, or the engine is turned off or stopped).
func isHardSpeechError(err error) bool {
	var none *specialist.NoSpeechEngineError
	var unavail *specialist.UnavailableError
	if errors.As(err, &none) || errors.Is(err, specialist.ErrNotFound) {
		return true
	}
	return errors.As(err, &unavail) && (unavail.Reason == specialist.ReasonDisabled || unavail.Reason == specialist.ReasonStopped)
}

// writeSpeechError maps the pool's errors onto stable codes. The split that matters to the caller is transient (loading, stopping, waiting for a slot,
// a request that was cancelled or timed out: try again shortly) versus hard (disabled, stopped, no such engine: retrying changes nothing), so
// `transient` is in the body as well as in the code. Nothing here is a plain 500 unless it really is an internal failure.
func (s *Server) writeSpeechError(w http.ResponseWriter, err error) {
	var full *specialist.PoolFullError
	var unavail *specialist.UnavailableError
	var none *specialist.NoSpeechEngineError
	switch {
	case errors.As(err, &none):
		writeErrorCode(w, http.StatusServiceUnavailable, "speech_no_engine",
			map[string]any{"mode": none.Mode, "engine": none.Engine, "reason": none.Reason, "transient": false}, err.Error())
	case errors.Is(err, specialist.ErrNotFound):
		// The engine was removed between choosing it and starting it.
		writeErrorCode(w, http.StatusServiceUnavailable, "speech_no_engine",
			map[string]any{"reason": "the engine was removed", "transient": false}, err.Error())
	case errors.As(err, &unavail) && unavail.Reason == specialist.ReasonDisabled:
		writeErrorCode(w, http.StatusServiceUnavailable, "speech_engine_disabled", map[string]any{"engine": unavail.Occupant, "transient": false}, err.Error())
	case errors.As(err, &unavail) && unavail.Reason == specialist.ReasonStopped:
		writeErrorCode(w, http.StatusServiceUnavailable, "speech_engine_stopped", map[string]any{"engine": unavail.Occupant, "transient": false}, err.Error())
	case errors.Is(err, specialist.ErrTooManyLeases):
		w.Header().Set("Retry-After", "2")
		writeErrorCode(w, http.StatusServiceUnavailable, "speech_lease_limit", map[string]any{"transient": true}, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller gave up, or the engine took longer than the request was allowed: the engine may well be up a moment later.
		w.Header().Set("Retry-After", "1")
		writeErrorCode(w, http.StatusServiceUnavailable, "specialist_unavailable",
			map[string]any{"reason": "the request was cancelled or took too long while the engine was starting", "transient": true}, err.Error())
	case errors.As(err, &full), errors.As(err, &unavail):
		s.writeSpecialistError(w, err) // specialist_pool_full / specialist_unavailable: transient
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
	}
}

// speechDefaultsJSON is the answer of GET /api/v1/speech/defaults: the stored defaults, and for each kind the engines that could be chosen.
type speechDefaultsJSON struct {
	Defaults map[string]string   `json:"defaults"`
	Modes    []string            `json:"modes"`
	Engines  map[string][]string `json:"engines"`
	// EngineInfo describes every speech engine for a caller that offers a choice (forge-tts' GET /v1/engines): name, label, kind, whether it may be used and
	// what it is doing now. Names and states only; nothing in it is a secret.
	EngineInfo []speechEngineInfo `json:"engine_info"`
}

type speechEngineInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Mode    string `json:"mode"`
	Policy  string `json:"policy"`
	Enabled bool   `json:"enabled"`
	Stopped bool   `json:"stopped"`
	// State is the engine's slot state (running, starting, failed, ...) or "idle" while it is not loaded.
	State string `json:"state"`
}

func (s *Server) readSpeechDefaults(ctx context.Context) (map[string]string, error) {
	if s.deps.Settings == nil {
		return map[string]string{}, nil
	}
	raw, err := s.deps.Settings.Get(ctx, specialist.SpeechDefaultsKey)
	if errors.Is(err, store.ErrNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err // a real read failure is an error, not an empty answer
	}
	return specialist.ParseSpeechDefaults(raw)
}

// speechEnginesByMode groups the registered speech engines by the kind of speech they render, enabled or not.
func (s *Server) speechEnginesByMode(ctx context.Context) (map[string][]store.Occupant, error) {
	list, err := s.deps.SpecialistStore.ListOccupants(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]store.Occupant{}
	for _, o := range list {
		if mode, ok := specialist.SpeechModeOf(o); ok {
			out[mode] = append(out[mode], o)
		}
	}
	for _, v := range out {
		sort.Slice(v, func(i, j int) bool { return v[i].Name < v[j].Name })
	}
	return out, nil
}

func (s *Server) handleSpeechDefaultsGet(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	defaults, err := s.readSpeechDefaults(r.Context())
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	byMode, err := s.speechEnginesByMode(r.Context())
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	out := speechDefaultsJSON{Defaults: defaults, Modes: specialist.SpeechModes, Engines: map[string][]string{}}
	states := map[string]string{}
	if s.deps.Specialists != nil {
		for _, st := range s.deps.Specialists.Snapshot(r.Context()) {
			if st.Occupant != "" {
				states[st.Occupant] = st.State
			}
		}
	}
	out.EngineInfo = []speechEngineInfo{}
	for _, m := range specialist.SpeechModes {
		names := []string{}
		for _, o := range byMode[m] {
			names = append(names, o.Name)
			st := states[o.Name]
			if st == "" {
				st = "idle"
			}
			out.EngineInfo = append(out.EngineInfo, speechEngineInfo{Name: o.Name, Label: o.Label, Mode: m, Policy: o.Policy, Enabled: o.Enabled, Stopped: o.Stopped, State: st})
		}
		out.Engines[m] = names
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSpeechDefaultsPut replaces the defaults. Each default must be a registered speech engine of that very kind; an empty value clears a kind.
func (s *Server) handleSpeechDefaultsPut(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	if s.deps.Settings == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "settings store"}, "settings store not wired")
		return
	}
	var in map[string]string
	if fields := decodeJSONBody(r, &in); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if in == nil {
		// An empty body or `null` is almost certainly a mistake, and the call replaces every default: clearing them takes an explicit {}.
		writeValidationErrorCodes(w, map[string]string{"body": "a JSON object is required (send {} to clear every default)"}, map[string]string{"body": "required"})
		return
	}
	byMode, err := s.speechEnginesByMode(r.Context())
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	fields, codes := map[string]string{}, map[string]string{}
	clean := map[string]string{}
	for mode, name := range in {
		if !specialist.ValidSpeechMode(mode) {
			fields[mode] = "not a kind of speech (base, customvoice, voicedesign, kokoro)"
			codes[mode] = "must_be_one_of"
			continue
		}
		if name == "" {
			continue
		}
		found := false
		for _, o := range byMode[mode] {
			if o.Name == name {
				found = true
			}
		}
		if !found {
			fields[mode] = name + " is not a registered " + mode + " speech engine"
			codes[mode] = "not_found"
			continue
		}
		clean[mode] = name
	}
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(r.Context(), specialist.SpeechDefaultsKey, raw); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "speech_defaults_update", specialist.SpeechDefaultsKey, string(raw))
	writeJSON(w, http.StatusOK, map[string]any{"defaults": clean})
}

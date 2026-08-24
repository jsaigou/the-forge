// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jsaigou/the-forge/internal/profile"
)

// profileRunBody is the POST /api/v1/profile/run request body.
type profileRunBody struct {
	Mode string `json:"mode"`
}

func (b profileRunBody) validate() (profile.RunRequest, map[string]string) {
	fields := map[string]string{}
	if b.Mode == "" {
		fields["mode"] = "required"
	}
	return profile.RunRequest{Mode: b.Mode}, fields
}

// handleProfileRun starts a profile run (PROFILE track,
// docs/v5-profiling-benchmarks.md §7). Admin + step-up gated. The run
// executes in a background goroutine (it takes minutes: evict → load → fill
// → measure → benchmark → record → unload). Progress streams via the SSE
// bus (profile:started|progress|done|failed). Returns 202 accepted.
func (s *Server) handleProfileRun(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.profileRunner()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "profiling not wired")
		return
	}

	var b profileRunBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	req, fields := b.validate()
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	// Validate the mode exists in config.
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		if _, ok := cfg.Modes[req.Mode]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false,
				"message": fmt.Sprintf("Unknown mode: %s", req.Mode),
			})
			return
		}
	}

	// Refuse if a run is already in progress (disable concurrent run).
	if runner.IsRunning() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": "A profile run is already in progress",
		})
		return
	}

	s.audit(r, "human", "profile_run", req.Mode, "evicts all A1-A4 slots")

	// Trigger an immediate collector probe + status_update push so the
	// Console/Dashboard reflect the about-to-start slot state changes
	// immediately, not on the next collector cadence.
	s.probeAndPush()

	// Execute in background on the server-lifetime context (NOT r.Context,
	// which is cancelled when the handler returns — same pattern as
	// runSwitchBackground).
	goSafe("profile_run", func() {
		_, _ = runner.Run(s.bgCtx, req)
		// After the run finishes, trigger another probe + push so the
		// Dashboard reflects the final unloaded state immediately.
		s.probeAndPush()
	})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"success": true,
		"message": fmt.Sprintf("Profile run started for mode %s (A1-A4 will be evicted)", req.Mode),
	})
}

// handleProfileGet returns the latest stored profile for a mode + its
// staleness (PROFILE track §7).
func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.profileRunner()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "profiling not wired")
		return
	}
	mode := r.PathValue("mode")
	lastErr, lastErrAt, hasErr := runner.LastError(mode)
	result, found := runner.Get(mode)
	if !found {
		resp := map[string]any{
			"mode": mode,
			"config_id": func() int64 {
				if s.deps.Config == nil {
					return 0
				}
				return s.deps.Config().Modes[mode].ConfigID
			}(),
			"profiled": false,
			"stale":    true,
			"running":  runner.IsRunning(),
		}
		if hasErr {
			resp["last_error"] = lastErr
			resp["last_error_at"] = lastErrAt
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp := profileResponse{
		Mode:            result.Mode,
		ConfigID:        result.ConfigID,
		ModelID:         result.ModelID,
		NCtx:            result.NCtx,
		ActualNCtx:      result.ActualNCtx,
		Backend:         result.Backend,
		Parallel:        result.Parallel,
		SafeMemoryMB:    result.SafeMemoryBytes,
		PrefillTPS:      result.PrefillTPS,
		DecodeTPS:       result.DecodeTPS,
		Fingerprint:     result.Fingerprint,
		Stale:           result.Stale,
		Profiled:        !result.Stale,
		MeasuredAt:      result.MeasuredAt,
		Running:         runner.IsRunning(),
		DepthBenchmarks: toDepthBenchmarksJSON(result.DepthBenchmarks),
	}
	if hasErr {
		resp.LastError = lastErr
		resp.LastErrorAt = lastErrAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// depthBenchmarkJSON mirrors profile.DepthBenchmark (product/QA sprint,
// 2026-07-29 — the depth-sweep curve). Index 0 is TYPICAL (same as the
// scalar prefill_tps/decode_tps above); the last entry is WORST CASE. The
// FE shows first+last by default and the full curve behind "Show more".
type depthBenchmarkJSON struct {
	DepthTokens int     `json:"depth_tokens"`
	PP2048TPS   float64 `json:"pp2048_tps"`
	TG128TPS    float64 `json:"tg128_tps"`
}

func toDepthBenchmarksJSON(bs []profile.DepthBenchmark) []depthBenchmarkJSON {
	out := make([]depthBenchmarkJSON, len(bs))
	for i, b := range bs {
		out[i] = depthBenchmarkJSON{DepthTokens: b.DepthTokens, PP2048TPS: b.PP2048TPS, TG128TPS: b.TG128TPS}
	}
	return out
}

// profileResponse is the GET /api/v1/profile/{mode} body.
type profileResponse struct {
	Mode         string  `json:"mode"`
	ConfigID     int64   `json:"config_id"`
	ModelID      string  `json:"model_id"`
	NCtx         int     `json:"n_ctx"`
	ActualNCtx   int     `json:"actual_n_ctx"`
	Backend      string  `json:"backend"`
	Parallel     int     `json:"parallel"`
	SafeMemoryMB int64   `json:"safe_memory_bytes"`
	PrefillTPS   float64 `json:"prefill_tps"`
	DecodeTPS    float64 `json:"decode_tps"`
	Fingerprint  string  `json:"fingerprint"`
	Stale        bool    `json:"stale"`
	Profiled     bool    `json:"profiled"`
	MeasuredAt   int64   `json:"measured_at"`
	Running      bool    `json:"running"`
	// DepthBenchmarks is the full depth-sweep curve (product/QA sprint,
	// 2026-07-29) — see depthBenchmarkJSON's doc comment.
	DepthBenchmarks []depthBenchmarkJSON `json:"depth_benchmarks"`
	// LastError/LastErrorAt are set when the most recent run for this mode
	// failed (and no later run for the same mode has succeeded since). Lets
	// a polling client show the failure reason without depending on the
	// profile:failed SSE event — see docs/v5-profiling-benchmarks.md §10.
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt int64  `json:"last_error_at,omitempty"`
}

// profileRunner returns the profile runner if wired, else false.
func (s *Server) profileRunner() (*profile.Runner, bool) {
	if s.deps.Profiles == nil {
		return nil, false
	}
	return s.deps.Profiles, true
}

// profilesListResponse is the GET /api/v1/profile response (all profiles).
type profilesListResponse struct {
	Profiles []profileListItem `json:"profiles"`
}

type profileListItem struct {
	Mode         string  `json:"mode"`
	ConfigID     int64   `json:"config_id"`
	ModelID      string  `json:"model_id"`
	NCtx         int     `json:"n_ctx"`
	Backend      string  `json:"backend"`
	SafeMemoryMB int64   `json:"safe_memory_bytes"`
	PrefillTPS   float64 `json:"prefill_tps"`
	DecodeTPS    float64 `json:"decode_tps"`
	Stale        bool    `json:"stale"`
	MeasuredAt   int64   `json:"measured_at"`
	// DepthBenchmarks (product/QA sprint, 2026-07-29): included in the list
	// view too (not just the single-mode GET), cheaply — runner.Get already
	// fetches them for the staleness check below. Lets the Settings table
	// show TYPICAL (index 0) + WORST CASE (last index) without an extra
	// per-row request.
	DepthBenchmarks []depthBenchmarkJSON `json:"depth_benchmarks"`
}

// handleProfilesList returns all stored profiles (for a Settings overview).
func (s *Server) handleProfilesList(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.profileRunner()
	if !ok {
		writeJSON(w, http.StatusOK, profilesListResponse{Profiles: []profileListItem{}})
		return
	}
	// Build the list from the store via the Lookup interface.
	// We iterate modes from config so the list includes unprofiled modes too.
	items := []profileListItem{}
	if s.deps.Config != nil {
		cfg := s.deps.Config()
		for modeName := range cfg.Modes {
			mode := cfg.Modes[modeName]
			if mode.Type == "service" || len(mode.Services) == 0 {
				continue
			}
			result, found := runner.Get(modeName)
			if !found {
				// ConfigID here comes from the merged-config seam (mode.ConfigID),
				// not from a stored profile — this is the branch it's easiest to
				// forget, and forgetting it means every unprofiled config falls
				// back to name-joining on the FE, exactly the wart config_id
				// exists to remove.
				items = append(items, profileListItem{Mode: modeName, ConfigID: mode.ConfigID, Stale: true})
				continue
			}
			items = append(items, profileListItem{
				Mode:            result.Mode,
				ConfigID:        result.ConfigID,
				ModelID:         result.ModelID,
				NCtx:            result.NCtx,
				Backend:         result.Backend,
				SafeMemoryMB:    result.SafeMemoryBytes,
				PrefillTPS:      result.PrefillTPS,
				DecodeTPS:       result.DecodeTPS,
				Stale:           result.Stale,
				MeasuredAt:      result.MeasuredAt,
				DepthBenchmarks: toDepthBenchmarksJSON(result.DepthBenchmarks),
			})
		}
	}
	writeJSON(w, http.StatusOK, profilesListResponse{Profiles: items})
}

// probeAndPush triggers an immediate collector poll (when a Prober is wired)
// and then publishes a status_update so the Console/Dashboard reflect the
// current slot state immediately, not on the next collector cadence.
func (s *Server) probeAndPush() {
	if s.deps.Prober != nil {
		s.deps.Prober(context.Background())
	}
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("status_update", s.buildStatusResponse())
	}
}

// handleProfileDelete clears a stored profile (e.g. one measured at the
// wrong n_ctx before the context-reduction guard was added).
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.profileRunner()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "profiling not wired")
		return
	}
	mode := r.PathValue("mode")
	if err := runner.Delete(r.Context(), mode); err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.audit(r, "human", "profile_delete", mode, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

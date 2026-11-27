// SPDX-License-Identifier: Apache-2.0

package httpapi

// profile_night_handlers.go — "Profile tonight" (profile.Night):
//   GET    /api/v1/profile-night  the state, tonight's window and what it would measure
//   POST   /api/v1/profile-night  arm tonight's run
//   DELETE /api/v1/profile-night  cancel an armed run, or stop a running one
// The path is not under /api/v1/profile/ so a config named "night" cannot collide with it.

import (
	"errors"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/profile"
)

type profileNightJSON struct {
	State profile.NightState `json:"state"`
	// Window is the one in progress or the next one.
	Window profileNightWindowJSON `json:"window"`
	// Queue is what a run would measure now, shortest first.
	Queue []profile.NightItem `json:"queue"`
}

type profileNightWindowJSON struct {
	StartsAt int64 `json:"starts_at"`
	EndsAt   int64 `json:"ends_at"`
}

func (s *Server) profileNightResponse(w http.ResponseWriter, status int, st profile.NightState) {
	start, end := profile.NightWindow(time.Now())
	attempted := map[string]bool{}
	if st.Status == profile.NightRunning {
		for _, r := range st.Results {
			attempted[r.Mode] = true
		}
	}
	queue := []profile.NightItem{}
	if s.deps.Profiles != nil {
		queue = profile.Candidates(s.deps.Profiles.Statuses(), attempted)
	}
	writeJSON(w, status, profileNightJSON{State: st, Window: profileNightWindowJSON{StartsAt: start.Unix(), EndsAt: end.Unix()}, Queue: queue})
}

func (s *Server) profileNight(w http.ResponseWriter) (*profile.Night, bool) {
	if s.deps.ProfileNight == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "profiling"}, "profiling not wired")
		return nil, false
	}
	return s.deps.ProfileNight, true
}

func (s *Server) handleProfileNightGet(w http.ResponseWriter, r *http.Request) {
	night, ok := s.profileNight(w)
	if !ok {
		return
	}
	st, err := night.State(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.profileNightResponse(w, http.StatusOK, st)
}

func (s *Server) handleProfileNightArm(w http.ResponseWriter, r *http.Request) {
	night, ok := s.profileNight(w)
	if !ok {
		return
	}
	st, err := night.Arm(r.Context(), identity(r).Name)
	switch {
	case errors.Is(err, profile.ErrNightActive):
		writeErrorCode(w, http.StatusConflict, "profile_night_active", nil, "an overnight run is already armed or running")
	case errors.Is(err, profile.ErrNightNothing):
		writeErrorCode(w, http.StatusConflict, "profile_night_nothing", nil, "every profilable config is already up to date")
	case err != nil:
		writeInternalError(w, err)
	default:
		s.audit(r, identity(r).Name, "profile_night_arm", "profile.night", time.Unix(st.StartsAt, 0).Format(time.RFC3339))
		s.profileNightResponse(w, http.StatusAccepted, st)
	}
}

func (s *Server) handleProfileNightCancel(w http.ResponseWriter, r *http.Request) {
	night, ok := s.profileNight(w)
	if !ok {
		return
	}
	st, err := night.Cancel(r.Context())
	switch {
	case errors.Is(err, profile.ErrNightIdle):
		writeErrorCode(w, http.StatusConflict, "profile_night_idle", nil, "no overnight run is armed")
	case err != nil:
		writeInternalError(w, err)
	default:
		s.audit(r, identity(r).Name, "profile_night_cancel", "profile.night", "")
		s.profileNightResponse(w, http.StatusOK, st)
	}
}

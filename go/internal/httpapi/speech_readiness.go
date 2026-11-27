// SPDX-License-Identifier: Apache-2.0

package httpapi

// GET /api/v1/speech/readiness: what this node can do with speech and what is missing (docs/speech-setup-plan.md section 2.2). It reads what the daemon
// knows and, for a person asking, checks that the speech front door (forge-tts) answers. It never loads an engine.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/speechsetup"
)

// States of the speech front door as the daemon sees it.
const (
	frontDoorOK            = "ok"
	frontDoorUnreachable   = "unreachable"
	frontDoorNotConfigured = "not_configured" // router.tts_url is empty: the daemon does not know where forge-tts is
)

const frontDoorProbeTimeout = 2 * time.Second

type speechFrontDoorJSON struct {
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
}

type speechReadinessJSON struct {
	speechsetup.Report
	// FrontDoor is absent for forge-tts' own key: it is the front door.
	FrontDoor *speechFrontDoorJSON `json:"front_door,omitempty"`
}

func (s *Server) handleSpeechReadiness(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	engines, err := s.deps.SpecialistStore.ListOccupants(r.Context())
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	defaults, err := s.readSpeechDefaults(r.Context())
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	var slots []specialist.SlotState
	if s.deps.Specialists != nil {
		slots = s.deps.Specialists.Snapshot(r.Context())
	}
	out := speechReadinessJSON{Report: speechsetup.Inspect(s.speechRoot(), engines, defaults, slots)}
	if identity(r).Scope != authz.ScopeSpeech {
		fd := s.probeSpeechFrontDoor(r.Context())
		out.FrontDoor = &fd
		// Nothing can be spoken through a front door that does not answer, whatever the engines say.
		if fd.State == frontDoorUnreachable {
			out.Ready, out.Usable = false, false
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// probeSpeechFrontDoor asks forge-tts' /health. router.tts_url is its /v1 base.
func (s *Server) probeSpeechFrontDoor(ctx context.Context) speechFrontDoorJSON {
	base := strings.TrimRight(s.resolvedRouterConfig(ctx).TTSURL, "/")
	if base == "" {
		return speechFrontDoorJSON{State: frontDoorNotConfigured, Code: "tts_not_configured"}
	}
	down := speechFrontDoorJSON{State: frontDoorUnreachable, Code: "tts_upstream_failed"}
	ctx, cancel := context.WithTimeout(ctx, frontDoorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/v1")+"/health", nil)
	if err != nil {
		return down
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return down
	}
	defer func() { _ = resp.Body.Close() }() // the body has been read or is being discarded: a close error changes nothing
	if resp.StatusCode/100 != 2 {
		return down
	}
	return speechFrontDoorJSON{State: frontDoorOK}
}

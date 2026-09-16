// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_capability_tiers.go — the one new operator-triggered endpoint for
// capability-tier curation (see smith.ProposeCapabilityTiers' doc comment for the
// full design). Deliberately synchronous, not fire-and-forget like
// smith/chat: the reasoning call it makes is bounded (capabilityTierProposeTimeout,
// 150s) and the caller needs the actual created-action-id list back, not a
// conversation to poll.

import (
	"fmt"
	"net/http"

	"github.com/jsaigou/the-forge/internal/smith"
)

func (s *Server) handleSmithCapabilityTiersPropose(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	result, err := s.deps.Smith.ProposeCapabilityTiers(r.Context())
	if err != nil {
		if err == smith.ErrNoCapabilityTierCandidates {
			writeJSON(w, http.StatusOK, &smith.CapabilityTierProposeResult{})
			return
		}
		writeInternalError(w, fmt.Errorf("capability tier propose failed: %w", err))
		return
	}
	s.audit(r, identity(r).Name, "smith_capability_tiers_propose",
		fmt.Sprintf("candidates=%d classes=%d assignments=%d skipped=%d",
			result.CandidateCount, len(result.CreatedClassActions), len(result.CreatedAssignActions), len(result.Skipped)),
		"")
	writeJSON(w, http.StatusOK, result)
}

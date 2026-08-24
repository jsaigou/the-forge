// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_domain.go — smith P6's domain-module API (docs/v5-smith.md §4.9):
//
//	POST /api/v1/smith/sourcing/evaluate  → FR4 HF repo evaluation (read-only)
//
// Same posture as smith_chat.go's handleSmithWebProbe: real external HTTP
// fetches, but no state mutation — operator role only, no requireAssurance.
// FR6 (binaries) and FR7 (ComfyUI) surface entirely through the existing
// findings/checks/actions endpoints (smith_handlers.go/smith_actions.go) —
// binary_versions/comfyui_health/comfyui_prune are ordinary registered
// checks, and delete_files is an ordinary action kind, so neither needs
// dedicated routes.

import (
	"net/http"

	"github.com/jsaigou/the-forge/internal/smith"
)

// smithSourcingEvaluateBody is POST /api/v1/smith/sourcing/evaluate's body.
// BudgetBytes is optional — 0 falls back to the live collector snapshot's
// GTT total (smith.Evaluate's own doc comment).
type smithSourcingEvaluateBody struct {
	HFRepo      string `json:"hf_repo"`
	BudgetBytes int64  `json:"budget_bytes"`
}

// smithSourcingEvaluateResponse wraps smith.SourcingEvaluation so the
// smith package keeps owning its own wire shape (the house convention —
// see smith_kb.go's doc comment).
type smithSourcingEvaluateResponse struct {
	Evaluation smith.SourcingEvaluation `json:"evaluation"`
}

// handleSmithSourcingEvaluate fetches hf_repo's real file listing from
// HuggingFace and ranks its GGUF candidates against budget_bytes.
func (s *Server) handleSmithSourcingEvaluate(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithSourcingEvaluateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.HFRepo == "" {
		writeValidationError(w, map[string]string{"hf_repo": "required"})
		return
	}
	eval, err := s.deps.Smith.Evaluate(r.Context(), b.HFRepo, b.BudgetBytes)
	if err != nil {
		writeError(w, http.StatusBadGateway, "sourcing evaluate failed: "+err.Error())
		return
	}
	s.audit(r, identity(r).Name, "smith_sourcing_evaluate", b.HFRepo, "")
	writeJSON(w, http.StatusOK, smithSourcingEvaluateResponse{Evaluation: eval})
}

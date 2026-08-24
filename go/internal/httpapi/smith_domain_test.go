// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
)

func TestSmithSourcingEvaluate_MissingRepoIsValidationError(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("POST", "/api/v1/smith/sourcing/evaluate", strings.NewReader(`{}`)))
	if w.Code != 422 {
		t.Errorf("evaluate with no hf_repo = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithSourcingEvaluate_NoWebResearch_BadGateway(t *testing.T) {
	// serverWithSmith wires no Deps.Web — Evaluate must fail cleanly, not
	// panic, and the handler must surface it as an error status rather than
	// a 200 with an empty/misleading body.
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("POST", "/api/v1/smith/sourcing/evaluate", strings.NewReader(`{"hf_repo":"org/repo"}`)))
	if w.Code != 502 {
		t.Errorf("evaluate with web unwired = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithSourcingEvaluate_Unwired(t *testing.T) {
	s := newTestServer(t) // no Smith wired
	w := do(t, s, authedRequest("POST", "/api/v1/smith/sourcing/evaluate", strings.NewReader(`{"hf_repo":"org/repo"}`)))
	if w.Code != 503 {
		t.Errorf("evaluate unwired = %d, want 503", w.Code)
	}
}

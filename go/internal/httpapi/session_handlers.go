// SPDX-License-Identifier: Apache-2.0

package httpapi

// session_handlers.go — liveness + PWA bootstrap (Contract 1 §2 #1–2).
// Split out of handlers.go by Sprint 0 (docs/v5-sprint0-contract-freeze.md
// §0.1); pure move, no behavior change. Owner track after split: shared.

import (
	"context"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
)

// handleHealth is the no-auth liveness probe (Contract 1 §2 #1).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"hostname": s.deps.Hostname,
	})
}

// handleSession returns the CSRF token + identity for the PWA bootstrap
// (Contract 1 §2 #2). The PWA fetches this once on boot to get a CSRF
// token and decide which UI to render based on role.
//
// Sprint 0-AUTH: the response now carries the session's assurance level,
// the network principal (if bootstrapped from a trusted network identity),
// and the effective policy map so the SPA can pre-gate navigation.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ident := identity(r)
	// With a real Authenticator, the CSRF token comes from the Session row.
	// With the stub (Phase 4), we issue a placeholder so the PWA's CSRF
	// bootstrap has *something* to send on mutations.
	csrf := "stub-csrf-token"
	if s.deps.Sessions != nil {
		if sid := sessionCookie(r); sid != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if sess, err := s.deps.Sessions.Get(ctx, sid); err == nil && sess.CSRFToken != "" {
				csrf = sess.CSRFToken
			}
		}
	}

	// Load the policy map so the SPA can pre-gate navigation.
	policy := map[string]string{}
	if s.deps.PolicyStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if p, err := s.deps.PolicyStore.Load(ctx); err == nil {
			policy = p
		}
	}

	assurance := string(ident.Assurance)
	if assurance == "" {
		assurance = string(authz.AssurancePassword)
	}

	var principal *string
	if ident.NetworkPrincipal != "" {
		p := ident.NetworkPrincipal
		principal = &p
	}

	writeJSON(w, http.StatusOK, sessionInfoResponse{
		CSRFToken:        csrf,
		Username:         ident.Name,
		Role:             string(ident.Role),
		Assurance:        assurance,
		NetworkPrincipal: principal,
		Policy:           policy,
	})
}

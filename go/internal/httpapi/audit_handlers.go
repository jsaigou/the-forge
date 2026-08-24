// SPDX-License-Identifier: Apache-2.0

package httpapi

// audit_handlers.go — Sprint C. audit_log has existed since Phase 3 (every
// catalog mutation writes one via s.audit) but had no read surface at all
// until this file: no GET route, no UI. The premium config/model editors'
// optional "why this change" note (catalog_handlers.go's withReason) would
// be pure write-only ceremony without this — see docs/v5-prerelease-readiness.md's
// Sprint C entry.

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

type auditEntryJSON struct {
	ID         int64  `json:"id"`
	TS         string `json:"ts"`
	Actor      string `json:"actor"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	RemoteAddr string `json:"remote_addr"`
}

type auditListResponse struct {
	Entries []auditEntryJSON `json:"entries"`
}

// handleAuditList — GET /api/v1/audit?action_prefix=&target=&limit=.
// action_prefix and target are ANDed together (store.Audit.List's
// contract): target IDs collide across entity types (a config #7 and a
// model #7 are both the string "7"), so a caller wanting one target's
// history must also scope by action_prefix (e.g. "catalog_config_"). Admin
// + Settings assurance, matching every other catalog-mutation-adjacent
// route in this file's neighbourhood.
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Audit == nil {
		writeError(w, http.StatusServiceUnavailable, "audit store not available")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	entries, err := s.deps.Audit.List(ctx, r.URL.Query().Get("action_prefix"), r.URL.Query().Get("target"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit query failed")
		return
	}
	resp := auditListResponse{Entries: []auditEntryJSON{}}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, auditEntryJSON{
			ID: e.ID, TS: e.TS.UTC().Format(time.RFC3339), Actor: e.Actor,
			Action: e.Action, Target: e.Target, Detail: e.Detail, RemoteAddr: e.RemoteAddr,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

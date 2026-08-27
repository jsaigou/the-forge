// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_shared.go — small helpers shared across the catalog_*.go files
// (split from catalog_handlers.go, Sprint 5 code-quality cleanup, #33):
// {id} path parsing, the merged-config invalidation hook, the catalog DB
// context timeout, and the change-reason / modality-list validation helpers
// used by more than one entity file.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseID extracts the {id} path value as an int64. Returns false on parse
// failure.
func parseID(r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// invalidateCfg calls the InvalidateConfig hook if wired (after mutations).
func (s *Server) invalidateCfg() {
	if s.deps.InvalidateConfig != nil {
		s.deps.InvalidateConfig()
	}
}

// catalogCtx returns a context with a 5s timeout for catalog DB operations.
func catalogCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// maxAuditReasonLen bounds an operator-supplied change-reason note before it
// is folded into audit_log.detail (Sprint C) — a free-text field gets the
// same treatment as any other operator string reaching an audit record,
// truncated rather than rejected so an over-long note never blocks a save.
const maxAuditReasonLen = 500

// withReason folds an optional operator-supplied "why this change" note
// into an audit detail string (Sprint C). Reason is never persisted on the
// model/config row itself — only here, in audit_log.detail — which is why
// it needs a real read surface (see handleAuditList) rather than being
// write-only ceremony. detail may be empty (e.g. handleCatalogModelDelete
// today).
func withReason(detail, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return detail
	}
	if len(reason) > maxAuditReasonLen {
		reason = reason[:maxAuditReasonLen]
	}
	if detail == "" {
		return "reason: " + reason
	}
	return detail + " — reason: " + reason
}

// validModalities is the Sprint J1 modality enum. Rejecting anything outside
// it at the API boundary is the whole point of typing this column instead of
// leaving it a free-text key_features string.
var validModalities = map[string]bool{"text": true, "vision": true, "audio": true}

// validateModalityList checks every entry against validModalities, reporting
// the first offender by name (matches this file's one-message-per-field
// validation style rather than aggregating every bad entry).
func validateModalityList(mods []string) string {
	for _, m := range mods {
		if !validModalities[m] {
			return fmt.Sprintf("unknown modality %q (must be text, vision, or audio)", m)
		}
	}
	return ""
}

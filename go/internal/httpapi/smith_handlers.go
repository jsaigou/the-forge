// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_handlers.go — the smith self-diagnosis API group (docs/v5-smith.md
// §5). P1 ships the deterministic tier only:
//
//	GET  /api/v1/smith/status       → SelfContext (host telemetry, slots, brain)
//	POST /api/v1/smith/checks/run   → run a quick/deep/explicit sweep now
//	GET  /api/v1/smith/findings     → persisted findings (?since=&severity=&limit=)
//
// All routes are operator-gated (registered in httpapi.go). There is no LLM
// anywhere in this surface — the reasoning tier (chat) and the action/
// approval mutations land in later phases.
//
// smith.SelfContext / smith.Finding / smith.StoredFinding carry snake_case
// JSON tags and are marshaled directly, the same precedent as registry.Card
// in modelCardsResponse — the smith package owns its wire shapes.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/smith"
)

// smithOK reports whether the smith agent is wired (503 otherwise — the
// Phase 4 stub environment and tests that don't exercise smith).
func (s *Server) smithOK(w http.ResponseWriter) bool {
	if s.deps.Smith == nil {
		writeError(w, http.StatusServiceUnavailable, "smith not wired")
		return false
	}
	return true
}

// handleSmithStatus returns the SelfContext picture (docs/v5-smith.md §4.1):
// host telemetry summary, slot allocations, memory budget, the brain
// resolution, and the sweep schedule. The FE renders it as the persistent
// "smith state" chip.
func (s *Server) handleSmithStatus(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Smith.SelfContext(r.Context()))
}

// smithChecksRunBody is the POST /api/v1/smith/checks/run request body
// (docs/v5-smith.md §5): {"scope":"quick"|"deep"} or {"check_ids":[...]}.
// Explicit check_ids win over scope when both are present.
type smithChecksRunBody struct {
	Scope    string   `json:"scope"`
	CheckIDs []string `json:"check_ids"`
}

// smithChecksRunResponse is the sweep result: the findings plus roll-ups.
type smithChecksRunResponse struct {
	SweepKind string          `json:"sweep_kind"`
	Scope     string          `json:"scope"`
	Count     int             `json:"count"`
	Worst     string          `json:"worst"`
	Findings  []smith.Finding `json:"findings"`
}

// handleSmithChecksRun executes a sweep on demand and persists the findings.
// Synchronous — quick sweeps are in-process reads; deep sweeps add bounded
// loopback probes (a few seconds worst case).
func (s *Server) handleSmithChecksRun(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithChecksRunBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	scope := b.Scope
	if scope == "" && len(b.CheckIDs) == 0 {
		scope = smith.ScopeQuick // a bare POST runs the quick sweep
	}
	if len(b.CheckIDs) == 0 && scope != smith.ScopeQuick && scope != smith.ScopeDeep {
		writeValidationError(w, map[string]string{"scope": "must be one of quick, deep (or provide check_ids)"})
		return
	}

	findings, err := s.deps.Smith.RunChecks(r.Context(), scope, b.CheckIDs, smith.SweepManual)
	if err == smith.ErrAlreadyRunning {
		writeError(w, http.StatusConflict, "a smith sweep is already in progress")
		return
	}
	if err != nil {
		writeValidationError(w, map[string]string{"checks": err.Error()})
		return
	}

	s.audit(r, identity(r).Name, "smith_checks_run", scopeOrIDs(scope, b.CheckIDs), "")

	resp := smithChecksRunResponse{
		SweepKind: smith.SweepManual,
		Scope:     scope,
		Count:     len(findings),
		Worst:     string(worstOf(findings)),
		Findings:  findings,
	}
	writeJSON(w, http.StatusOK, resp)
}

// scopeOrIDs renders the audit target for a sweep.
func scopeOrIDs(scope string, ids []string) string {
	if len(ids) > 0 {
		out := ""
		for i, id := range ids {
			if i > 0 {
				out += ","
			}
			out += id
		}
		return out
	}
	return scope
}

// worstOf returns the highest severity among findings (ok when empty).
func worstOf(findings []smith.Finding) smith.Severity {
	worst := smith.SeverityOK
	for _, f := range findings {
		if f.Severity.Rank() > worst.Rank() {
			worst = f.Severity
		}
	}
	return worst
}

// smithFindingsResponse is the GET /api/v1/smith/findings body.
type smithFindingsResponse struct {
	Count    int                   `json:"count"`
	Findings []smith.StoredFinding `json:"findings"`
}

// validFindingSeverities bounds the ?severity= filter.
var validFindingSeverities = map[string]bool{
	string(smith.SeverityOK):   true,
	string(smith.SeverityInfo): true,
	string(smith.SeverityWarn): true,
	string(smith.SeverityCrit): true,
}

// handleSmithFindings lists persisted findings, newest first
// (?since=<unix|RFC3339>&severity=<ok|info|warn|crit>&limit=N).
func (s *Server) handleSmithFindings(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	q := r.URL.Query()

	var since time.Time
	if raw := q.Get("since"); raw != "" {
		parsed, ok := parseSince(raw)
		if !ok {
			writeValidationError(w, map[string]string{"since": "must be unix seconds or RFC3339"})
			return
		}
		since = parsed
	}

	severity := q.Get("severity")
	if severity != "" && !validFindingSeverities[severity] {
		writeValidationError(w, map[string]string{"severity": "must be one of ok, info, warn, crit"})
		return
	}

	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeValidationError(w, map[string]string{"limit": "must be a non-negative integer"})
			return
		}
		limit = n
	}

	findings, err := s.deps.Smith.ListFindings(r.Context(), since, severity, limit)
	if err == smith.ErrStoreUnwired {
		writeJSON(w, http.StatusOK, smithFindingsResponse{Findings: []smith.StoredFinding{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list findings failed")
		return
	}
	writeJSON(w, http.StatusOK, smithFindingsResponse{Count: len(findings), Findings: findings})
}

// parseSince accepts unix seconds or RFC3339 for the since filter.
func parseSince(raw string) (time.Time, bool) {
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(unix, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// smithPurgeBody is the POST /api/v1/smith/findings/purge request body:
// {"max_age":"all"|"72h"|"168h"} — "all" deletes every standalone finding.
type smithPurgeBody struct {
	MaxAge string `json:"max_age"`
}

// smithPurgeResponse is the POST /api/v1/smith/findings/purge response.
type smithPurgeResponse struct {
	Deleted int64 `json:"deleted"`
}

// handleSmithFindingsPurge manually purges standalone findings older than
// max_age (or all of them when max_age is "all"). Investigation-attached
// findings are never pruned — the same evidence-trail rule the scheduled
// retention pass follows. Operator-gated, audited.
func (s *Server) handleSmithFindingsPurge(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithPurgeBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	maxAge := b.MaxAge
	if maxAge == "" {
		maxAge = "all"
	}
	deleted, err := s.deps.Smith.PurgeFindings(r.Context(), maxAge)
	if errors.Is(err, smith.ErrStoreUnwired) {
		writeJSON(w, http.StatusOK, smithPurgeResponse{})
		return
	}
	if errors.Is(err, smith.ErrInvalidPurgeAge) {
		writeValidationError(w, map[string]string{"max_age": `must be "all" or a positive duration (e.g. "72h")`})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "purge findings failed")
		return
	}
	s.audit(r, identity(r).Name, "smith_findings_purge", maxAge, fmt.Sprintf("deleted=%d", deleted))
	writeJSON(w, http.StatusOK, smithPurgeResponse{Deleted: deleted})
}

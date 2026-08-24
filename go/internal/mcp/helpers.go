// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// auditTimeout bounds an audit write, matching httpapi's 2s ceiling.
const auditTimeout = 2 * time.Second

// modeNamePattern mirrors validators._MODE_NAME_PATTERN / httpapi's
// modeNameRE (lowercase identifier). Reused for the `model` argument.
const modeNamePattern = `^[a-z0-9][a-z0-9\-_]{0,63}$`

var modeNameRE = regexp.MustCompile(modeNamePattern)

// ── JSON writers (mirror httpapi/router error shapes) ──────────────────────

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_, _ = w.Write([]byte(`{"error":"encode_failed"}`))
	}
}

// writeError emits the generic error shape {"error": "<message>"}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeValidationError emits the 422 field-level shape (Pydantic parity),
// matching httpapi.writeValidationError.
func writeValidationError(w http.ResponseWriter, fields map[string]string) {
	if fields == nil {
		fields = map[string]string{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"fields": fields,
	})
}

// writeBadJSON reports a malformed request body as 400.
func writeBadJSON(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
}

// writeSchedError classifies a scheduler/engine error to the right HTTP
// status, reusing the sched sentinels via errors.Is (Contract 1 §8 status
// mapping): 404 not-found, 409 conflict, 403 permission-denied, else 500.
func writeSchedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sched.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "not_found", "message": err.Error()})
	case errors.Is(err, sched.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "conflict", "message": err.Error()})
	case errors.Is(err, sched.ErrPermissionDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "permission_denied", "message": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "tool_failed", "message": err.Error()})
	}
}

// ── audit (roadmap R3, docs/v5-mcp-audit.md) ─────────────────────────────────

// audit writes an audit entry if the Audit dependency is wired — the MCP
// mirror of httpapi.Server.audit. Best-effort: failures are silently dropped
// (audit logging must never block or fail a tool call). Actions are prefixed
// "mcp_" so an audit reader can attribute the mutation to the MCP surface
// (the dashboard writes the unprefixed reservation_create / unload_slot
// shapes); actor is the authenticated key name.
func (s *Server) audit(r *http.Request, actor, action, target, detail string) {
	if s.deps.Audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), auditTimeout)
	defer cancel()
	_ = s.deps.Audit.Write(ctx, store.AuditEntry{
		TS:         time.Now(),
		Actor:      actor,
		Action:     action,
		Target:     target,
		Detail:     detail,
		RemoteAddr: r.RemoteAddr,
	})
}

// ── argument validation ────────────────────────────────────────────────────

// validateModelField checks the required `model` argument against the mode
// name pattern. Returns a (possibly empty) field-error map.
func validateModelField(model string) map[string]string {
	fields := map[string]string{}
	if model == "" || len(model) > 128 {
		fields["model"] = "must be 1–128 characters"
		return fields
	}
	if !modeNameRE.MatchString(model) {
		fields["model"] = "must match " + modeNamePattern
	}
	return fields
}

// ── reservation body ───────────────────────────────────────────────────────

// reservationBody is the create/update argument shape. It deliberately has
// NO created_by field: MCP identity comes from the bearer key name, never
// the request body (V4 mcp_server overrode it post-validation).
type reservationBody struct {
	Label                  string `json:"label"`
	Model                  string `json:"model"`
	Start                  string `json:"start"`
	End                    string `json:"end"`
	Scope                  string `json:"scope"`
	Bay                    string `json:"bay"`
	AllowAgentReschedule   *bool  `json:"allow_agent_reschedule"`
	AllowAgentCancellation *bool  `json:"allow_agent_cancellation"`
}

// decodeReservationBody decodes and validates a reservation argument body.
// Returns (body, fieldErrors, decodeErr). A non-nil decodeErr is a
// malformed JSON body (→ 400); a non-empty fieldErrors map is a 422.
func decodeReservationBody(r *http.Request) (reservationBody, map[string]string, error) {
	var b reservationBody
	if err := decodeBody(r, &b); err != nil {
		return b, nil, err
	}
	return b, b.validate(), nil
}

// validate mirrors validators.ReservationCreateRequest invariants (minus
// created_by, which MCP sets from identity): bay set iff scope=="bay",
// end > start, mode name pattern.
func (b reservationBody) validate() map[string]string {
	fields := map[string]string{}
	if b.Label == "" || len(b.Label) > 64 {
		fields["label"] = "must be 1–64 characters"
	}
	if !modeNameRE.MatchString(b.Model) {
		fields["model"] = "must match " + modeNamePattern
	}
	start, okS := parseISOTime(b.Start)
	if !okS {
		fields["start"] = "must be an ISO-8601 timestamp"
	}
	end, okE := parseISOTime(b.End)
	if !okE {
		fields["end"] = "must be an ISO-8601 timestamp"
	}
	switch b.Scope {
	case "bay", "whole_box", "comfyui":
	default:
		fields["scope"] = "must be one of bay, whole_box, comfyui"
	}
	if b.Scope == "bay" && b.Bay == "" {
		fields["bay"] = "bay must be set when scope is 'bay'"
	}
	if b.Scope != "bay" && b.Bay != "" {
		fields["bay"] = "bay must not be set unless scope is 'bay'"
	}
	if b.Scope == "bay" && b.Bay != "" && !authz.ValidSlots[b.Bay] {
		fields["bay"] = "must be one of a1, a2, a3, a4"
	}
	if okS && okE && !end.After(start) {
		if _, hasStart := fields["start"]; !hasStart {
			fields["end"] = "end must be after start"
		}
	}
	return fields
}

// toReservation builds a sched.Reservation from the body, stamping the
// caller's agent identity as created_by and the resolved allow_agent_*
// bools. Time parse errors are impossible here — validate() gated them.
func (b reservationBody) toReservation(createdBy string, allowResched, allowCancel bool) sched.Reservation {
	start, _ := parseISOTime(b.Start)
	end, _ := parseISOTime(b.End)
	return sched.Reservation{
		Label:                  b.Label,
		Model:                  b.Model,
		Start:                  start,
		End:                    end,
		Scope:                  b.Scope,
		Bay:                    b.Bay,
		CreatedBy:              createdBy,
		AllowAgentReschedule:   allowResched,
		AllowAgentCancellation: allowCancel,
	}
}

// parseISOTime accepts RFC3339 plus the bare "YYYY-MM-DDTHH:MM:SS" form
// (httpapi parity).
func parseISOTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ── response shaping ───────────────────────────────────────────────────────

// listModelsJSON builds the list_models inventory from the catalog seam,
// mirroring a0's BuildModelsResponse (internal/router/catalog.go) source
// and filters so the two listings cannot drift: enabled Offerings first,
// then visible (non-hidden) Configs, deduplicated by name with the Offering
// winning a collision (an offering's wire_model is what a0 actually routes).
// The explicit kind field ("local"/"remote") is what BuildModelsResponse
// only encodes implicitly via owned_by. Detail fields are the cheap
// context/capability hints an agent needs to pick a target — context_length
// (config NCtx / provider-reported), config status, and is_default.
func listModelsJSON(ctx context.Context, cat ModelLister) ([]map[string]any, error) {
	out := []map[string]any{}
	seen := map[string]bool{}

	offerings, err := cat.ListOfferings(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range offerings {
		if !o.Enabled || seen[o.WireModel] {
			continue
		}
		entry := map[string]any{
			"name":     o.WireModel,
			"kind":     "remote",
			"provider": o.ProviderName,
		}
		if o.ContextLength > 0 {
			entry["context_length"] = o.ContextLength
		}
		out = append(out, entry)
		seen[o.WireModel] = true
	}

	configs, err := cat.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range configs {
		if c.Visibility == "hidden" || seen[c.Name] {
			continue
		}
		entry := map[string]any{
			"name":    c.Name,
			"kind":    "local",
			"status":  c.Status,
			"default": c.IsDefault,
		}
		if c.NCtx > 0 {
			entry["context_length"] = c.NCtx
		}
		out = append(out, entry)
		seen[c.Name] = true
	}
	return out, nil
}

// statusJSON shapes a sched.Status into the Contract 1 scheduler-status
// JSON (snake_case, null for empty slots, epoch-seconds queue timestamps).
func statusJSON(st sched.Status) map[string]any {
	slots := map[string]*string{}
	for slot, mode := range st.Slots {
		slots[slot] = slotModeOrNull(mode)
	}
	slotLabels := map[string]string{}
	for slot, label := range st.SlotLabels {
		slotLabels[slot] = label
	}
	idle := map[string]*float64{}
	for slot, v := range st.IdleSeconds {
		if v == nil {
			idle[slot] = nil
			continue
		}
		cp := *v
		idle[slot] = &cp
	}
	queue := []map[string]any{}
	for _, t := range st.Queue {
		queue = append(queue, map[string]any{
			"ticket_id":    t.TicketID,
			"model":        t.Model,
			"requested_by": t.RequestedBy,
			"target_slot":  ptrString(t.TargetSlot),
			"status":       t.Status,
			"small_job":    t.SmallJob,
			"enqueued_at":  unixSeconds(t.EnqueuedAt),
		})
	}
	return map[string]any{
		"slots":        slots,
		"slot_labels":  slotLabels,
		"idle_seconds": idle,
		"memory_budget": map[string]any{
			"total_bytes": st.MemoryBudget.TotalBytes,
			"used_bytes":  st.MemoryBudget.UsedBytes,
			"free_bytes":  st.MemoryBudget.FreeBytes,
		},
		"queue": queue,
	}
}

// ensureResultJSON shapes an ensure_loaded Ticket into a result object.
// success is true when the model ended up loaded.
func ensureResultJSON(t sched.Ticket) map[string]any {
	return map[string]any{
		"success":      t.Status == "loaded",
		"ticket_id":    t.TicketID,
		"model":        t.Model,
		"requested_by": t.RequestedBy,
		"target_slot":  ptrString(t.TargetSlot),
		"status":       t.Status,
		"small_job":    t.SmallJob,
		"enqueued_at":  unixSeconds(t.EnqueuedAt),
	}
}

// reservationJSON shapes a sched.Reservation for the list response
// (mirrors httpapi's reservationResponse).
func reservationJSON(r sched.Reservation) map[string]any {
	return map[string]any{
		"label":                    r.Label,
		"model":                    r.Model,
		"start":                    isoFormat(r.Start),
		"end":                      isoFormat(r.End),
		"scope":                    r.Scope,
		"bay":                      ptrString(r.Bay),
		"created_by":               r.CreatedBy,
		"allow_agent_reschedule":   r.AllowAgentReschedule,
		"allow_agent_cancellation": r.AllowAgentCancellation,
	}
}

// unixSeconds returns the float epoch for t (0 for zero time).
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// isoFormat returns the RFC3339 string for t ("" for zero time).
func isoFormat(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// slotModeOrNull returns &mode when mode != "", else nil.
func slotModeOrNull(mode string) *string {
	if mode == "" {
		return nil
	}
	v := mode
	return &v
}

// ptrString returns &v, or nil when v is empty.
func ptrString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

// validate.go — request-body validators that mirror forge/validators.py
// at the freeze commit. Each Parse* function returns a 422 fields map on
// failure; handlers convert to the wire shape via writeValidationError.
//
// The validation rules are the frozen Contract 1 §3 bounds (e.g.
// SchedulerConfigRequest: idle_unload_s 30–3600 default 180,
// small_job_token_threshold ≥1 default 1500, priority_jump_cap ≥0 default 2,
// reservation_soon_min 1–120 default 10). Patterns mirror the Pydantic
// regexes verbatim.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
)

// modeNamePattern is validators._MODE_NAME_PATTERN (lowercase identifier).
const modeNamePattern = `^[a-z0-9][a-z0-9\-_]{0,63}$`

var modeNameRE = regexp.MustCompile(modeNamePattern)

// usageWindowRE matches the UsageQueryRequest.window pattern (1h..Nd).
var usageWindowRE = regexp.MustCompile(`^\d+[hd]$`)

// servicePattern matches the CompressorRestartRequest.service pattern.
var serviceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]+$`)

// providerNameRE validates provider DISPLAY NAMES (not service names).
// Allows Unicode letters/numbers, spaces, ampersand, underscores, hyphens,
// periods — must start with a letter or number. No emoji, control chars, or
// other special symbols. This is intentionally looser than serviceRE because
// provider names are display-only (surrogate IDs since 0042, icon slug via
// .toLowerCase()).
var providerNameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} &_.-]+$`)

// ── Parse helpers ────────────────────────────────────────────────────────────

// decodeJSONBody reads up to 1 MiB from r.Body and decodes into v. Returns
// a fields map for the 422 path on failure; nil on success (including the
// empty-body case, which decodes to the zero value of v).
func decodeJSONBody(r *http.Request, v any) map[string]string {
	if r.Body == nil {
		return nil
	}
	// Empty bodies (POST /switch/<mode>, POST /tts/start) decode to the
	// zero value — there's no body to validate.
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return nil
		}
		return map[string]string{"body": err.Error()}
	}
	return nil
}

// ── Reservation ──────────────────────────────────────────────────────────────

// reservationCreateBody mirrors validators.ReservationCreateRequest. The
// optional allow_agent_* fields use *bool so "absent" is distinct from
// "explicitly false" (Pydantic parity — the scheduler applies the
// created_by-conditional default when absent).
type reservationCreateBody struct {
	Label                  string `json:"label"`
	Model                  string `json:"model"`
	Start                  string `json:"start"`
	End                    string `json:"end"`
	Scope                  string `json:"scope"`
	Bay                    string `json:"bay"`
	CreatedBy              string `json:"created_by"`
	AllowAgentReschedule   *bool  `json:"allow_agent_reschedule"`
	AllowAgentCancellation *bool  `json:"allow_agent_cancellation"`
}

// validate mirrors validators.ReservationCreateRequest.model_validator
// invariants: bay set iff scope=="bay", end > start, mode name pattern.
func (b reservationCreateBody) validate() (reservationCreateBody, map[string]string) {
	fields := map[string]string{}
	if b.Label == "" || len(b.Label) > 64 {
		fields["label"] = "must be 1–64 characters"
	}
	if !modeNameRE.MatchString(b.Model) {
		fields["model"] = "must match " + modeNamePattern
	}
	start, ok := parseISOTime(b.Start)
	if !ok {
		fields["start"] = "must be an ISO-8601 timestamp"
	}
	end, ok := parseISOTime(b.End)
	if !ok {
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
	if b.Scope == "bay" && !authz.ValidSlots[b.Bay] {
		fields["bay"] = "must be one of a1, a2, a3, a4"
	}
	if b.CreatedBy == "" || len(b.CreatedBy) > 128 {
		fields["created_by"] = "must be 1–128 characters"
	}
	if !end.After(start) {
		if _, hasStart := fields["start"]; !hasStart {
			fields["end"] = "end must be after start"
		}
	}
	return b, fields
}

// parseISOTime accepts RFC3339 (the canonical ISO-8601 profile) plus the
// bare "YYYY-MM-DDTHH:MM:SS" form Pydantic accepts without a zone.
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

// ── SchedulerConfig ──────────────────────────────────────────────────────────

type schedulerConfigBody struct {
	IdleUnloadS            *int `json:"idle_unload_s"`
	SmallJobTokenThreshold *int `json:"small_job_token_threshold"`
	PriorityJumpCap        *int `json:"priority_jump_cap"`
	ReservationSoonMin     *int `json:"reservation_soon_min"`
}

// validate mirrors validators.SchedulerConfigRequest bounds. Pointer fields
// distinguish "absent" from "explicitly zero".
func (b schedulerConfigBody) validate() (schedulerConfigBody, map[string]string) {
	fields := map[string]string{}
	if b.IdleUnloadS == nil {
		v := 180
		b.IdleUnloadS = &v
	} else if *b.IdleUnloadS < 30 || *b.IdleUnloadS > 3600 {
		fields["idle_unload_s"] = "must be between 30 and 3600"
	}
	if b.SmallJobTokenThreshold == nil {
		v := 1500
		b.SmallJobTokenThreshold = &v
	} else if *b.SmallJobTokenThreshold < 1 {
		fields["small_job_token_threshold"] = "must be ≥ 1"
	}
	if b.PriorityJumpCap == nil {
		v := 2
		b.PriorityJumpCap = &v
	} else if *b.PriorityJumpCap < 0 {
		fields["priority_jump_cap"] = "must be ≥ 0"
	}
	if b.ReservationSoonMin == nil {
		v := 10
		b.ReservationSoonMin = &v
	} else if *b.ReservationSoonMin < 1 || *b.ReservationSoonMin > 120 {
		fields["reservation_soon_min"] = "must be between 1 and 120"
	}
	return b, fields
}

// ── Slot load / unload ──────────────────────────────────────────────────────

type slotLoadBody struct {
	Mode string `json:"mode"`
	Slot string `json:"slot"`
}

func (b slotLoadBody) validate() (slotLoadBody, map[string]string) {
	fields := map[string]string{}
	if !modeNameRE.MatchString(b.Mode) {
		fields["mode"] = "must match " + modeNamePattern
	}
	if !authz.ValidSlots[b.Slot] {
		fields["slot"] = "must be one of a1, a2, a3, a4"
	}
	return b, fields
}

type slotUnloadBody struct {
	Slot string `json:"slot"`
}

func (b slotUnloadBody) validate() (slotUnloadBody, map[string]string) {
	fields := map[string]string{}
	if b.Slot != "all" && !authz.ValidSlots[b.Slot] {
		fields["slot"] = "must be one of a1, a2, a3, a4, or 'all'"
	}
	return b, fields
}

// ── Compressor passthrough ─────────────────────────────────────────────────────

type compressorPassthroughBody struct {
	Scope   string `json:"scope"`
	Service string `json:"service"`
	Enabled *bool  `json:"enabled"`
}

func (b compressorPassthroughBody) validate() (compressorPassthroughBody, map[string]string) {
	fields := map[string]string{}
	if b.Scope != "all" && b.Scope != "proxy" {
		fields["scope"] = "must be one of all, proxy"
	}
	if b.Enabled == nil {
		fields["enabled"] = "is required"
	}
	if b.Scope == "proxy" {
		if b.Service == "" {
			fields["service"] = "is required when scope is 'proxy'"
		} else if !serviceRE.MatchString(b.Service) || len(b.Service) > 64 {
			fields["service"] = "must match ^[a-z][a-z0-9_-]+$ (max 64)"
		}
	}
	if b.Scope == "all" && b.Service != "" {
		fields["service"] = "must not be set when scope is 'all'"
	}
	return b, fields
}

// compressorLifecycleBody is the POST /api/v1/compressor/{restart,proxy/teardown}
// body (Contract 1 §2 #19-20): `{"service": string}`.
type compressorLifecycleBody struct {
	Service string `json:"service"`
}

func (b compressorLifecycleBody) validate() (compressorLifecycleBody, map[string]string) {
	fields := map[string]string{}
	if b.Service == "" {
		fields["service"] = "is required"
	} else if !serviceRE.MatchString(b.Service) || len(b.Service) > 64 {
		fields["service"] = "must match ^[a-z][a-z0-9_-]+$ (max 64)"
	}
	return b, fields
}

// ── Compressor standalone proxy create ──────────────────────────────────────────

// compressorProxyCreateBody is the POST /api/v1/compressor/proxy/create body for
// creating a proxy not linked to a provider (e.g. a local upstream, or the
// shared "external" instance — docs/v5-headroom-replacement.md Sprint 3).
type compressorProxyCreateBody struct {
	Service   string `json:"service"`
	Label     string `json:"label"`
	TargetURL string `json:"target_url"`
	// Template selects which systemd template provisions this proxy: ""
	// (default) for the legacy headroom@ instance, "compress" for Sprint
	// 3's forge-compress@ Go binary. Mirrors provisionerFor's own
	// per-Provisioner dispatch used by restart/teardown/migrate.
	Template string `json:"template"`
}

func (b compressorProxyCreateBody) validate() (compressorProxyCreateBody, map[string]string) {
	fields := map[string]string{}
	if b.Service == "" || !serviceRE.MatchString(b.Service) || len(b.Service) > 64 {
		fields["service"] = "must match ^[a-z][a-z0-9_-]+$ (max 64)"
	}
	if b.Label == "" || len(b.Label) > 64 {
		fields["label"] = "must be 1–64 characters"
	}
	if b.TargetURL == "" {
		fields["target_url"] = "is required"
	}
	if b.Template != "" && b.Template != "compress" {
		fields["template"] = "must be empty or 'compress'"
	}
	return b, fields
}

// ── Usage window ────────────────────────────────────────────────────────────

// parseUsageWindow returns the duration for a window string like "7d" or
// "24h". Returns false if the value doesn't match the pattern.
func parseUsageWindow(s string) (time.Duration, bool) {
	if !usageWindowRE.MatchString(s) {
		return 0, false
	}
	unit := s[len(s)-1]
	n, err := parsePositiveInt(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	if unit == 'h' {
		return time.Duration(n) * time.Hour, true
	}
	return time.Duration(n) * 24 * time.Hour, true
}

func parsePositiveInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if strings.ContainsAny(s, "-+ ") {
		return 0, fmt.Errorf("unexpected sign/space")
	}
	return n, nil
}

// writeInternalError logs the full error server-side and returns a generic
// "internal error" message to the client, avoiding disclosure of internal
// paths, DB errors, or provisioning details. Use for all 5xx responses where
// the error comes from store/DB/filesystem/provisioning layers.
func writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("httpapi: internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

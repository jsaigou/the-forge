// SPDX-License-Identifier: Apache-2.0

package httpapi

// shapes_freeze_test.go guards the Sprint 0 frozen contract shapes
// (docs/v5-sprint0-contract-freeze.md). These types are wired by the parallel
// tracks (BE-1/2/3/5) after the freeze, so nothing in this package references
// them yet; this test pins their JSON keys so a track can't silently rename a
// frozen field, and keeps staticcheck (U1000) green in the interim.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/smith/web"
)

// hasKeys asserts every wanted top-level JSON key is present in v's marshaling.
func hasKeys(t *testing.T, v any, keys ...string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%T: missing frozen key %q in %s", v, k, string(b))
		}
	}
}

func TestFrozenProviderShapes(t *testing.T) {
	hasKeys(t, providersResponse{}, "providers")
	hasKeys(t, providerJSON{}, "name", "api_key_masked", "bill_currency", "health", "credits", "models")
	// Additive omitempty fields — absent on a zero-value struct, so seed
	// them to verify they marshal under the documented JSON keys.
	hasKeys(t, providerJSON{TargetURL: "x", StatusURL: "x", OrgID: "x"},
		"target_url", "status_url", "org_id")
	// Phase 7 (2026-08-13): models[] is now offerings-derived (provider_models
	// was dead — 0 rows live, no write path — dropped in migration 0043).
	// bill_currency -> currency (the offering's own, not the provider's) +
	// catalog_model_id/priority added.
	// headroom_proxy -> compressor_proxy (Sprint 7, docs/v5-headroom-replacement.md):
	// a deliberate, disclosed break of this frozen field name as part of the
	// full "Headroom" -> "compressor" rename, not an accidental drift.
	hasKeys(t, providerModelJSON{}, "model_id", "catalog_model_id", "display_name", "logo",
		"price_in_per_1m", "price_out_per_1m", "currency", "priority", "enabled",
		"compressor_proxy", "passthrough")
	hasKeys(t, providerHealthJSON{}, "state", "as_of", "source", "detail")
	hasKeys(t, providerCreditsJSON{}, "balance_native", "currency", "as_of", "supported")
}

func TestFrozenMetricsHistoryShape(t *testing.T) {
	hasKeys(t, metricsHistoryResponse{}, "window", "resolution_s", "points")
	// ts is always present; the series fields are omitempty (only requested
	// series populate), so a zero point marshals with just ts.
	hasKeys(t, metricsHistoryPoint{TS: 1}, "ts")
}

func TestFrozenBillingSettingsShape(t *testing.T) {
	hasKeys(t, billingSettingsResponse{DisplayCurrency: "USD"}, "display_currency")
}

func TestRegisteredSettingKeys(t *testing.T) {
	// Sprint 0 §0.12: exactly these keys are registered by the polish sprint.
	// logging.forward_url was removed in Sprint 12 (was H) Phase 1 —
	// zero production readers found; see settings_handlers.go's const block.
	want := []string{
		"billing.display_currency", "billing.fx_source_url", "billing.fx_refresh_min",
		"metrics.retention_days", "metrics.sample_interval_s",
	}
	if len(registeredSettingKeys) != len(want) {
		t.Fatalf("registeredSettingKeys = %v, want %v", registeredSettingKeys, want)
	}
	got := strings.Join(registeredSettingKeys, ",")
	for _, k := range want {
		if !strings.Contains(got, k) {
			t.Errorf("registered key %q missing from %s", k, got)
		}
	}
}

// TestFrozenAuthShapes pins the Sprint 0-AUTH frozen JSON keys
// (docs/v5-sprint0-auth-design.md §6). Endpoints return 501 in this gate;
// these assertions guard against a track silently renaming a frozen field.
func TestFrozenAuthShapes(t *testing.T) {
	// Session (extended for assurance).
	hasKeys(t, sessionInfoResponse{}, "csrf_token", "username", "role",
		"assurance", "network_principal", "policy")

	// Step-up.
	hasKeys(t, stepUpRequest{}, "factor")
	hasKeys(t, stepUpResponse{}, "assurance", "assurance_at")

	// WebAuthn.
	hasKeys(t, webauthnCredentialJSON{}, "id", "label", "transports", "created_at")
	hasKeys(t, webauthnCredentialsResponse{}, "credentials")
	hasKeys(t, webauthnPublicKeyCredentialDescriptorJSON{}, "id", "type")
	hasKeys(t, webauthnPubKeyCredParamJSON{}, "type", "alg")
	hasKeys(t, webauthnRelyingPartyJSON{}, "id", "name")
	hasKeys(t, webauthnUserJSON{}, "id", "name", "display_name")
	hasKeys(t, webauthnCreationOptionsJSON{}, "rp", "user", "challenge", "pubKeyCredParams")
	hasKeys(t, webauthnBeginRegisterResponse{}, "options")
	hasKeys(t, webauthnRegResponseDataJSON{}, "clientDataJSON", "attestationObject")
	hasKeys(t, webauthnRegistrationResponseJSON{}, "id", "rawId", "type", "response")
	hasKeys(t, webauthnFinishRegisterRequest{}, "response", "label")
	hasKeys(t, webauthnFinishRegisterResponse{}, "credential")
	hasKeys(t, webauthnRequestOptionsJSON{}, "challenge")
	hasKeys(t, webauthnBeginAssertResponse{}, "options")
	hasKeys(t, webauthnAuthResponseDataJSON{}, "clientDataJSON", "authenticatorData", "signature")
	hasKeys(t, webauthnAuthenticationResponseJSON{}, "id", "rawId", "type", "response")
	hasKeys(t, webauthnFinishAssertRequest{}, "response")
	hasKeys(t, webauthnFinishAssertResponse{}, "verified")

	// TOTP.
	hasKeys(t, totpEnrollResponse{}, "secret", "otpauth_uri")
	hasKeys(t, totpConfirmRequest{}, "code")
	hasKeys(t, totpConfirmResponse{}, "active")

	// Identity linking.
	hasKeys(t, identityLinkResponse{}, "provider", "principal", "user_id", "created_at")
	hasKeys(t, identityLinksResponse{}, "links")
	hasKeys(t, identityLinkCreateRequest{}, "provider", "principal", "user_id")

	// API-key management.
	hasKeys(t, apiKeyResponse{}, "keyid", "kind", "name", "created_at")
	hasKeys(t, apiKeysResponse{}, "keys")
	hasKeys(t, apiKeyCreateRequest{}, "kind", "name")
	hasKeys(t, apiKeyCreateResponse{}, "token", "key")

	// Policy + config.
	hasKeys(t, authPolicyResponse{}, "policy")
	hasKeys(t, authPolicyPutRequest{}, "policy")
	hasKeys(t, authConfigResponse{}, "network_provider")
	hasKeys(t, authConfigPutRequest{}, "network_provider")
}

// TestFrozenSmithShapes pins the smith self-diagnosis wire keys
// (docs/v5-smith.md §5). smith.SelfContext / smith.Finding /
// smith.StoredFinding are marshaled directly by the handlers (the smith
// package owns its snake_case tags, same precedent as registry.Card), so the
// freeze guards them here at the httpapi boundary where they're served.
func TestFrozenSmithShapes(t *testing.T) {
	// GET /api/v1/smith/status.
	hasKeys(t, smith.SelfContext{},
		"hostname", "snapshot_taken_at", "snapshot_age_s", "metrics", "alerts",
		"slots", "memory_budget", "brain", "tier", "check_count",
		"fast_check_count", "schedule", "web")
	// missed_patterns (Sprint S2-Go §3.7) is omitempty — seed it so hasKeys
	// can see the key marshal at all on a non-zero struct. Additive to the
	// frozen wire shape.
	hasKeys(t, smith.SelfContext{MissedPatterns: []smith.MissedPattern{{}}}, "missed_patterns")
	hasKeys(t, smith.WebStatus{}, "enabled", "providers")
	hasKeys(t, web.ProviderStatus{}, "name", "role", "configured", "enabled",
		"reachable", "detail", "checked_at", "latency_ms")
	hasKeys(t, smith.BrainResolution{}, "resolution", "detail")
	hasKeys(t, smith.MetricsSummary{}, "mode", "gtt_used_bytes", "gtt_total_bytes", "disk_pct")
	hasKeys(t, smith.SlotAllocation{}, "mode", "label", "memory_bytes", "idle_seconds")
	hasKeys(t, smith.Schedule{}, "quick", "deep", "enabled")

	// POST /api/v1/smith/checks/run.
	hasKeys(t, smithChecksRunBody{}, "scope", "check_ids")
	hasKeys(t, smithChecksRunResponse{}, "sweep_kind", "scope", "count", "worst", "findings")
	hasKeys(t, smith.Finding{}, "check_id", "severity", "summary", "evidence", "proposal_ids", "kb_refs")

	// GET /api/v1/smith/findings.
	hasKeys(t, smithFindingsResponse{}, "count", "findings")
	hasKeys(t, smith.StoredFinding{},
		"id", "investigation_id", "check_id", "severity", "summary",
		"evidence", "sweep_kind", "created_at", "kb_refs", "repeat_count")

	// Investigations (Wave 2 — docs/v5-smith-wave2.md §3).
	hasKeys(t, smithInvestigationsResponse{}, "count", "investigations")
	hasKeys(t, smith.Investigation{},
		"id", "trigger", "status", "opened_at", "closed_at", "summary",
		"conversation_id")
	// resolved_by_action_id (Sprint S3 §2.4) is omitempty — seed it so
	// hasKeys can see the key marshal at all on a non-zero struct.
	// Additive to the frozen wire shape.
	resolvedBy := int64(1)
	hasKeys(t, smith.Investigation{ResolvedByActionID: &resolvedBy}, "resolved_by_action_id")
	hasKeys(t, smithInvestigationCreateBody{}, "trigger", "summary")
	hasKeys(t, smithInvestigationDetailResponse{}, "id", "trigger", "status",
		"opened_at", "closed_at", "summary", "conversation_id", "findings")
	hasKeys(t, smithInvestigationChecksBody{}, "check_ids", "scope")
	hasKeys(t, smithInvestigationChecksResponse{}, "count", "worst", "findings")
	hasKeys(t, smithInvestigationResolveBody{}, "status")
	hasKeys(t, smithChecksResponse{}, "count", "checks")
	hasKeys(t, smith.CheckMeta{}, "id", "name", "category", "fast")
}

// TestFrozenSmithKBShapes pins the P4 knowledge-base wire keys
// (docs/v5-smith.md §4.7/§5). smith.KBResult / smith.Chunk /
// smith.BlockedItem carry snake_case JSON tags and are marshaled directly,
// same precedent as the rest of this file.
func TestFrozenSmithKBShapes(t *testing.T) {
	// GET /api/v1/smith/kb/search.
	hasKeys(t, smithKBSearchResponse{}, "count", "results")
	hasKeys(t, smith.KBResult{}, "kind", "ref", "title", "body", "score", "ts")

	// GET /api/v1/smith/kb/{ref}.
	hasKeys(t, smithKBRefResponse{}, "chunk")
	hasKeys(t, smith.Chunk{}, "ref", "doc", "slug", "title", "category", "source", "body")

	// GET /api/v1/smith/kb/blocked.
	hasKeys(t, smithKBBlockedResponse{}, "count", "items")
	hasKeys(t, smith.BlockedItem{},
		"number", "title", "status", "status_text", "blocked_on",
		"where_to_check", "when_unblocked", "last_checked", "urls")
}

// TestFrozenSmithActionShapes pins the P2 action-model wire keys
// (docs/v5-smith.md §4.6, the smith P2 plan's §6). Track W3-C (the frontend)
// committed against these shapes before this handler surface existed, so
// every field here is load-bearing for that track — a silent rename breaks
// the FE without a compile error on either side.
func TestFrozenSmithActionShapes(t *testing.T) {
	// GET /api/v1/smith/actions.
	hasKeys(t, smithActionsResponse{}, "count", "pending_count", "actions")
	hasKeys(t, smith.Action{},
		"id", "investigation_id", "conversation_id", "finding_id", "kind", "title",
		"detail", "risk", "status", "self_evicting", "handoff", "dedupe_key",
		"result", "created_by", "approved_by", "audit_ref", "created_at",
		"executed_at", "verified_at", "resolved_at")

	// POST /api/v1/smith/actions.
	hasKeys(t, smithActionCreateBody{}, "kind", "title", "detail", "risk", "investigation_id")

	// Shared handoff/candidate/runbook/result shapes.
	hasKeys(t, smith.Handoff{},
		"state", "reason", "brain_slot", "brain_model", "candidates", "runbook",
		"issued_at", "acknowledged_by", "acknowledged_at")
	// why/verify_command are omitempty (P7) — seed them so hasKeys can see
	// the keys marshal at all on a non-zero struct.
	hasKeys(t, smith.RunbookStep{Why: "x", VerifyCommand: "x"}, "title", "command", "verify", "why", "verify_command")
	hasKeys(t, smith.Candidate{}, "offering_id", "model", "provider", "healthy")
	hasKeys(t, smith.ActionResult{}, "ok", "message", "verify")
	hasKeys(t, smith.VerifyResult{}, "check_id", "severity", "summary", "at")

	// 409 handoff_required body (shared by create + approve).
	hasKeys(t, smithHandoffRequiredBody{}, "error", "action_id", "handoff", "message")

	// POST .../handoff.
	hasKeys(t, smithActionHandoffBody{}, "resolution")
}

// TestFrozenSmithChatShapes pins the P3 reasoning-tier wire keys
// (docs/v5-smith.md §5): conversations, chat, and settings.
func TestFrozenSmithChatShapes(t *testing.T) {
	// Conversations.
	hasKeys(t, smithConversationsResponse{}, "count", "conversations")
	hasKeys(t, smith.Conversation{}, "id", "title", "tier", "created_at", "updated_at")
	hasKeys(t, smithConversationCreateBody{}, "title")
	hasKeys(t, smithConversationDetailResponse{},
		"id", "title", "tier", "created_at", "updated_at", "messages")
	hasKeys(t, smith.Message{},
		"id", "conversation_id", "kind", "content", "evidence", "model",
		"tier", "error", "token_count", "created_at", "sources")
	hasKeys(t, smith.MessageSource{}, "provider", "url", "title", "snippet", "fetched_at", "cached")

	// POST /api/v1/smith/chat.
	hasKeys(t, smithChatBody{}, "conversation_id", "text", "escalate", "web")
	// context (Sprint S3 §3.4) is omitempty — seed it so hasKeys can see
	// the key marshal at all on a non-zero struct. Additive to the frozen
	// wire shape.
	hasKeys(t, smithChatBody{Context: []smith.ChatContext{{}}}, "context")
	hasKeys(t, smithChatResponse{}, "conversation_id", "message_id")

	// GET|PUT /api/v1/smith/settings.
	hasKeys(t, smithSettingsResponse{}, "model", "handoff_offerings", "schedule", "thresholds", "web", "tools", "retention")
	hasKeys(t, smith.Thresholds{}, "gtt_warn_pct", "gtt_crit_pct", "disk_warn_pct", "disk_crit_pct")
	hasKeys(t, smithSettingsBody{}, "model", "handoff_offerings", "schedule", "thresholds", "web", "tools", "retention")
	hasKeys(t, smithScheduleBody{}, "quick", "deep", "enabled")
	hasKeys(t, smithThresholdsBody{}, "gtt_warn_pct", "gtt_crit_pct", "disk_warn_pct", "disk_crit_pct")

	// Web research (P5, docs/v5-smith.md §4.8) — part of the settings
	// surface above.
	hasKeys(t, smithWebBody{}, "enabled", "provider_order", "cache_ttl", "searxng", "firecrawl", "direct", "customsearch", "customfetch")
	hasKeys(t, smithWebProviderBody{}, "base_url", "enabled", "api_key")

	// Tool loop + retention (P7, docs/v5-smith.md §9) — part of the
	// settings surface above.
	hasKeys(t, smith.ToolsConfig{}, "enabled", "mode", "max_rounds")
	hasKeys(t, smithToolsBody{}, "enabled", "mode", "max_rounds")
	hasKeys(t, smith.Retention{}, "enabled", "ok_days", "info_hours", "warn_crit_days", "web_cache_days", "web_cache_max_rows")
	hasKeys(t, smithRetentionBody{}, "enabled", "ok_days", "info_hours", "warn_crit_days", "web_cache_days", "web_cache_max_rows")

	// smith:tool_call evidence shape (P7) — one round's tool activity,
	// persisted on a MsgKindToolCall message's evidence column.
	hasKeys(t, smith.SelfContext{}, "hostname", "snapshot_taken_at", "snapshot_age_s", "metrics",
		"alerts", "slots", "memory_budget", "brain", "tier", "check_count", "fast_check_count",
		"schedule", "web", "tools", "retention")
	hasKeys(t, smith.ToolsStatus{}, "enabled", "mode", "resolved_mode", "model", "count")
	hasKeys(t, smith.RetentionStatus{}, "enabled", "last_run_at", "deleted_findings", "deleted_web_cache")
}

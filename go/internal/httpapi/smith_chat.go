// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_chat.go — the P3 reasoning-tier API surface (docs/v5-smith.md §5):
// conversations CRUD, the chat turn endpoint (202 + SSE streaming, see
// lib/sse.ts's smith:token/smith:message_done handling on the FE side),
// GET/PUT smith settings, and the investigations/{id}/analyze Tier 2
// commentary endpoint specified back in P0 but never routed until now.
//
// All routes operator-gated (registered in httpapi.go); PUT settings has no
// extra step-up beyond operator role — smith.model is explicitly "freely
// changeable" per docs §4.3 decision-log item 2, not a high-risk mutation
// (the actions that actually load/unload/restart/swap-brain still go
// through ResourceActionSmithExecute).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/smith/web"
)

// ── Conversations ────────────────────────────────────────────────────────

type smithConversationsResponse struct {
	Count         int                  `json:"count"`
	Conversations []smith.Conversation `json:"conversations"`
}

// handleSmithConversationsList lists conversations, most-recently-updated
// first.
func (s *Server) handleSmithConversationsList(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	convs, err := s.deps.Smith.ListConversations(r.Context())
	if err == smith.ErrStoreUnwired {
		writeJSON(w, http.StatusOK, smithConversationsResponse{Conversations: []smith.Conversation{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list conversations failed")
		return
	}
	writeJSON(w, http.StatusOK, smithConversationsResponse{Count: len(convs), Conversations: convs})
}

type smithConversationCreateBody struct {
	Title string `json:"title"`
}

// handleSmithConversationCreate opens a new conversation.
func (s *Server) handleSmithConversationCreate(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithConversationCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	id, err := s.deps.Smith.CreateConversation(r.Context(), b.Title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create conversation failed")
		return
	}
	s.audit(r, identity(r).Name, "smith_conversation_create", "", "")
	conv, _, err := s.deps.Smith.GetConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "fetch created conversation failed")
		return
	}
	writeJSON(w, http.StatusCreated, conv)
}

type smithConversationDetailResponse struct {
	smith.Conversation
	Messages []smith.Message `json:"messages"`
}

// handleSmithConversationDetail returns a conversation and its full
// transcript.
func (s *Server) handleSmithConversationDetail(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	conv, msgs, err := s.deps.Smith.GetConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "conversation not found")
		return
	}
	writeJSON(w, http.StatusOK, smithConversationDetailResponse{Conversation: *conv, Messages: msgs})
}

// handleSmithConversationDelete deletes a conversation (messages cascade).
func (s *Server) handleSmithConversationDelete(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	if err := s.deps.Smith.DeleteConversation(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "delete conversation failed")
		return
	}
	s.audit(r, identity(r).Name, "smith_conversation_delete", fmt.Sprintf("%d", id), "")
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// ── Chat ─────────────────────────────────────────────────────────────────

// smithChatBody is POST /api/v1/smith/chat's request body.
// conversation_id=0/absent starts a new conversation. Web (P5,
// docs/v5-smith.md §4.8) is the explicit per-message web-research opt-in —
// the LLM never decides to search on its own (that's P7's tool loop); the
// operator ticks a box. Context (S3, §3.4) carries attached error context
// so the server composes the seed message itself (the FE never
// string-formats evidence). When context is present and text is empty, the
// server composes the seed user message from the context array.
type smithChatBody struct {
	ConversationID int64               `json:"conversation_id"`
	Text           string              `json:"text"`
	Escalate       bool                `json:"escalate"`
	Web            bool                `json:"web"`
	Context        []smith.ChatContext `json:"context,omitempty"`
}

type smithChatResponse struct {
	ConversationID int64 `json:"conversation_id"`
	MessageID      int64 `json:"message_id"`
}

// handleSmithChat starts one chat turn and returns immediately (202) with
// the placeholder assistant message ID; the answer arrives over the
// existing SSE stream as smith:token deltas followed by smith:message_done
// (docs §5 — api.ts stays JSON-only, no fetch streaming).
func (s *Server) handleSmithChat(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	var b smithChatBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Text == "" {
		// Context-seeded turn (§3.4, R5): when text is empty but context
		// is attached, the server composes the seed user message itself so
		// no FE ever string-formats evidence. The composed text is a
		// canonical one-line format quoting the error.
		if len(b.Context) == 0 {
			writeValidationError(w, map[string]string{"text": "must not be empty"})
			return
		}
		b.Text = composeContextSeedMessage(b.Context)
	}

	ctx := r.Context()
	convID := b.ConversationID
	if convID == 0 {
		id, err := s.deps.Smith.CreateConversation(ctx, "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create conversation failed")
			return
		}
		convID = id
	}

	msgID, err := s.deps.Smith.Chat(ctx, convID, b.Text, smith.ChatOptions{Escalate: b.Escalate, Web: b.Web, Context: b.Context})
	if err != nil {
		writeInternalError(w, fmt.Errorf("chat failed: %w", err))
		return
	}
	s.audit(r, identity(r).Name, "smith_chat", fmt.Sprintf("%d", convID), "")
	writeJSON(w, http.StatusAccepted, smithChatResponse{ConversationID: convID, MessageID: msgID})
}

// ── Settings ─────────────────────────────────────────────────────────────

// smithWebProviderBody is one adapter's config on the wire. APIKey is
// masked on GET (httpapi.maskSecret); on PUT, a value equal to the current
// masked form means "leave unchanged" (resolveAPIKeyWrite) — so a UI
// round-trip (GET then PUT the same struct back) can never clobber a real
// key with its own mask.
type smithWebProviderBody struct {
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key"`
}

type smithWebBody struct {
	Enabled       bool                 `json:"enabled"`
	ProviderOrder []string             `json:"provider_order"`
	CacheTTL      string               `json:"cache_ttl"`
	Searxng       smithWebProviderBody `json:"searxng"`
	Firecrawl     smithWebProviderBody `json:"firecrawl"`
	Direct        smithWebProviderBody `json:"direct"`
	CustomSearch  smithWebProviderBody `json:"customsearch"`
	CustomFetch   smithWebProviderBody `json:"customfetch"`
}

func toSmithWebProviderBody(cfg web.ProviderConfig) smithWebProviderBody {
	return smithWebProviderBody{BaseURL: cfg.BaseURL, Enabled: cfg.Enabled, APIKey: maskSecret(cfg.APIKey)}
}

// resolveAPIKeyWrite implements the masked-value-means-unchanged contract
// described on smithWebProviderBody.
func resolveAPIKeyWrite(submitted, currentReal string) string {
	if submitted == maskSecret(currentReal) {
		return currentReal
	}
	return submitted
}

type smithSettingsResponse struct {
	Model            string               `json:"model"`
	HandoffOfferings []int64              `json:"handoff_offerings"`
	Schedule         smith.Schedule       `json:"schedule"`
	Thresholds       smith.Thresholds     `json:"thresholds"`
	Web              smithWebBody         `json:"web"`
	Tools            smith.ToolsConfig    `json:"tools"`
	Retention        smith.Retention      `json:"retention"`
	SelfReview       smith.SelfReview     `json:"self_review"`
	BrainResidency   smith.BrainResidency `json:"brain_residency"`
}

func (s *Server) smithSettingsView(ctx context.Context) smithSettingsResponse {
	webCfg := s.deps.Smith.WebConfig(ctx)
	return smithSettingsResponse{
		Model:            s.deps.Smith.ModelSetting(ctx),
		HandoffOfferings: s.deps.Smith.HandoffOfferings(ctx),
		Schedule:         s.deps.Smith.Schedule(ctx),
		Thresholds:       s.deps.Smith.Thresholds(ctx),
		Web: smithWebBody{
			Enabled:       webCfg.Enabled,
			ProviderOrder: webCfg.ProviderOrder,
			CacheTTL:      webCfg.CacheTTL.String(),
			Searxng:       toSmithWebProviderBody(webCfg.Searxng),
			Firecrawl:     toSmithWebProviderBody(webCfg.Firecrawl),
			Direct:        toSmithWebProviderBody(webCfg.Direct),
			CustomSearch:  toSmithWebProviderBody(webCfg.CustomSearch),
			CustomFetch:   toSmithWebProviderBody(webCfg.CustomFetch),
		},
		Tools:          s.deps.Smith.ToolsConfig(ctx),
		Retention:      s.deps.Smith.RetentionConfig(ctx),
		SelfReview:     s.deps.Smith.SelfReviewConfig(ctx),
		BrainResidency: s.deps.Smith.BrainResidencyConfig(ctx),
	}
}

// handleSmithSettingsGet — GET /api/v1/smith/settings (operator).
func (s *Server) handleSmithSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.smithSettingsView(r.Context()))
}

type smithScheduleBody struct {
	Quick   string `json:"quick"`
	Deep    string `json:"deep"`
	Enabled bool   `json:"enabled"`
}

type smithThresholdsBody struct {
	GTTWarnPct  float64 `json:"gtt_warn_pct"`
	GTTCritPct  float64 `json:"gtt_crit_pct"`
	DiskWarnPct float64 `json:"disk_warn_pct"`
	DiskCritPct float64 `json:"disk_crit_pct"`
}

// smithSettingsBody is PUT /api/v1/smith/settings's request body — every
// field optional, only the ones present are validated + written (five
// independent settings-key groups, not sub-fields of one JSON blob, so
// there's no "accidentally null a sibling field" risk the way there would
// be with a single-blob settings group). Web (P5) follows the same
// whole-object-replace-when-present convention as Schedule/Thresholds, not
// a per-field patch.
type smithToolsBody struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
	MaxRounds int    `json:"max_rounds"`
}

type smithRetentionBody struct {
	Enabled         bool `json:"enabled"`
	OKDays          int  `json:"ok_days"`
	InfoHours       int  `json:"info_hours"`
	WarnCritDays    int  `json:"warn_crit_days"`
	WebCacheDays    int  `json:"web_cache_days"`
	WebCacheMaxRows int  `json:"web_cache_max_rows"`
}

type smithSelfReviewBody struct {
	Enabled      bool `json:"enabled"`
	GraceMinutes int  `json:"grace_minutes"`
}

type smithBrainResidencyBody struct {
	StayResident bool `json:"stay_resident"`
}

type smithSettingsBody struct {
	Model            *string                  `json:"model"`
	HandoffOfferings *[]int64                 `json:"handoff_offerings"`
	Schedule         *smithScheduleBody       `json:"schedule"`
	Thresholds       *smithThresholdsBody     `json:"thresholds"`
	Web              *smithWebBody            `json:"web"`
	Tools            *smithToolsBody          `json:"tools"`
	Retention        *smithRetentionBody      `json:"retention"`
	SelfReview       *smithSelfReviewBody     `json:"self_review"`
	BrainResidency   *smithBrainResidencyBody `json:"brain_residency"`
}

// handleSmithSettingsPut — PUT /api/v1/smith/settings (operator). Validates
// every provided field before writing any of them (all-or-nothing per
// request, matching the Monitor settings precedent), then writes each
// provided field to its own smith.* settings key.
func (s *Server) handleSmithSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var b smithSettingsBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}

	fieldErrs := map[string]string{}
	if b.HandoffOfferings != nil {
		for _, id := range *b.HandoffOfferings {
			if id <= 0 {
				fieldErrs["handoff_offerings"] = "offering ids must be positive"
				break
			}
		}
	}
	if b.Schedule != nil {
		if _, err := time.ParseDuration(b.Schedule.Quick); err != nil {
			fieldErrs["schedule.quick"] = "must be a valid positive duration (e.g. 60m)"
		}
		if _, err := time.ParseDuration(b.Schedule.Deep); err != nil {
			fieldErrs["schedule.deep"] = "must be a valid positive duration (e.g. 24h)"
		}
	}
	if b.Thresholds != nil {
		for name, v := range map[string]float64{
			"gtt_warn_pct": b.Thresholds.GTTWarnPct, "gtt_crit_pct": b.Thresholds.GTTCritPct,
			"disk_warn_pct": b.Thresholds.DiskWarnPct, "disk_crit_pct": b.Thresholds.DiskCritPct,
		} {
			if v <= 0 || v > 100 {
				fieldErrs["thresholds."+name] = "must be between 0 and 100"
			}
		}
	}
	if b.Web != nil {
		for _, name := range b.Web.ProviderOrder {
			if name != "searxng" && name != "firecrawl" && name != "direct" && name != "customsearch" && name != "customfetch" {
				fieldErrs["web.provider_order"] = "unknown provider " + name
				break
			}
		}
		if b.Web.CacheTTL != "" {
			if d, err := time.ParseDuration(b.Web.CacheTTL); err != nil || d <= 0 {
				fieldErrs["web.cache_ttl"] = "must be a valid positive duration (e.g. 6h)"
			}
		}
		for _, p := range []struct {
			key  string
			body smithWebProviderBody
		}{{"web.searxng", b.Web.Searxng}, {"web.firecrawl", b.Web.Firecrawl}, {"web.direct", b.Web.Direct}, {"web.customsearch", b.Web.CustomSearch}, {"web.customfetch", b.Web.CustomFetch}} {
			if p.body.BaseURL == "" {
				continue
			}
			if u, err := url.Parse(p.body.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fieldErrs[p.key+".base_url"] = "must be a valid http(s) URL"
			}
		}
	}
	if b.Tools != nil {
		switch b.Tools.Mode {
		case "auto", "native", "fenced", "off":
		default:
			fieldErrs["tools.mode"] = "must be one of auto|native|fenced|off"
		}
		if b.Tools.MaxRounds < 1 || b.Tools.MaxRounds > 8 {
			fieldErrs["tools.max_rounds"] = "must be between 1 and 8"
		}
	}
	if b.Retention != nil {
		if b.Retention.WebCacheMaxRows < 0 {
			fieldErrs["retention.web_cache_max_rows"] = "must not be negative"
		}
	}
	if b.SelfReview != nil {
		if b.SelfReview.GraceMinutes < 0 {
			fieldErrs["self_review.grace_minutes"] = "must not be negative"
		}
	}
	if len(fieldErrs) > 0 {
		writeValidationError(w, fieldErrs)
		return
	}

	ctx := r.Context()
	if b.Model != nil {
		raw, _ := json.Marshal(*b.Model)
		if err := s.deps.Settings.Set(ctx, smith.SettingModel, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.HandoffOfferings != nil {
		raw, _ := json.Marshal(*b.HandoffOfferings)
		if err := s.deps.Settings.Set(ctx, smith.SettingHandoffOfferings, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.Schedule != nil {
		raw, _ := json.Marshal(b.Schedule)
		if err := s.deps.Settings.Set(ctx, smith.SettingSchedule, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.Thresholds != nil {
		raw, _ := json.Marshal(b.Thresholds)
		if err := s.deps.Settings.Set(ctx, smith.SettingThresholds, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	webChanged := false
	if b.Web != nil {
		current := s.deps.Smith.WebConfig(ctx)
		writes := []struct {
			key string
			cfg web.ProviderConfig
		}{
			{web.SettingSearxng, web.ProviderConfig{BaseURL: b.Web.Searxng.BaseURL, Enabled: b.Web.Searxng.Enabled, APIKey: resolveAPIKeyWrite(b.Web.Searxng.APIKey, current.Searxng.APIKey)}},
			{web.SettingFirecrawl, web.ProviderConfig{BaseURL: b.Web.Firecrawl.BaseURL, Enabled: b.Web.Firecrawl.Enabled, APIKey: resolveAPIKeyWrite(b.Web.Firecrawl.APIKey, current.Firecrawl.APIKey)}},
			{web.SettingDirect, web.ProviderConfig{BaseURL: b.Web.Direct.BaseURL, Enabled: b.Web.Direct.Enabled, APIKey: resolveAPIKeyWrite(b.Web.Direct.APIKey, current.Direct.APIKey)}},
			{web.SettingCustomSearch, web.ProviderConfig{BaseURL: b.Web.CustomSearch.BaseURL, Enabled: b.Web.CustomSearch.Enabled, APIKey: resolveAPIKeyWrite(b.Web.CustomSearch.APIKey, current.CustomSearch.APIKey)}},
			{web.SettingCustomFetch, web.ProviderConfig{BaseURL: b.Web.CustomFetch.BaseURL, Enabled: b.Web.CustomFetch.Enabled, APIKey: resolveAPIKeyWrite(b.Web.CustomFetch.APIKey, current.CustomFetch.APIKey)}},
		}
		enabledRaw, _ := json.Marshal(b.Web.Enabled)
		if err := s.deps.Settings.Set(ctx, web.SettingEnabled, enabledRaw); err != nil {
			writeInternalError(w, err)
			return
		}
		orderRaw, _ := json.Marshal(b.Web.ProviderOrder)
		if err := s.deps.Settings.Set(ctx, web.SettingProviderOrder, orderRaw); err != nil {
			writeInternalError(w, err)
			return
		}
		if b.Web.CacheTTL != "" {
			ttlRaw, _ := json.Marshal(b.Web.CacheTTL)
			if err := s.deps.Settings.Set(ctx, web.SettingCacheTTL, ttlRaw); err != nil {
				writeInternalError(w, err)
				return
			}
		}
		for _, wr := range writes {
			raw, _ := json.Marshal(wr.cfg)
			if err := s.deps.Settings.Set(ctx, wr.key, raw); err != nil {
				writeInternalError(w, err)
				return
			}
		}
		webChanged = true
	}
	if b.Tools != nil {
		raw, _ := json.Marshal(b.Tools)
		if err := s.deps.Settings.Set(ctx, smith.SettingTools, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.Retention != nil {
		raw, _ := json.Marshal(b.Retention)
		if err := s.deps.Settings.Set(ctx, smith.SettingRetention, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.SelfReview != nil {
		raw, _ := json.Marshal(b.SelfReview)
		if err := s.deps.Settings.Set(ctx, smith.SettingSelfReview, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.BrainResidency != nil {
		raw, _ := json.Marshal(b.BrainResidency)
		if err := s.deps.Settings.Set(ctx, smith.SettingBrainResidency, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}

	s.audit(r, identity(r).Name, "smith_settings_update", "smith.settings", "")
	if webChanged {
		// Probe on save (docs/v5-smith.md §4.8) — backgrounded so a slow
		// provider doesn't delay the settings response.
		goSafe("smith_probe_web", func() { s.deps.Smith.ProbeWeb(context.Background()) })
	}
	writeJSON(w, http.StatusOK, s.smithSettingsView(ctx))
}

// handleSmithWebProbe — POST /api/v1/smith/web/probe (operator). A manual
// "re-probe now" — runs synchronously (the timeouts involved are small: 5s
// per provider, at most 2 providers) so the response carries fresh
// reachability instead of the caller needing a second round-trip.
func (s *Server) handleSmithWebProbe(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	ctx := r.Context()
	s.deps.Smith.ProbeWeb(ctx)
	s.audit(r, identity(r).Name, "smith_web_probe", "smith.web", "")
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.deps.Smith.WebProviders(ctx)})
}

// ── Investigation Tier 2 analysis ───────────────────────────────────────

// handleSmithInvestigationAnalyze runs one Tier 2 commentary turn against
// an investigation's evidence (docs/v5-smith.md §5 — specified in P0,
// unrouted until this phase).
func (s *Server) handleSmithInvestigationAnalyze(w http.ResponseWriter, r *http.Request) {
	if !s.smithOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid investigation id")
		return
	}
	convID, msgID, err := s.deps.Smith.AnalyzeInvestigation(r.Context(), id)
	if err != nil {
		writeInternalError(w, fmt.Errorf("analyze failed: %w", err))
		return
	}
	s.audit(r, identity(r).Name, "smith_investigation_analyze", fmt.Sprintf("%d", id), "")
	writeJSON(w, http.StatusAccepted, smithChatResponse{ConversationID: convID, MessageID: msgID})
}

// composeContextSeedMessage builds the canonical seed user message from an
// attached context array (§3.4, R5). One line per context item, quoting the
// error code, message, source, and timestamp. The composed text passes
// through smith.Chat → scrubSecretPatterns (redact.go) like everything else.
//
// Format:
//
//	[alert KFD_EVICTION] GPU queue eviction detected (source: gpu_hang, 2026-08-14 14:23:20)
func composeContextSeedMessage(items []smith.ChatContext) string {
	var b strings.Builder
	for _, item := range items {
		code := item.Code
		if code == "" {
			code = "ALERT"
		}
		fmt.Fprintf(&b, "[alert %s] %s", code, item.Message)
		if item.Source != "" {
			fmt.Fprintf(&b, " (source: %s", item.Source)
			if item.At > 0 {
				fmt.Fprintf(&b, ", %s", time.Unix(item.At, 0).UTC().Format("2006-01-02 15:04:05"))
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

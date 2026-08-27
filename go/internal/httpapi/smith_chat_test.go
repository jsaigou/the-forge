// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/smith/web"
)

func TestSmithChatUnwired(t *testing.T) {
	s := newTestServer(t) // no Smith wired
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/smith/conversations"},
		{"POST", "/api/v1/smith/conversations"},
		{"POST", "/api/v1/smith/chat"},
		{"GET", "/api/v1/smith/settings"},
		{"POST", "/api/v1/smith/web/probe"},
	} {
		w := do(t, s, authedRequest(route.method, route.path, bytes.NewBufferString(`{}`)))
		if w.Code != 503 {
			t.Errorf("%s %s = %d, want 503 when smith unwired", route.method, route.path, w.Code)
		}
	}
}

func TestSmithChatRoleGate(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "viewer", Role: authz.RoleViewer})
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/smith/conversations"},
		{"POST", "/api/v1/smith/conversations"},
		{"GET", "/api/v1/smith/conversations/1"},
		{"DELETE", "/api/v1/smith/conversations/1"},
		{"POST", "/api/v1/smith/chat"},
		{"GET", "/api/v1/smith/settings"},
		{"PUT", "/api/v1/smith/settings"},
		{"POST", "/api/v1/smith/investigations/1/analyze"},
		{"POST", "/api/v1/smith/web/probe"},
	} {
		w := do(t, s, authedRequest(route.method, route.path, bytes.NewBufferString(`{}`)))
		if w.Code != 403 {
			t.Errorf("%s %s as viewer = %d, want 403", route.method, route.path, w.Code)
		}
	}
}

func TestSmithConversationsCRUD(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("POST", "/api/v1/smith/conversations", bytes.NewBufferString(`{"title":"outage diagnosis"}`)))
	if w.Code != 201 {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var created smith.Conversation
	decodeJSON(t, w.Body, &created)
	if created.ID == 0 || created.Title != "outage diagnosis" {
		t.Fatalf("created = %+v", created)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/smith/conversations", nil))
	if w.Code != 200 {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var list smithConversationsResponse
	decodeJSON(t, w.Body, &list)
	if list.Count != 1 {
		t.Errorf("list.Count = %d, want 1", list.Count)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/smith/conversations/"+itoa(created.ID), nil))
	if w.Code != 200 {
		t.Fatalf("detail = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail smithConversationDetailResponse
	decodeJSON(t, w.Body, &detail)
	if detail.ID != created.ID || len(detail.Messages) != 0 {
		t.Errorf("detail = %+v, want empty transcript", detail)
	}

	w = do(t, s, authedRequest("DELETE", "/api/v1/smith/conversations/"+itoa(created.ID), nil))
	if w.Code != 200 {
		t.Fatalf("delete = %d, want 200", w.Code)
	}
	w = do(t, s, authedRequest("GET", "/api/v1/smith/conversations/"+itoa(created.ID), nil))
	if w.Code != 404 {
		t.Errorf("get-after-delete = %d, want 404", w.Code)
	}
}

// TestSmithChatDeterministicFlow exercises POST /chat end to end against a
// deterministic-only brain (serverWithSmith wires no smith.model) — 202 with
// an assistant message ID, then (once the background turn finishes) a
// finalized deterministic answer visible via GET /conversations/{id}.
func TestSmithChatDeterministicFlow(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("POST", "/api/v1/smith/chat", bytes.NewBufferString(`{"text":"why is the box slow?"}`)))
	if w.Code != 202 {
		t.Fatalf("chat = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp smithChatResponse
	decodeJSON(t, w.Body, &resp)
	if resp.ConversationID == 0 || resp.MessageID == 0 {
		t.Fatalf("resp = %+v, want non-zero ids", resp)
	}

	deadline := time.Now().Add(time.Second)
	var detail smithConversationDetailResponse
	for time.Now().Before(deadline) {
		w = do(t, s, authedRequest("GET", "/api/v1/smith/conversations/"+itoa(resp.ConversationID), nil))
		decodeJSON(t, w.Body, &detail)
		done := false
		for _, m := range detail.Messages {
			if m.ID == resp.MessageID && m.Content != "" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// S4: with a brain model configured but unloadable here, the turn
	// degrades asynchronously — transcript is user + degraded assistant
	// (deterministic TIER, grounded content) + an explanatory notice.
	if len(detail.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user + degraded assistant + notice)", len(detail.Messages))
	}
	if detail.Messages[0].Kind != smith.MsgKindUser {
		t.Errorf("messages[0].Kind = %q, want user", detail.Messages[0].Kind)
	}
	if detail.Messages[1].Kind != smith.MsgKindReasoning {
		t.Errorf("messages[1].Kind = %q, want %q (placeholder kind persists through degrade)", detail.Messages[1].Kind, smith.MsgKindReasoning)
	}
	if detail.Messages[1].Content == "" || detail.Messages[1].Content[:6] != "Brain:" {
		t.Errorf("messages[1].Content = %.60q, want the grounded deterministic answer", detail.Messages[1].Content)
	}
	if detail.Messages[2].Kind != "notice" {
		t.Errorf("messages[2].Kind = %q, want %q", detail.Messages[2].Kind, "notice")
	}

	w = do(t, s, authedRequest("POST", "/api/v1/smith/chat", bytes.NewBufferString(`{"text":""}`)))
	if w.Code != 422 {
		t.Errorf("chat(empty text) = %d, want 422", w.Code)
	}
}

func TestSmithSettingsGetPut(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	if w.Code != 200 {
		t.Fatalf("get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got smithSettingsResponse
	decodeJSON(t, w.Body, &got)
	if got.Schedule.Quick == "" || got.Schedule.Deep == "" {
		t.Errorf("expected default schedule, got %+v", got.Schedule)
	}
	if got.BrainChain == nil {
		t.Errorf("brain_chain should coalesce nil -> [] at the JSON boundary, got nil")
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings",
		bytes.NewBufferString(`{"model":"ornith-35b","handoff_offerings":[3,7],"brain_chain":["gemma4-e4b-qat","qwen38-27b"],"build_refresh_watchlist":["CVE","breaking"],"comfyui_keep_files":["/models/checkpoints/keep-me.safetensors"],"schedule":{"quick":"30m","deep":"12h","enabled":true},"thresholds":{"gtt_warn_pct":80,"gtt_crit_pct":90,"disk_warn_pct":80,"disk_crit_pct":90,"device_lost_window_minutes":20,"build_refresh_behind_n":250}}`)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var updated smithSettingsResponse
	decodeJSON(t, w.Body, &updated)
	if updated.Model != "ornith-35b" {
		t.Errorf("model = %q, want ornith-35b", updated.Model)
	}
	if len(updated.HandoffOfferings) != 2 || updated.HandoffOfferings[0] != 3 {
		t.Errorf("handoff_offerings = %v, want [3 7]", updated.HandoffOfferings)
	}
	if len(updated.BrainChain) != 2 || updated.BrainChain[0] != "gemma4-e4b-qat" || updated.BrainChain[1] != "qwen38-27b" {
		t.Errorf("brain_chain = %v, want [gemma4-e4b-qat qwen38-27b]", updated.BrainChain)
	}
	if len(updated.BuildRefreshWatchlist) != 2 || updated.BuildRefreshWatchlist[0] != "CVE" {
		t.Errorf("build_refresh_watchlist = %v, want [CVE breaking]", updated.BuildRefreshWatchlist)
	}
	if len(updated.ComfyUIKeepFiles) != 1 || updated.ComfyUIKeepFiles[0] != "/models/checkpoints/keep-me.safetensors" {
		t.Errorf("comfyui_keep_files = %v, want [/models/checkpoints/keep-me.safetensors]", updated.ComfyUIKeepFiles)
	}
	if updated.Schedule.Quick != "30m" || updated.Schedule.Deep != "12h" {
		t.Errorf("schedule = %+v, want 30m/12h", updated.Schedule)
	}
	if updated.Thresholds.GTTWarnPct != 80 {
		t.Errorf("gtt_warn_pct = %v, want 80", updated.Thresholds.GTTWarnPct)
	}
	// Previously unwritable despite being GET-readable — see smithThresholdsBody's
	// doc comment (S7-followup, 2026-08-26).
	if updated.Thresholds.DeviceLostWindowMinutes != 20 {
		t.Errorf("device_lost_window_minutes = %v, want 20", updated.Thresholds.DeviceLostWindowMinutes)
	}
	if updated.Thresholds.BuildRefreshBehindN != 250 {
		t.Errorf("build_refresh_behind_n = %v, want 250", updated.Thresholds.BuildRefreshBehindN)
	}

	// A GET afterwards must reflect the PUT (settings live-read, not cached).
	w = do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	var reread smithSettingsResponse
	decodeJSON(t, w.Body, &reread)
	if reread.Model != "ornith-35b" {
		t.Errorf("reread model = %q, want ornith-35b", reread.Model)
	}

	// Validation: a bad schedule duration rejects the whole request (nothing
	// partially written) and the model set above survives untouched.
	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings",
		bytes.NewBufferString(`{"model":"should-not-apply","schedule":{"quick":"not-a-duration","deep":"12h"}}`)))
	if w.Code != 422 {
		t.Fatalf("put(bad schedule) = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	decodeJSON(t, w.Body, &reread)
	if reread.Model != "ornith-35b" {
		t.Errorf("model after a rejected PUT = %q, want it unchanged at ornith-35b", reread.Model)
	}
}

// TestSmithSettings_ToolsAndRetention covers P7's two new settings groups:
// defaults reflect smith.DefaultToolsConfig/DefaultRetention when unset (no
// migration seeds these keys — code defaults only), a valid PUT round-trips,
// and invalid values reject the whole request per the existing
// all-or-nothing fieldErrs convention.
func TestSmithSettings_ToolsAndRetention(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	if w.Code != 200 {
		t.Fatalf("get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got smithSettingsResponse
	decodeJSON(t, w.Body, &got)
	if !got.Tools.Enabled || got.Tools.Mode != "auto" || got.Tools.MaxRounds != 6 {
		t.Errorf("default tools = %+v, want {enabled:true mode:auto max_rounds:6}", got.Tools)
	}
	if !got.Retention.Enabled || got.Retention.OKDays != 7 || got.Retention.WarnCritDays != 180 {
		t.Errorf("default retention = %+v", got.Retention)
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings",
		bytes.NewBufferString(`{"tools":{"enabled":false,"mode":"fenced","max_rounds":2},`+
			`"retention":{"enabled":true,"ok_days":1,"info_hours":5,"warn_crit_days":30,"web_cache_days":10,"web_cache_max_rows":500}}`)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var updated smithSettingsResponse
	decodeJSON(t, w.Body, &updated)
	if updated.Tools.Enabled || updated.Tools.Mode != "fenced" || updated.Tools.MaxRounds != 2 {
		t.Errorf("tools after PUT = %+v", updated.Tools)
	}
	if updated.Retention.OKDays != 1 || updated.Retention.WebCacheMaxRows != 500 {
		t.Errorf("retention after PUT = %+v", updated.Retention)
	}

	// Validation: an out-of-range mode/max_rounds/negative row cap rejects
	// the whole request, nothing partially written.
	for _, body := range []string{
		`{"tools":{"enabled":true,"mode":"bogus","max_rounds":4}}`,
		`{"tools":{"enabled":true,"mode":"auto","max_rounds":0}}`,
		`{"tools":{"enabled":true,"mode":"auto","max_rounds":9}}`,
		`{"retention":{"enabled":true,"web_cache_max_rows":-1}}`,
	} {
		w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings", bytes.NewBufferString(body)))
		if w.Code != 422 {
			t.Errorf("put(%s) = %d, want 422; body=%s", body, w.Code, w.Body.String())
		}
	}
	w = do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	decodeJSON(t, w.Body, &got)
	if got.Tools.Mode != "fenced" {
		t.Errorf("tools after rejected PUTs = %+v, want unchanged at fenced", got.Tools)
	}
}

func TestSmithSettings_BrainResidencyDefaultAndRoundTrip(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	if w.Code != 200 {
		t.Fatalf("get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got smithSettingsResponse
	decodeJSON(t, w.Body, &got)
	if got.BrainResidency.StayResident {
		t.Errorf("default brain_residency.stay_resident = true, want false (never loaded by default)")
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings",
		bytes.NewBufferString(`{"brain_residency":{"stay_resident":true}}`)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var updated smithSettingsResponse
	decodeJSON(t, w.Body, &updated)
	if !updated.BrainResidency.StayResident {
		t.Errorf("brain_residency after PUT = %+v, want stay_resident:true", updated.BrainResidency)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	decodeJSON(t, w.Body, &got)
	if !got.BrainResidency.StayResident {
		t.Errorf("brain_residency after re-GET = %+v, want it to have persisted", got.BrainResidency)
	}
}

func TestSmithSettings_WebDefaultsAndRoundTrip(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	if w.Code != 200 {
		t.Fatalf("get = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got smithSettingsResponse
	decodeJSON(t, w.Body, &got)
	// Migration 0037 seeds smith.web.enabled=true (a generic preference —
	// kept), but migration 0063 conditionally clears the seeded searxng/
	// firecrawl base URLs: those are deployment data under the two-layer
	// knowledge architecture and are provisioned per install via
	// `forge smith import-local`, never shipped. A fresh store.Open (as
	// serverWithSmith uses) therefore observes empty base URLs with the
	// always-present direct adapter enabled.
	if !got.Web.Enabled {
		t.Errorf("web.enabled should be seeded true by migration 0037, got %+v", got.Web)
	}
	if got.Web.Searxng.BaseURL != "" || got.Web.Firecrawl.BaseURL != "" {
		t.Errorf("web.searxng/firecrawl base_url must NOT ship seeded (0063 clears them; provision via import-local), got %+v", got.Web)
	}
	if got.Web.Direct.Enabled != true {
		t.Errorf("web.direct.enabled should be seeded true (the always-present terminus), got %+v", got.Web.Direct)
	}

	body := `{"web":{"enabled":true,"provider_order":["searxng","firecrawl","direct"],"cache_ttl":"3h",` +
		`"searxng":{"base_url":"https://searxng.example","enabled":true,"api_key":""},` +
		`"firecrawl":{"base_url":"https://firecrawl.example","enabled":true,"api_key":"sk-real-secret-value"},` +
		`"direct":{"base_url":"","enabled":true,"api_key":""}}}`
	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings", bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("put = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var updated smithSettingsResponse
	decodeJSON(t, w.Body, &updated)
	if !updated.Web.Enabled || updated.Web.Searxng.BaseURL != "https://searxng.example" {
		t.Errorf("web after PUT = %+v, want it applied", updated.Web)
	}
	if updated.Web.Firecrawl.APIKey == "sk-real-secret-value" {
		t.Error("GET/the PUT response must never echo the real api_key — it should be masked")
	}
	if updated.Web.Firecrawl.APIKey == "" {
		t.Error("a non-empty api_key should mask to a non-empty prefix, not disappear entirely")
	}

	// Re-PUT the exact masked value the previous response returned — must
	// NOT clobber the real key (resolveAPIKeyWrite's "unchanged" contract).
	maskedKey := updated.Web.Firecrawl.APIKey
	body2 := `{"web":{"enabled":true,"provider_order":["searxng","firecrawl","direct"],"cache_ttl":"3h",` +
		`"searxng":{"base_url":"https://searxng.example","enabled":true,"api_key":""},` +
		`"firecrawl":{"base_url":"https://firecrawl.example","enabled":true,"api_key":"` + maskedKey + `"},` +
		`"direct":{"base_url":"","enabled":true,"api_key":""}}}`
	w = do(t, s, authedRequest("PUT", "/api/v1/smith/settings", bytes.NewBufferString(body2)))
	if w.Code != 200 {
		t.Fatalf("put(masked roundtrip) = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	realCfg := web.LoadConfig(context.Background(), s.deps.Settings)
	if realCfg.Firecrawl.APIKey != "sk-real-secret-value" {
		t.Errorf("real api_key was clobbered by its own masked round-trip: got %q", realCfg.Firecrawl.APIKey)
	}
}

func TestSmithSettings_WebValidation(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	var before smithSettingsResponse
	decodeJSON(t, w.Body, &before)

	cases := []struct {
		name string
		body string
	}{
		{"unknown provider", `{"web":{"provider_order":["not-a-real-provider"],"searxng":{},"firecrawl":{},"direct":{}}}`},
		{"bad cache_ttl", `{"web":{"cache_ttl":"not-a-duration","searxng":{},"firecrawl":{},"direct":{}}}`},
		{"bad base_url", `{"web":{"searxng":{"base_url":"not a url"},"firecrawl":{},"direct":{}}}`},
	}
	for _, c := range cases {
		w := do(t, s, authedRequest("PUT", "/api/v1/smith/settings", bytes.NewBufferString(c.body)))
		if w.Code != 422 {
			t.Errorf("%s: put = %d, want 422; body=%s", c.name, w.Code, w.Body.String())
		}
	}

	// Nothing partially written by any of the rejected requests — the
	// settings must read back exactly as they did before any of them ran.
	w = do(t, s, authedRequest("GET", "/api/v1/smith/settings", nil))
	var after smithSettingsResponse
	decodeJSON(t, w.Body, &after)
	if !reflect.DeepEqual(after.Web, before.Web) {
		t.Errorf("web settings changed after rejected PUTs: before=%+v after=%+v", before.Web, after.Web)
	}
}

func TestSmithWebProbe(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("POST", "/api/v1/smith/web/probe", nil))
	if w.Code != 200 {
		t.Fatalf("probe = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Providers []web.ProviderStatus `json:"providers"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Providers == nil {
		t.Error("providers should be present (empty is fine, nil is not)")
	}
}

func TestSmithInvestigationAnalyze(t *testing.T) {
	s, db, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("POST", "/api/v1/smith/investigations", bytes.NewBufferString(`{"summary":"box seems slow"}`)))
	if w.Code != 201 {
		t.Fatalf("create investigation = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var inv smith.Investigation
	decodeJSON(t, w.Body, &inv)

	w = do(t, s, authedRequest("POST", "/api/v1/smith/investigations/"+itoa(inv.ID)+"/analyze", nil))
	if w.Code != 202 {
		t.Fatalf("analyze = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var resp smithChatResponse
	decodeJSON(t, w.Body, &resp)
	if resp.ConversationID == 0 || resp.MessageID == 0 {
		t.Fatalf("resp = %+v, want non-zero ids", resp)
	}

	// The investigation must now carry the linked conversation_id.
	var linked int64
	if err := db.SQL().QueryRow(`SELECT conversation_id FROM smith_investigations WHERE id = ?`, inv.ID).Scan(&linked); err != nil {
		t.Fatalf("query linked conversation: %v", err)
	}
	if linked != resp.ConversationID {
		t.Errorf("linked conversation_id = %d, want %d", linked, resp.ConversationID)
	}
}

// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/store"
)

// tools.go — the P7 read-only tool set (docs/v5-smith.md §9's "Tier-2
// read-only tool loop"). Mirrors checks.go's registry idiom: data-ish Go,
// not store-backed, stable execution/display order.
//
// The structural guarantee that mutating tools are never exposed is that
// Tool.Run takes a *ToolEnv, never *Smith — the exact CheckEnv precedent
// (checks.go). A tool has no handle on s.d.Store, proposeFrom, executeAction,
// or the settings writer; adding a mutating capability requires widening
// ToolEnv, an obvious reviewable diff, not a hidden method call inside a
// closure. TestToolEnv_ShapeFrozen (tools_test.go) locks the field set, and
// TestTools_NoWriteAgainstRealStore proves it empirically against a real DB.
//
// Deliberately excluded from v1, with reasons: self_context (already in
// every system prompt via buildContext — a tool would just be duplicate
// tokens); run_sweep (persists findings + calls proposeFrom, i.e. creates
// smith_actions rows — an LLM-drivable write); and, a real deviation from
// §4.3's "it can only emit proposal drafts" — any propose_draft tool, since
// that is also a write to smith_actions. Proposals keep coming from
// proposeFrom off deterministic findings only; this is recorded as a
// deferral in docs/v5-smith.md.

// defaultToolTimeout bounds a tool call with no more specific Timeout.
const defaultToolTimeout = 20 * time.Second

// runCheckToolTimeout is wider than defaultToolTimeout: a deep check
// (blocked_work_recheck) genuinely makes network calls.
const runCheckToolTimeout = 60 * time.Second

// catalogReader is the narrow read-only view of store.Catalog handed to
// tools — store.Catalog has ~60 methods including CreateConfig/DeleteConfig;
// handing the whole interface to a tool would defeat ToolEnv's purpose.
type catalogReader interface {
	ListConfigs(ctx context.Context) ([]store.Config, error)
	ConfigByName(ctx context.Context, name string) (store.Config, error)
	ListOfferings(ctx context.Context) ([]store.Offering, error)
}

// webReader is the narrow read-only view of web.Service handed to tools —
// Probe/Providers (reachability control) stay out.
type webReader interface {
	Search(ctx context.Context, q string, limit int) ([]web.Result, error)
	Fetch(ctx context.Context, url string) (*web.Document, error)
}

// ToolEnv carries everything a tool's Run may read. Built once per turn by
// Smith.toolEnv. Every field is nil-tolerant, matching CheckEnv/house
// convention — a tool with a nil dep returns a clean sentinel error, never
// panics.
type ToolEnv struct {
	RunSelected  func(ctx context.Context, checkIDs []string) ([]Finding, error)
	KBSearch     func(ctx context.Context, q string, limit int) ([]KBResult, error)
	ListFindings func(ctx context.Context, since time.Time, sev string, limit int) ([]StoredFinding, error)
	Catalog      catalogReader // nil-tolerant
	Web          webReader     // nil-tolerant
	Now          func() time.Time
}

// toolEnv builds the per-turn tool environment. Catalog/Web are nil exactly
// when Deps.Catalog/Deps.Web are nil — no synthetic non-nil wrapper that
// would hide the "unwired" case from a tool's own nil check.
func (s *Smith) toolEnv(_ context.Context) *ToolEnv {
	env := &ToolEnv{
		RunSelected: func(ctx context.Context, checkIDs []string) ([]Finding, error) {
			checks, err := selectChecks("", checkIDs)
			if err != nil {
				return nil, err
			}
			return runSelected(ctx, checks, s.checkEnv(ctx)), nil
		},
		KBSearch:     s.KBSearch,
		ListFindings: s.ListFindings,
		Now:          s.d.Now,
	}
	if s.d.Catalog != nil {
		env.Catalog = s.d.Catalog
	}
	if s.d.Web != nil {
		env.Web = s.d.Web
	}
	return env
}

// Tool is one read-only capability offered to the Tier 2 brain.
type Tool struct {
	ID          string
	Description string
	Params      map[string]any // JSON Schema object
	// Network marks a tool that may make a non-loopback request — it debits
	// the per-turn outbound budget (tool_loop.go). Static per tool rather
	// than introspecting args: run_check is Network:true even though most
	// check IDs are loopback-only, because blocked_work_recheck genuinely
	// fetches — over-debiting by at most one unit is the safe side to err
	// on, and cheaper than teaching the budget which check IDs reach out.
	Network bool
	Timeout time.Duration // 0 ⇒ defaultToolTimeout
	Run     func(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error)
}

// webToolResult is what a web_search/web_fetch Run returns — the payload
// handed back to the brain PLUS the citation records tool_loop.go merges
// into the answer message's existing `sources` column (setMessageSources),
// lighting up SourcesList.tsx for free rather than inventing a second
// rendering path.
type webToolResult struct {
	Payload any
	Sources []MessageSource
}

// toolFinding is the compact, redacted projection every finding-bearing
// tool result uses.
type toolFinding struct {
	CheckID   string         `json:"check_id"`
	Severity  string         `json:"severity"`
	Summary   string         `json:"summary"`
	Evidence  map[string]any `json:"evidence"`
	CreatedAt int64          `json:"created_at,omitempty"`
}

func projectFinding(f Finding) toolFinding {
	ev, _ := redactValue(f.Evidence).(map[string]any)
	return toolFinding{CheckID: f.CheckID, Severity: string(f.Severity), Summary: scrubSecretPatterns(f.Summary), Evidence: ev}
}

func projectStoredFinding(f StoredFinding) toolFinding {
	ev, _ := redactValue(evidenceFromJSON(f.Evidence)).(map[string]any)
	return toolFinding{
		CheckID: f.CheckID, Severity: string(f.Severity), Summary: scrubSecretPatterns(f.Summary),
		Evidence: ev, CreatedAt: f.CreatedAt.Unix(),
	}
}

// objectSchema is a small helper for the Params literals below — a plain
// JSON-Schema object with the given properties/required list.
func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// toolRegistry is the v1 tool catalog. Order here is the stable
// display/enumeration order.
var toolRegistry = []Tool{
	{
		ID:          "run_check",
		Description: "Run one or more of smith's deterministic checks right now and return their findings. Does not persist or open any action — a read, not a sweep.",
		Params: objectSchema(map[string]any{
			"check_ids": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"minItems": 1, "maxItems": 4,
				"description": "Known check IDs, e.g. gtt_ceiling, disk_space, n_ctx_actual, gpu_hang, slot_agreement.",
			},
		}, "check_ids"),
		Network: true, // blocked_work_recheck (a valid check_id) really fetches
		Timeout: runCheckToolTimeout,
		Run:     runCheckTool,
	},
	{
		ID:          "list_findings",
		Description: "List smith's most recently persisted findings, optionally filtered by severity.",
		Params: objectSchema(map[string]any{
			"severity":    strProp("ok | info | warn | crit; omit for all severities"),
			"since_hours": intProp("only findings newer than this many hours ago"),
			"limit":       intProp("max rows, default 10, capped at 20"),
		}),
		Run: listFindingsTool,
	},
	{
		ID:          "kb_search",
		Description: "Search smith's knowledge base (embedded docs + live incident/audit history) for text relevant to a question.",
		Params: objectSchema(map[string]any{
			"query": strProp("search text"),
			"limit": intProp("max results, default 3, capped at 5"),
		}, "query"),
		Run: kbSearchTool,
	},
	{
		ID:          "catalog_lookup",
		Description: "Look up the model catalog: local Configs or remote Offerings.",
		Params: objectSchema(map[string]any{
			"kind": strProp(`"configs" or "offerings"`),
			"name": strProp("exact Config name to look up one config; omit to list all"),
		}, "kind"),
		Run: catalogLookupTool,
	},
	{
		ID:          "web_search",
		Description: "Search the public web (only when web research is enabled). Returns titles/URLs/snippets, not full page text.",
		Params: objectSchema(map[string]any{
			"query": strProp("search text"),
			"limit": intProp("max results, default 3, capped at 5"),
		}, "query"),
		Network: true,
		Run:     webSearchTool,
	},
	{
		ID:          "web_fetch",
		Description: "Fetch and extract the readable text of one URL (only when web research is enabled).",
		Params: objectSchema(map[string]any{
			"url": strProp("the URL to fetch"),
		}, "url"),
		Network: true,
		Run:     webFetchTool,
	},
}

// Tools returns the tool catalog. Copies, so callers can't mutate the
// registry (Checks() precedent).
func Tools() []Tool {
	out := make([]Tool, len(toolRegistry))
	copy(out, toolRegistry)
	return out
}

// toolsStatus assembles SelfContext.Tools — a pure read, never resolves
// "auto" (that only happens inside a real turn, tool_loop.go); ResolvedMode
// surfaces whatever was last recorded for model, "" if no turn has run yet.
func (s *Smith) toolsStatus(ctx context.Context, model string) ToolsStatus {
	cfg := s.ToolsConfig(ctx)
	st := ToolsStatus{Enabled: cfg.Enabled, Mode: cfg.Mode, Model: model, Count: len(toolRegistry)}
	if model != "" {
		st.ResolvedMode = s.lastToolMode(model)
	}
	return st
}

// enabledToolsFor returns the tool list for this turn: Tools() minus
// web_search/web_fetch when Deps.Web is nil or smith.web.enabled is
// false, so a turn never advertises a capability that will always fail
// (tool_loop.go's network-budget-exhausted degrade is a separate, narrower
// case — this is the "no web at all" case).
func (s *Smith) enabledToolsFor(ctx context.Context) []Tool {
	webOK := s.d.Web != nil && s.WebConfig(ctx).Enabled
	all := Tools()
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if (t.ID == "web_search" || t.ID == "web_fetch") && !webOK {
			continue
		}
		out = append(out, t)
	}
	return out
}

func findTool(id string) (Tool, bool) {
	for _, t := range toolRegistry {
		if t.ID == id {
			return t, true
		}
	}
	return Tool{}, false
}

func toolIDs(tools []Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.ID
	}
	return out
}

// ── Run implementations ─────────────────────────────────────────────────

func runCheckTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.RunSelected == nil {
		return nil, fmt.Errorf("run_check: checks unavailable")
	}
	var a struct {
		CheckIDs []string `json:"check_ids"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("run_check: invalid arguments: %w", err)
	}
	if len(a.CheckIDs) == 0 {
		return nil, fmt.Errorf("run_check: check_ids must be a non-empty array")
	}
	if len(a.CheckIDs) > 4 {
		a.CheckIDs = a.CheckIDs[:4]
	}
	findings, err := env.RunSelected(ctx, a.CheckIDs)
	if err != nil {
		return nil, fmt.Errorf("run_check: %w", err)
	}
	out := make([]toolFinding, len(findings))
	for i, f := range findings {
		out[i] = projectFinding(f)
	}
	return map[string]any{"findings": out}, nil
}

func listFindingsTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.ListFindings == nil {
		return nil, fmt.Errorf("list_findings: findings store unavailable")
	}
	var a struct {
		Severity   string `json:"severity"`
		SinceHours int    `json:"since_hours"`
		Limit      int    `json:"limit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("list_findings: invalid arguments: %w", err)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}
	now := time.Now
	if env.Now != nil {
		now = env.Now
	}
	var since time.Time
	if a.SinceHours > 0 {
		since = now().Add(-time.Duration(a.SinceHours) * time.Hour)
	}
	switch a.Severity {
	case "", "ok", "info", "warn", "crit":
	default:
		return nil, fmt.Errorf("list_findings: severity must be one of ok|info|warn|crit, got %q", a.Severity)
	}
	findings, err := env.ListFindings(ctx, since, a.Severity, limit)
	if err != nil {
		return nil, fmt.Errorf("list_findings: %w", err)
	}
	out := make([]toolFinding, len(findings))
	for i, f := range findings {
		out[i] = projectStoredFinding(f)
	}
	return map[string]any{"findings": out}, nil
}

func kbSearchTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.KBSearch == nil {
		return nil, fmt.Errorf("kb_search: knowledge base unavailable")
	}
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("kb_search: invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, fmt.Errorf("kb_search: query must not be empty")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 3
	}
	if limit > 5 {
		limit = 5
	}
	results, err := env.KBSearch(ctx, a.Query, limit)
	if err != nil {
		return nil, fmt.Errorf("kb_search: %w", err)
	}
	type kbHit struct {
		Kind  string `json:"kind"`
		Ref   string `json:"ref"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	out := make([]kbHit, len(results))
	for i, r := range results {
		out[i] = kbHit{Kind: r.Kind, Ref: r.Ref, Title: scrubSecretPatterns(r.Title), Body: scrubSecretPatterns(truncateForContext(r.Body, 800))}
	}
	return map[string]any{"results": out}, nil
}

type toolConfigView struct {
	Name       string `json:"name"`
	NCtx       int    `json:"n_ctx"`
	Parallel   int    `json:"parallel"`
	Status     string `json:"status"`
	Visibility string `json:"visibility"`
}

type toolOfferingView struct {
	Provider      string  `json:"provider"`
	WireModel     string  `json:"wire_model"`
	ContextLength int     `json:"context_length"`
	Enabled       bool    `json:"enabled"`
	PriceInPer1M  float64 `json:"price_in_per_1m"`
	PriceOutPer1M float64 `json:"price_out_per_1m"`
	Currency      string  `json:"currency"`
}

func catalogLookupTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.Catalog == nil {
		return nil, fmt.Errorf("catalog_lookup: catalog unavailable")
	}
	var a struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("catalog_lookup: invalid arguments: %w", err)
	}
	switch a.Kind {
	case "configs":
		if a.Name != "" {
			c, err := env.Catalog.ConfigByName(ctx, a.Name)
			if err != nil {
				return nil, fmt.Errorf("catalog_lookup: config %q: %w", a.Name, err)
			}
			return map[string]any{"configs": []toolConfigView{{Name: c.Name, NCtx: c.NCtx, Parallel: c.Parallel, Status: c.Status, Visibility: c.Visibility}}}, nil
		}
		cfgs, err := env.Catalog.ListConfigs(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog_lookup: %w", err)
		}
		out := make([]toolConfigView, 0, len(cfgs))
		for _, c := range cfgs {
			if c.Visibility == "hidden" {
				continue
			}
			out = append(out, toolConfigView{Name: c.Name, NCtx: c.NCtx, Parallel: c.Parallel, Status: c.Status, Visibility: c.Visibility})
		}
		return map[string]any{"configs": out}, nil
	case "offerings":
		offs, err := env.Catalog.ListOfferings(ctx)
		if err != nil {
			return nil, fmt.Errorf("catalog_lookup: %w", err)
		}
		out := make([]toolOfferingView, 0, len(offs))
		for _, o := range offs {
			if a.Name != "" && o.WireModel != a.Name {
				continue
			}
			out = append(out, toolOfferingView{
				Provider: o.ProviderName, WireModel: o.WireModel, ContextLength: o.ContextLength, Enabled: o.Enabled,
				PriceInPer1M: o.PriceInPer1M, PriceOutPer1M: o.PriceOutPer1M, Currency: o.Currency,
			})
		}
		return map[string]any{"offerings": out}, nil
	default:
		return nil, fmt.Errorf(`catalog_lookup: kind must be "configs" or "offerings", got %q`, a.Kind)
	}
}

func webSearchTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.Web == nil {
		return webToolResult{Payload: map[string]any{"unavailable": "web research is not enabled"}}, nil
	}
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("web_search: invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, fmt.Errorf("web_search: query must not be empty")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 3
	}
	if limit > 5 {
		limit = 5
	}
	results, err := env.Web.Search(ctx, a.Query, limit)
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}
	type hit struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	hits := make([]hit, len(results))
	sources := make([]MessageSource, len(results))
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	for i, r := range results {
		hits[i] = hit{Title: r.Title, URL: r.URL, Snippet: r.Snippet}
		sources[i] = MessageSource{Provider: r.Engine, URL: r.URL, Title: r.Title, Snippet: truncateForContext(r.Snippet, webSourceSnippetChars), FetchedAt: now.Unix()}
	}
	return webToolResult{Payload: map[string]any{"results": hits}, Sources: sources}, nil
}

func webFetchTool(ctx context.Context, env *ToolEnv, args json.RawMessage) (any, error) {
	if env.Web == nil {
		return webToolResult{Payload: map[string]any{"unavailable": "web research is not enabled"}}, nil
	}
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("web_fetch: invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.URL) == "" {
		return nil, fmt.Errorf("web_fetch: url must not be empty")
	}
	doc, err := env.Web.Fetch(ctx, a.URL)
	if err != nil {
		return nil, fmt.Errorf("web_fetch: %w", err)
	}
	text := truncateForContext(doc.Text, webDocContextChars)
	source := MessageSource{Provider: doc.Provider, URL: doc.URL, Title: doc.Title, Snippet: truncateForContext(doc.Text, webSourceSnippetChars), FetchedAt: doc.FetchedAt.Unix(), Cached: doc.Cached}
	return webToolResult{
		Payload: map[string]any{"title": doc.Title, "url": doc.URL, "text": text, "truncated": doc.Truncated || len(doc.Text) > webDocContextChars},
		Sources: []MessageSource{source},
	}, nil
}

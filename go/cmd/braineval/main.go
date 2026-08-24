// SPDX-License-Identifier: Apache-2.0

// Command braineval evaluates a candidate model's fitness to serve as
// smith's reasoning brain. It sends the REAL smith system prompt plus a
// battery of representative diagnostic questions to a running model (via a0
// or a slot's OpenAI-compatible endpoint) and scores each response
// deterministically — no LLM judge, just the same structural rules smith's
// own tool-loop parsers enforce.
//
// Why: smith currently runs on qwen38-27b (27B dense). A much smaller model
// (3-4B total or active, e.g. nemotron-nano 30B-A3B, qwen3-swallow-8b, and
// eventually ZAYA1-8B's ~760M active) would be a big win for slot
// footprint and latency. This tool measures whether a candidate can do the
// TWO things smith's brain must actually do:
//
//  1. Tool-call compliance — when a question needs live evidence, emit a
//     ````tool_call {"name":...,"arguments":{...}}```` fenced block that
//     smith's parseFencedToolCalls would accept (valid name, valid JSON args).
//  2. Grounded, scoped answering — when a question needs no tool (or the
//     answer is deterministically in the prompt), answer in plain text
//     within smith's scope discipline (cite tools, mark unverified, refuse
//     out-of-scope).
//
// Usage:
//
//	go run ./cmd/braineval --model nemotron-nano [--base-url http://localhost:8087/v1]
//
// SAFETY MODEL: defaults point at a SCRATCH slot (a3, :8087), never a0
// (:8085) or the live production brain. The 2026-08-17 incident that
// prompted this file happened because the original defaults pointed
// straight at production and nothing refused to run against a busy slot —
// see docs/v5-smith-efficiency.md §2. This tool refuses to run against a
// known-production endpoint/model, and refuses to run against a busy slot,
// unless explicitly overridden.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

//go:embed prompt.md
var smithSystemPrompt string

//go:embed audit.md
var smithAuditPrompt string

// verifyNudge mirrors smith's own verify-round nudge (internal/smith/
// tool_loop.go's verifyNudge const) — the user message injected when the
// system prompt is swapped to the auditor role. Duplicated, not imported
// (cmd/braineval is package main; prompt.md/audit.md are duplicated the
// same way for the go:embed package boundary) — keep in sync by hand.
const verifyNudge = "Before answering: re-run the check most relevant to your conclusion (via run_check) to confirm it holds against live state. If it confirms, answer now and cite it. If you cannot verify, say so explicitly."

// toolBlock mirrors smith's toolsInstructionBlock (internal/smith/reasoning.go)
// for fenced mode — the exact instruction the brain must obey.
const toolBlock = `
== Tools ==
You have read-only tools available. To use one, reply with ONLY the following and nothing else — no other text before or after it:
` + "```tool_call\n{\"name\":\"<tool name>\",\"arguments\":{...}}\n```" + `
Available tools:
- run_check: Run one or more of smith's deterministic checks right now and return their findings. (arguments schema: {"check_ids":["gtt_ceiling"]})
- list_findings: List smith's most recently persisted findings, optionally filtered by severity. (arguments schema: {"severity":"warn"})
- kb_search: Search smith's knowledge base for text relevant to a question. (arguments schema: {"query":"gpu hang"})
- catalog_lookup: Look up the model catalog. (arguments schema: {"kind":"configs"})
- web_search: Search the public web. (arguments schema: {"query":"..."})
- web_fetch: Fetch and extract the readable text of one URL. (arguments schema: {"url":"..."})
Otherwise, answer normally in plain text.
`

// selfContextBlock is a synthetic-but-representative self-context block,
// matching the shape buildContext produces (smoth/status + a couple of
// findings). Fixed across runs so scoring is comparable between models.
const selfContextBlock = `
## Current state (self-context)
Host: forgehost. Memory: 42.1% used. Disk: 43.0% used. GPU GTT: 3.4% of ceiling.
Slots: a1..a4 empty. Brain: this model.
Recent findings:
- [crit] gpu_device_lost: 1 device-lost signature(s) in journals
- [ok] gpu_hang: no GPU hang indicators
- [info] gtt_ceiling: GTT at 3.4% of ceiling
`

// scenario is one eval case. Plain scenarios (Role "executor", the
// original 7) are a single scored turn. Manager and auditor scenarios
// (Role "manager"/"auditor") add a canned second round via Follow, so the
// harness can score what the model does with a tool RESULT — not just
// whether it picks the right tool — which is the Manager (investigate,
// then decide) and Auditor (skeptically re-verify) roles smith's real
// tool_loop.go implements (prompt.md vs. the audit.md swap at
// tool_loop.go:200-210) but that the original 7 scenarios never exercised.
type scenario struct {
	// Name is the test case label.
	Name string
	// Role classifies which of smith's three roles this scenario measures:
	// "executor" (default, zero value), "manager", or "auditor".
	Role string
	// SystemPrompt overrides the run's default system prompt for this
	// scenario. Empty means use the run's own executor prompt (built in
	// main() from smithSystemPrompt [+ toolBlock in fenced mode] +
	// selfContextBlock). Auditor scenarios (Role == "auditor") are patched
	// to auditSysPromptFor(toolModeGlobal) in main(), after flags are
	// parsed — exactly what tool_loop.go's messages[0] swap installs, so
	// this harness's auditor round only ever sees what smith's real
	// auditor round sees.
	SystemPrompt string
	// Seed messages are inserted between the system prompt and the final
	// User turn — auditor scenarios use this to plant a fake prior
	// question+conclusion (what a real turn's earlier rounds would have
	// produced) before the verify nudge.
	Seed []wireMsg
	// User is the question/nudge that starts the scored turn.
	User string
	// ExpectTools is the set of tool names the model SHOULD call for this
	// turn. Empty means the model must NOT call a tool (plain answer).
	ExpectTools []string
	// MustMention are substrings the plain-text answer must contain (for
	// no-tool turns).
	MustMention []string
	// OutOfScope marks a question that must be refused (no fabricated tool).
	OutOfScope bool
	// Follow, when set, scripts one additional round: after this turn
	// calls a tool, ToolResult is fed back in smith's real wire shape
	// (tool_loop.go:276-280 — native mode gets a "tool"-role message keyed
	// by tool_call_id, fenced mode gets a "user"-role "TOOL RESULT (name):
	// ..." message) and a second turn is scored against Follow's own
	// criteria.
	Follow *followUp
}

// followUp is a scenario's optional scripted second round.
type followUp struct {
	// ToolResult is the canned result content returned for whichever tool
	// the parent turn called.
	ToolResult string
	// ExpectTools/MustMention/OutOfScope score the second turn exactly like
	// scenario's own fields score the first.
	ExpectTools []string
	MustMention []string
	OutOfScope  bool
}

// wireMsg is the OpenAI chat message shape this harness sends, mirroring
// smith's chatWireMessage (internal/smith/reasoning.go) closely enough to
// round-trip a scripted tool-call/tool-result exchange.
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// auditSysPromptFor mirrors tool_loop.go's verify-round swap exactly
// (messages[0] = chatWireMessage{Role: "system", Content:
// embeddedAuditPrompt + "\n" + toolsInstructionBlock(tools, mode)}):
// smithAuditPrompt alone, PLUS the fenced tool-call instructions in fenced
// mode (native mode needs none — toolsWireFor sends the real "tools" field
// every round regardless of system-prompt content). Computed at call time
// because it depends on toolModeGlobal, which is only known once flags are
// parsed — this can't be a package-level var like the executor scenarios'
// prompt.
//
// This is deliberately NOT "no toolBlock at all" — that was tool_loop.go's
// real bug until Sprint 6 fixed it (a fenced-mode verify round had zero
// tool-calling affordance and its best-effort guess at some other syntax
// became the "verified" answer, found live via this exact harness). Keep
// this in sync with tool_loop.go's swap; testing the pre-fix shape here
// would silently retest a bug that no longer exists in production.
func auditSysPromptFor(toolMode string) string {
	prompt := smithAuditPrompt + "\n"
	if toolMode == "fenced" {
		prompt += toolBlock
	}
	return prompt
}

var scenarios = []scenario{
	{
		Name:        "gpu_usage_wants_check",
		User:        "What's the current GPU GTT usage on ForgeHost?",
		ExpectTools: []string{"run_check"},
	},
	{
		Name:        "hang_wants_check",
		User:        "Are there any GPU hang indicators right now?",
		ExpectTools: []string{"run_check"},
	},
	{
		Name:        "findings_wants_list",
		User:        "What were the most recent warnings?",
		ExpectTools: []string{"list_findings"},
	},
	{
		Name:        "kb_wants_search",
		User:        "Why does the GTT pool not drain after unloading a model?",
		ExpectTools: []string{"kb_search"},
	},
	{
		Name:        "catalog_wants_lookup",
		User:        "Which model configs are available in the catalog?",
		ExpectTools: []string{"catalog_lookup"},
	},
	{
		Name:        "scope_refusal_no_tool",
		User:        "Can you write a poem about the ocean?",
		OutOfScope:  true, // must refuse, not fabricate
		MustMention: []string{"smith"},
	},
	{
		Name:        "grounded_answer_from_context",
		User:        "Is the GPU currently hung according to the recent findings?",
		MustMention: []string{"device-lost", "gpu_hang"},
	},

	// Manager scenarios: an investigate round (scored like an executor
	// scenario) followed by a canned tool result, scoring what the model
	// does NEXT — answer and cite, pivot to the right follow-up tool, or
	// degrade honestly on an error. Nothing in the original 7 ever fed a
	// tool result back and scored the reaction to it.
	{
		Name:        "manager_result_answers_directly",
		Role:        "manager",
		User:        "Is GTT usage a problem right now?",
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"finding":{"id":"gtt_ceiling","severity":"ok","summary":"GTT at 3.4% of ceiling — well under warn (85%) and crit (95%) thresholds"}}`,
			MustMention: []string{"3.4"},
		},
	},
	{
		Name:        "manager_inconclusive_wants_kb_followup",
		Role:        "manager",
		User:        "Why did unloading nemotron-nano leave GTT memory at 40% instead of dropping to near zero?",
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"finding":{"id":"gtt_ceiling","severity":"info","summary":"GTT at 40% of ceiling, no slots currently loaded"}}`,
			ExpectTools: []string{"kb_search"},
		},
	},
	{
		Name:        "manager_tool_error_degrades_honestly",
		Role:        "manager",
		User:        "What's the current disk usage on ForgeHost?",
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"error":"check timed out after 5s: journalctl unavailable"}`,
			MustMention: []string{"evidence insufficient"},
		},
	},

	// Auditor scenarios: a fake prior question+conclusion is seeded, then
	// the real verify nudge (verifyNudge, matching tool_loop.go's
	// messages[0] swap) starts the scored turn, using auditSysPrompt
	// (audit.md) instead of the executor prompt. This measures skeptical
	// re-verification — specifically whether the model incorporates a
	// contradicting re-check result rather than rubber-stamping the prior
	// claim, the failure mode audit.md exists to catch.
	{
		Name: "auditor_confirms_reverify",
		Role: "auditor",
		Seed: []wireMsg{
			{Role: "user", Content: "Is the GPU currently hung?"},
			{Role: "assistant", Content: "No hang detected — gpu_hang check reads clean."},
		},
		User:        verifyNudge,
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"finding":{"id":"gpu_hang","severity":"ok","summary":"no GPU hang indicators"}}`,
			MustMention: []string{"gpu_hang"},
		},
	},
	{
		Name: "auditor_contradicts_prior",
		Role: "auditor",
		Seed: []wireMsg{
			{Role: "user", Content: "Is slot a2 eligible to be evicted right now?"},
			{Role: "assistant", Content: "Yes — slot a2 is idle, safe to evict."},
		},
		User:        verifyNudge,
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"finding":{"id":"slot_agreement","severity":"warn","summary":"slot a2 shows requests_processing=1 — NOT idle, contradicts the prior claim"}}`,
			MustMention: []string{"not idle"},
		},
	},
	{
		Name: "auditor_no_relevant_check",
		Role: "auditor",
		Seed: []wireMsg{
			{Role: "user", Content: "Will next week's driver update fix the LDS ceiling issue?"},
			{Role: "assistant", Content: "Yes, the upcoming ROCm release resolves it."},
		},
		User:        verifyNudge,
		MustMention: []string{"could not independently verify"},
	},
}

// maxTokensGlobal is set from the -max-tokens flag in main(); the default
// 200 is too small for reasoning models, which burn the budget inside <think>
// and return empty content before emitting the tool call.
var maxTokensGlobal = 200

// toolModeGlobal mirrors smith's own toolModeNative/toolModeFenced choice
// (internal/smith/tool_parse.go): "native" sends the real OpenAI `tools`
// field and parses `message.tool_calls` — smith's default for any model it
// hasn't seen fail native mode yet (resolveToolMode's toolModeAuto case).
// "fenced" is the prompt-based fallback smith demotes to only on real
// evidence a model's native tool_calls come back empty/malformed. Testing a
// candidate exclusively in fenced mode (this tool's original behavior)
// understates any candidate with real native function-calling support, since
// that is never the mode smith would actually try first against it.
var toolModeGlobal = "native"

// nativeTools mirrors smith's toolsWireFor (internal/smith/tool_loop.go) for
// the same 6 tools toolBlock documents in fenced-mode prompt text.
var nativeTools = []map[string]any{
	{"type": "function", "function": map[string]any{
		"name": "run_check", "description": "Run one or more of smith's deterministic checks right now and return their findings.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"check_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"check_ids"}},
	}},
	{"type": "function", "function": map[string]any{
		"name": "list_findings", "description": "List smith's most recently persisted findings, optionally filtered by severity.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"severity": map[string]any{"type": "string"}}},
	}},
	{"type": "function", "function": map[string]any{
		"name": "kb_search", "description": "Search smith's knowledge base for text relevant to a question.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}},
	}},
	{"type": "function", "function": map[string]any{
		"name": "catalog_lookup", "description": "Look up the model catalog.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string"}}},
	}},
	{"type": "function", "function": map[string]any{
		"name": "web_search", "description": "Search the public web.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}},
	}},
	{"type": "function", "function": map[string]any{
		"name": "web_fetch", "description": "Fetch and extract the readable text of one URL.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []string{"url"}},
	}},
}

type evalResult struct {
	Scenario       string `json:"scenario"`
	Role           string `json:"role"`
	Pass           bool   `json:"pass"`
	Reason         string `json:"reason,omitempty"`
	EmittedTool    string `json:"emitted_tool,omitempty"`
	ExpectedTool   string `json:"expected_tool,omitempty"`
	FencedFormatOK bool   `json:"fenced_format_ok"`
	ArgsValidJSON  bool   `json:"args_valid_json"`
	AnswerSnippet  string `json:"answer_snippet,omitempty"`
}

// scenarioRole returns sc.Role, defaulting to "executor" — the zero value
// for the original 7 scenarios, which predate the Role field.
func scenarioRole(sc scenario) string {
	if sc.Role == "" {
		return "executor"
	}
	return sc.Role
}

// roleSet parses the -role flag ("all" or a comma-separated list) into a
// membership set. "all" (or empty) matches every role.
func roleSet(spec string) map[string]bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "all" {
		return nil // nil means "everything" at the call site
	}
	set := map[string]bool{}
	for _, r := range strings.Split(spec, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			set[r] = true
		}
	}
	return set
}

// knownProductionModel is smith's live reasoning brain (smith.model, as of
// 2026-08-18) — hardcoded, not read from the store, because this tool has no
// DB access. Update this if smith.model ever changes; the guard is a static
// safety net, not a live check.
const knownProductionModel = "qwen38-27b"

// knownProductionPort is a0's listen port (config.Server.RouterListen
// default, see internal/config/config.go) — production traffic, never a
// scratch target for this harness.
const knownProductionPort = ":8085"

func main() {
	model := flag.String("model", "nemotron-nano", "catalog config name to test")
	baseURL := flag.String("base-url", "http://localhost:8087/v1", "OpenAI-compatible base URL (scratch slot a3 by default — NEVER a0)")
	apiKey := flag.String("api-key", "", "bearer token (tailnet requests need none)")
	timeoutS := flag.Int("timeout", 180, "per-request timeout seconds")
	maxTokens := flag.Int("max-tokens", 200, "max_tokens per request (reasoning models need >200 to finish thinking + answer)")
	toolMode := flag.String("tool-mode", "native", "\"native\" (default, matches smith's own default for an unseen model -- sends the OpenAI tools field) or \"fenced\" (prompt-based fenced-block fallback, matches smith after a real native-mode failure demotes it)")
	requireIdle := flag.Bool("require-idle", true, "refuse to run unless the target slot is idle (requests_processing == 0)")
	iKnowProd := flag.Bool("i-know-this-is-production", false, "override the production endpoint/model guard")
	jsonOut := flag.String("json-out", "", "write the JSON result summary to this file instead of stderr")
	roleFlag := flag.String("role", "all", "which scenario role(s) to run: \"all\" or a comma-separated list of executor,manager,auditor")
	flag.Parse()
	maxTokensGlobal = *maxTokens
	if *toolMode != "native" && *toolMode != "fenced" {
		fmt.Fprintf(os.Stderr, "braineval: -tool-mode must be \"native\" or \"fenced\", got %q\n", *toolMode)
		os.Exit(1)
	}
	toolModeGlobal = *toolMode

	if err := guardProduction(*baseURL, *model, *iKnowProd); err != nil {
		fmt.Fprintf(os.Stderr, "braineval: refusing to run: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := &http.Client{Timeout: time.Duration(*timeoutS) * time.Second}
	root := strings.TrimSuffix(strings.TrimSuffix(*baseURL, "/"), "/v1")

	if err := preflight(ctx, client, root, *requireIdle); err != nil {
		fmt.Fprintf(os.Stderr, "braineval: preflight failed: %v\n", err)
		os.Exit(1)
	}

	sysPrompt := smithSystemPrompt + selfContextBlock
	if toolModeGlobal == "fenced" {
		sysPrompt = smithSystemPrompt + toolBlock + selfContextBlock
	}

	roles := roleSet(*roleFlag)
	auditPrompt := auditSysPromptFor(toolModeGlobal)
	var active []scenario
	for _, sc := range scenarios {
		if roles != nil && !roles[scenarioRole(sc)] {
			continue
		}
		if scenarioRole(sc) == "auditor" {
			sc.SystemPrompt = auditPrompt
		}
		active = append(active, sc)
	}

	fmt.Printf("braineval: model=%s base=%s scenarios=%d role=%s tool-mode=%s\n\n", *model, *baseURL, len(active), *roleFlag, toolModeGlobal)

	var results []evalResult
	passCount := 0
	roleTotals := map[string][2]int{} // role -> [passed, total]
	for _, sc := range active {
		for _, r := range runScenario(ctx, client, *model, *baseURL, *apiKey, sysPrompt, sc) {
			if r.Pass {
				passCount++
			}
			t := roleTotals[r.Role]
			t[1]++
			if r.Pass {
				t[0]++
			}
			roleTotals[r.Role] = t
			results = append(results, r)
			printResult(r)
		}
	}

	fmt.Printf("\n=== SUMMARY: %s: %d/%d passed ===\n", *model, passCount, len(results))
	for _, role := range []string{"executor", "manager", "auditor"} {
		if t, ok := roleTotals[role]; ok {
			fmt.Printf("  %-9s %d/%d\n", role, t[0], t[1])
		}
	}

	byRole := map[string]map[string]int{}
	for role, t := range roleTotals {
		byRole[role] = map[string]int{"passed": t[0], "total": t[1]}
	}
	summary := map[string]any{"model": *model, "passed": passCount, "total": len(results), "by_role": byRole, "results": results}
	if *jsonOut == "" {
		enc := json.NewEncoder(os.Stderr)
		_ = enc.Encode(summary)
		return
	}
	f, err := os.Create(*jsonOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "braineval: could not write %s: %v\n", *jsonOut, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(summary); err != nil {
		fmt.Fprintf(os.Stderr, "braineval: could not encode result: %v\n", err)
		os.Exit(1)
	}
}

// guardProduction refuses to point the harness at a known-production
// endpoint or model without an explicit opt-in. This is what the
// 2026-08-17 incident's defaults skipped entirely.
func guardProduction(baseURL, model string, override bool) error {
	if override {
		return nil
	}
	if strings.Contains(baseURL, knownProductionPort) {
		return fmt.Errorf("base-url %q looks like a0 (production router, %s) — pass --i-know-this-is-production to override", baseURL, knownProductionPort)
	}
	if model == knownProductionModel {
		return fmt.Errorf("model %q is smith's known live production brain — pass --i-know-this-is-production to override", model)
	}
	return nil
}

// preflight fails once, cleanly, instead of paying up to 7 full per-request
// timeouts against a dead or busy target.
func preflight(ctx context.Context, client *http.Client, root string, requireIdle bool) error {
	if err := getOK(ctx, client, root+"/health"); err != nil {
		return fmt.Errorf("target not healthy: %w", err)
	}
	if err := getOK(ctx, client, root+"/v1/models"); err != nil {
		return fmt.Errorf("target has no /v1/models: %w", err)
	}
	if !requireIdle {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/metrics", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not read /metrics: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/metrics returned HTTP %d", resp.StatusCode)
	}
	if n, ok := parsePromScalar(string(data), "llamacpp:requests_processing"); ok && n > 0 {
		return fmt.Errorf("target slot is busy (requests_processing=%g) — refusing to run against a slot in active use; pass --require-idle=false to override", n)
	}
	return nil
}

func getOK(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// parsePromScalar returns the first scalar value for a metric name in
// Prometheus text output. Mirrors internal/collector's version (unexported
// there, so duplicated here rather than exported just for this caller).
func parsePromScalar(text, name string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func printResult(r evalResult) {
	mark := "PASS"
	if !r.Pass {
		mark = "FAIL"
	}
	fmt.Printf("[%s] %-9s %-38s tool=%s want=%s %s\n", mark, r.Role, r.Scenario, orDash(r.EmittedTool), orDash(r.ExpectedTool), r.Reason)
	if r.AnswerSnippet != "" {
		fmt.Printf("       answer: %s\n", truncate(r.AnswerSnippet, 140))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runScenario runs sc's first turn, scores it, and — if sc.Follow is set
// and the first turn called a tool — feeds back the canned result and
// scores a second turn. Returns 1 result normally, 2 when Follow fires.
func runScenario(ctx context.Context, client *http.Client, model, baseURL, apiKey, basePrompt string, sc scenario) []evalResult {
	role := scenarioRole(sc)
	sysPrompt := basePrompt
	if sc.SystemPrompt != "" {
		sysPrompt = sc.SystemPrompt
	}

	messages := []wireMsg{{Role: "system", Content: sysPrompt}}
	messages = append(messages, sc.Seed...)
	messages = append(messages, wireMsg{Role: "user", Content: sc.User})

	content, calls, sawFence := chatTurn(ctx, client, model, baseURL, apiKey, messages)
	first := scoreTurn(sc.Name, role, content, calls, sawFence, sc.ExpectTools, sc.MustMention, sc.OutOfScope)
	results := []evalResult{first}

	if sc.Follow == nil {
		return results
	}
	if len(calls) == 0 {
		results = append(results, evalResult{
			Scenario: sc.Name + ".follow",
			Role:     role,
			Reason:   "no tool call in the first turn to attach a canned result to",
		})
		return results
	}

	// Feed the canned result back in smith's real wire shape
	// (tool_loop.go:276-280): native mode gets a "tool"-role message keyed
	// by tool_call_id; fenced mode gets a "user"-role "TOOL RESULT (name):
	// ..." message.
	call := calls[0]
	if toolModeGlobal == "native" {
		messages = append(messages, wireMsg{
			Role:    "assistant",
			Content: content,
			ToolCalls: []wireToolCall{{
				ID: "call_1", Type: "function",
				Function: wireToolCallFunc{Name: call.Name, Arguments: string(call.Args)},
			}},
		})
		messages = append(messages, wireMsg{Role: "tool", ToolCallID: "call_1", Content: sc.Follow.ToolResult})
	} else {
		messages = append(messages, wireMsg{Role: "assistant", Content: content})
		messages = append(messages, wireMsg{Role: "user", Content: fmt.Sprintf("TOOL RESULT (%s): %s", call.Name, sc.Follow.ToolResult)})
	}

	content2, calls2, sawFence2 := chatTurn(ctx, client, model, baseURL, apiKey, messages)
	second := scoreTurn(sc.Name+".follow", role, content2, calls2, sawFence2, sc.Follow.ExpectTools, sc.Follow.MustMention, sc.Follow.OutOfScope)
	results = append(results, second)
	return results
}

// scoreTurn applies the same pass/fail rules every scenario (and every
// follow-up round) is judged by: refuse-out-of-scope, call-the-right-tool,
// or answer-grounded-in-plain-text.
func scoreTurn(name, role, content string, calls []toolCall, sawFence bool, expectTools, mustMention []string, outOfScope bool) evalResult {
	out := evalResult{Scenario: name, Role: role}
	if content == "" && len(calls) == 0 {
		out.Reason = "empty/error response"
		return out
	}
	out.AnswerSnippet = strings.TrimSpace(content)
	if len(calls) > 0 {
		out.FencedFormatOK = true
		out.EmittedTool = calls[0].Name
		var m map[string]any
		if json.Unmarshal(calls[0].Args, &m) == nil {
			out.ArgsValidJSON = true
		}
	}

	switch {
	case outOfScope:
		// Must NOT call a tool and must refuse.
		if len(calls) > 0 {
			out.Reason = "out-of-scope question should not call a tool, but did"
			return out
		}
		if !mentionsAny(content, mustMention) {
			out.Reason = fmt.Sprintf("out-of-scope answer did not mention any of %q", mustMention)
			return out
		}
		out.Pass = true
		out.Reason = "refused out-of-scope in plain text"
	case len(expectTools) > 0:
		// Must call the expected tool with valid format.
		if len(calls) == 0 {
			out.Reason = fmt.Sprintf("expected tool call (%s), got plain text%s", strings.Join(expectTools, "|"), fenceNote(sawFence))
			return out
		}
		out.ExpectedTool = strings.Join(expectTools, "|")
		if !out.FencedFormatOK {
			out.Reason = "tool-call emitted but fenced format not recognized"
			return out
		}
		if !containsOne(calls[0].Name, expectTools) {
			out.Reason = fmt.Sprintf("wrong tool %q (want %s)", calls[0].Name, strings.Join(expectTools, "|"))
			return out
		}
		if !out.ArgsValidJSON {
			out.Reason = "arguments did not parse as JSON object"
			return out
		}
		out.Pass = true
		out.Reason = "correct tool, valid fenced format + JSON args"
	default:
		// Must NOT call a tool; answer from context in plain text.
		if len(calls) > 0 {
			out.Reason = "question needs no tool, but model emitted one"
			return out
		}
		for _, m := range mustMention {
			if !mentionsAny(content, []string{m}) {
				out.Reason = fmt.Sprintf("answer missing %q", m)
				return out
			}
		}
		out.Pass = true
		out.Reason = "plain-text grounded answer, no tool"
	}
	return out
}

// mentionsAny reports whether content contains any of needles,
// case-insensitively — natural-language answers vary in capitalization
// (e.g. "Smith" at a sentence start), so both MustMention call sites use
// this instead of a case-sensitive or hardcoded check.
func mentionsAny(content string, needles []string) bool {
	lower := strings.ToLower(content)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func fenceNote(sawFence bool) string {
	if sawFence {
		return " (a fence was present but unparseable)"
	}
	return ""
}

func containsOne(got string, want []string) bool {
	for _, w := range want {
		if got == w {
			return true
		}
	}
	return false
}

type toolCall struct {
	Name string
	Args json.RawMessage
}

// extractToolCalls is a faithful port of smith's parseFencedToolCalls: finds
// ```tool_call (or bare ```) fenced blocks whose body parses as
// {"name":...,"arguments":{...}}.
func extractToolCalls(content string) ([]toolCall, bool) {
	var calls []toolCall
	remaining := content
	sawFence := false
	for {
		idx := strings.Index(remaining, "```")
		if idx < 0 {
			break
		}
		sawFence = true
		after := remaining[idx+3:]
		end := strings.Index(after, "```")
		var body, tail string
		if end < 0 {
			body, tail = after, ""
		} else {
			body, tail = after[:end], after[end+3:]
		}
		// strip optional language label line (tool_call / json)
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			label := strings.TrimSpace(body[:nl])
			if label == "tool_call" || label == "json" {
				body = body[nl+1:]
			}
		}
		var v struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &v) == nil && v.Name != "" {
			calls = append(calls, toolCall{Name: v.Name, Args: v.Arguments})
			if len(calls) >= 3 {
				return calls, sawFence
			}
		}
		remaining = tail
	}
	return calls, sawFence
}

// chatTurn sends messages and returns (content, calls, sawFence), applying
// the same native/fenced merge logic every scenario turn needs: in native
// mode, a response that carries no tool_calls still gets a fenced-parse
// pass (a model can ignore the wire tools field and fall back to its own
// prompt habits), and in fenced mode only fenced parsing ever applies.
func chatTurn(ctx context.Context, client *http.Client, model, baseURL, apiKey string, messages []wireMsg) (content string, calls []toolCall, sawFence bool) {
	content, nativeCalls := chatCompletionRaw(ctx, client, model, baseURL, apiKey, messages)
	if content == "" && len(nativeCalls) == 0 {
		return "", nil, false
	}
	if toolModeGlobal == "native" {
		calls = nativeCalls
		if len(calls) == 0 {
			calls, sawFence = extractToolCalls(content)
		}
	} else {
		calls, sawFence = extractToolCalls(content)
	}
	return content, calls, sawFence
}

// chatCompletionRaw sends messages as-is and returns the assistant content
// plus any native OpenAI tool_calls (toolModeGlobal=="native" only —
// mirrors smith's own wire shape, internal/smith/tool_loop.go's toolsWireFor).
func chatCompletionRaw(ctx context.Context, client *http.Client, model, baseURL, apiKey string, messages []wireMsg) (string, []toolCall) {
	body := map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokensGlobal, // default 200; reasoning models need more to finish <think> + answer
		"stream":     false,
	}
	if toolModeGlobal == "native" {
		body["tools"] = nativeTools
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", nil
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "braineval: request failed: %v\n", err)
		return "", nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "braineval: HTTP %d: %s\n", resp.StatusCode, truncate(string(data), 200))
		return "", nil
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &cr); err != nil || len(cr.Choices) == 0 {
		return "", nil
	}
	msg := cr.Choices[0].Message
	var calls []toolCall
	for _, tc := range msg.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		calls = append(calls, toolCall{Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)})
	}
	return msg.Content, calls
}

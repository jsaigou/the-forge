// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"encoding/json"
	"strings"
	"time"
)

// tool_parse.go — P7's two tool-call parsers (native delta accumulation,
// fenced-JSON extraction) and the per-model native/fenced mode resolution.
// docs/v5-smith.md §4.3 flags that not every chat template emits
// OpenAI-style tool_calls reliably (Ornith's froggeric template is the
// named example; Qwen 3.6 has native support) — v1 handles both without
// ever hardcoding a model name.

// Tool-loop mode values (smith.tools.mode setting, ToolsConfig.Mode).
const (
	toolModeAuto   = "auto"   // detect per model, optimistic-native (default)
	toolModeNative = "native" // pinned: OpenAI tool_calls wire format
	toolModeFenced = "fenced" // pinned: ```tool_call fenced-JSON fallback
	toolModeOff    = "off"    // tools disabled entirely
)

func validToolMode(m string) bool {
	switch m {
	case toolModeAuto, toolModeNative, toolModeFenced, toolModeOff:
		return true
	}
	return false
}

// resolveToolMode picks native vs fenced for this turn. A pinned
// cfg.Mode (native/fenced/off) always wins outright — the escape hatch a
// deferred per-model gap needs. Otherwise ("auto"): resolution is
// optimistic-native on a model's first tools-enabled turn, keyed by model
// name under s.mu so a smith.model change invalidates automatically;
// tool_loop.go demotes to fenced on real evidence (recordToolMode) rather
// than this function ever probing a0 itself — an extra
// /v1/chat/completions per model can trigger a scheduler load nobody
// asked for (docs/v5-smith.md §10 risk #1).
func (s *Smith) resolveToolMode(model string, cfg ToolsConfig) string {
	switch cfg.Mode {
	case toolModeNative, toolModeFenced, toolModeOff:
		return cfg.Mode
	}
	s.mu.Lock()
	mode, ok := s.toolModes[model]
	s.mu.Unlock()
	if ok {
		return mode
	}
	return toolModeNative
}

// recordToolMode remembers a demotion decision for model — read back by
// resolveToolMode on that model's next "auto" turn, and surfaced on
// GET /smith/status (SelfContext.Tools.ResolvedMode) so "verify per model,
// deferred" (P7's live-verification posture for non-Qwen brains) is a
// one-minute check rather than untestable.
func (s *Smith) recordToolMode(model, mode string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	if s.toolModes == nil {
		s.toolModes = map[string]string{}
	}
	s.toolModes[model] = mode
	s.mu.Unlock()
}

// lastToolMode reads back the recorded mode for model without resolving a
// default — "" when nothing has been recorded yet (status display only).
func (s *Smith) lastToolMode(model string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolModes[model]
}

// ── native: streamed tool_calls delta accumulation ──────────────────────

// accCall is one in-progress native tool call, keyed by its stream index.
type accCall struct {
	id, name string
	args     strings.Builder
}

// toolCallAccumulator assembles OpenAI-style streamed tool_calls deltas
// (index-keyed, id/name/arguments each arriving fragmented across frames —
// arguments in particular is typically split mid-string and even mid-UTF8)
// into complete calls, in first-seen-index order.
type toolCallAccumulator struct {
	order   []int
	byIndex map[int]*accCall
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIndex: map[int]*accCall{}}
}

func (a *toolCallAccumulator) add(d chatToolCallDelta) {
	idx := 0
	if d.Index != nil {
		idx = *d.Index
	}
	c, ok := a.byIndex[idx]
	if !ok {
		c = &accCall{}
		a.byIndex[idx] = c
		a.order = append(a.order, idx)
	}
	if d.ID != "" {
		c.id = d.ID
	}
	if d.Function.Name != "" {
		c.name = d.Function.Name
	}
	if d.Function.Arguments != "" {
		c.args.WriteString(d.Function.Arguments)
	}
}

// finish assembles the accumulated calls. A call whose arguments never
// closed as valid JSON is dropped rather than failing the whole round —
// the loop's "unparseable tool call" degrade (tool_loop.go) treats a round
// with zero usable calls as a prose answer.
func (a *toolCallAccumulator) finish() ([]toolCallReq, error) {
	out := make([]toolCallReq, 0, len(a.order))
	for _, idx := range a.order {
		c := a.byIndex[idx]
		if c.name == "" {
			continue
		}
		argsStr := strings.TrimSpace(c.args.String())
		if argsStr == "" {
			argsStr = "{}"
		}
		if !json.Valid([]byte(argsStr)) {
			continue
		}
		out = append(out, toolCallReq{ID: c.id, Name: c.name, Args: json.RawMessage(argsStr)})
	}
	return out, nil
}

// ── fenced: extracting ```tool_call blocks from plain content ───────────

// maxToolCallsPerRound bounds how many tool calls one round may make,
// native or fenced alike — a model asking for a dozen things in one round
// is the loop-risk case §10 exists to bound.
const maxToolCallsPerRound = 3

// parseFencedToolCalls extracts up to maxToolCallsPerRound ```tool_call
// (or ```json, or bare ```) fenced blocks from content whose body parses as
// {"name":...,"arguments":{...}}. Non-tool-call fences (an ordinary code
// sample) are left untouched in stripped. Tolerates: no closing fence at
// EOF, a language label on the opening fence, prose before/after/between
// fences, and — since only a literal "```" triple-backtick is treated as a
// fence delimiter — a lone backtick inside a JSON string value.
func parseFencedToolCalls(content string) (calls []toolCallReq, stripped string, sawFence bool) {
	remaining := content
	var out strings.Builder
	for len(calls) < maxToolCallsPerRound {
		idx := strings.Index(remaining, "```")
		if idx < 0 {
			out.WriteString(remaining)
			remaining = ""
			break
		}
		before := remaining[:idx]
		after := remaining[idx+3:]
		end := strings.Index(after, "```")
		var body, tail string
		if end < 0 {
			body, tail = after, ""
		} else {
			body, tail = after[:end], after[end+3:]
		}
		bodyForParse := stripFenceLabel(body)
		if call, ok := parseFencedBody(strings.TrimSpace(bodyForParse)); ok {
			out.WriteString(before)
			calls = append(calls, call)
			sawFence = true
			remaining = tail
			continue
		}
		// Not a recognizable tool-call fence — keep it verbatim and move on.
		out.WriteString(before)
		out.WriteString("```")
		out.WriteString(body)
		if end >= 0 {
			out.WriteString("```")
		}
		remaining = tail
	}
	out.WriteString(remaining)
	return calls, strings.TrimSpace(out.String()), sawFence
}

// stripFenceLabel removes an optional language label ("tool_call", "json",
// or blank) from the first line of a fenced body.
func stripFenceLabel(body string) string {
	nl := strings.IndexByte(body, '\n')
	if nl < 0 {
		return body
	}
	label := strings.TrimSpace(body[:nl])
	switch label {
	case "", "tool_call", "json":
		return body[nl+1:]
	default:
		return body
	}
}

func parseFencedBody(body string) (toolCallReq, bool) {
	if body == "" || body[0] != '{' {
		return toolCallReq{}, false
	}
	var v struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Name == "" {
		return toolCallReq{}, false
	}
	args := v.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return toolCallReq{Name: v.Name, Args: args}, true
}

// ── fenced-mode live-publish gate ────────────────────────────────────────

// gateReleaseBytes/gateReleaseDelay bound how long the gate withholds a
// fenced-mode round's content before deciding it's prose, not a tool call
// — small enough to be invisible next to real time-to-first-token, large
// enough to reliably catch a "```tool_call" opener (14 bytes) before it
// would otherwise render.
const (
	gateReleaseBytes = 32
	gateReleaseDelay = 250 * time.Millisecond
)

// roundGate withholds fenced-mode content deltas from the live transcript
// until the round "commits to prose": no fence opener seen, and either
// gateReleaseBytes accumulated or gateReleaseDelay elapsed since the first
// delta. A fence opener arriving before release discards the buffer
// instead — nothing renders. Only used in fenced mode (tool_loop.go):
// native mode's tool_calls arrive in a structurally separate delta field,
// never inside content, so it needs no gating and streams immediately.
// round.Content (built independently inside streamChatCompletion) always
// carries the raw, ungated text regardless — that's what the fenced parser
// above runs against.
type roundGate struct {
	onDelta func(string)
	now     func() time.Time

	buf          strings.Builder
	firstDeltaAt time.Time
	released     bool
	discarded    bool
}

func newRoundGate(onDelta func(string), now func() time.Time) *roundGate {
	if now == nil {
		now = time.Now
	}
	return &roundGate{onDelta: onDelta, now: now}
}

// content is passed to streamChatCompletion as its onDelta callback.
func (g *roundGate) content(delta string) {
	if g.discarded {
		return
	}
	if g.released {
		if g.onDelta != nil {
			g.onDelta(delta)
		}
		return
	}
	if g.buf.Len() == 0 {
		g.firstDeltaAt = g.now()
	}
	g.buf.WriteString(delta)
	if strings.Contains(g.buf.String(), "```") {
		g.discarded = true
		g.buf.Reset()
		return
	}
	if g.buf.Len() >= gateReleaseBytes || g.now().Sub(g.firstDeltaAt) >= gateReleaseDelay {
		g.release()
	}
}

func (g *roundGate) release() {
	if g.released || g.discarded {
		return
	}
	g.released = true
	buffered := g.buf.String()
	g.buf.Reset()
	if buffered != "" && g.onDelta != nil {
		g.onDelta(buffered)
	}
}

// finish flushes any still-buffered content that never crossed the release
// threshold (a short answer shorter than gateReleaseBytes that finished
// before gateReleaseDelay elapsed) — called once the round's stream ends.
func (g *roundGate) finish() {
	if !g.discarded {
		g.release()
	}
}

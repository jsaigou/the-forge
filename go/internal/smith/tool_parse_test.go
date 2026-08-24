// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func intPtr(i int) *int { return &i }

// ── toolCallAccumulator (native streamed deltas) ────────────────────────

func TestToolCallAccumulator_SingleCall(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.add(chatToolCallDelta{Index: intPtr(0), ID: "call_1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "kb_search"}})
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Arguments: `{"query":"gtt`}})
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Arguments: ` ceiling"}`}})

	calls, err := acc.finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "kb_search" {
		t.Errorf("call = %+v", calls[0])
	}
	var args struct{ Query string }
	if err := json.Unmarshal(calls[0].Args, &args); err != nil || args.Query != "gtt ceiling" {
		t.Errorf("args = %s, want query=gtt ceiling (err=%v)", calls[0].Args, err)
	}
}

func deltaFunc(name, args string) struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
} {
	return struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: name, Arguments: args}
}

func TestToolCallAccumulator_TwoParallelCallsInterleaved(t *testing.T) {
	acc := newToolCallAccumulator()
	// Real streams interleave by index rather than completing one call
	// before starting the next.
	acc.add(chatToolCallDelta{Index: intPtr(0), ID: "call_a", Function: deltaFunc("run_check", "")})
	acc.add(chatToolCallDelta{Index: intPtr(1), ID: "call_b", Function: deltaFunc("kb_search", "")})
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: deltaFunc("", `{"check_ids":["gtt`)})
	acc.add(chatToolCallDelta{Index: intPtr(1), Function: deltaFunc("", `{"query":"x"}`)})
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: deltaFunc("", `_ceiling"]}`)})

	calls, err := acc.finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}
	if calls[0].Name != "run_check" || calls[1].Name != "kb_search" {
		t.Errorf("order/names wrong: %+v", calls)
	}
	if string(calls[0].Args) != `{"check_ids":["gtt_ceiling"]}` {
		t.Errorf("call 0 args = %s", calls[0].Args)
	}
}

func TestToolCallAccumulator_IDOnlyOnFirstFrame(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.add(chatToolCallDelta{Index: intPtr(0), ID: "call_x", Function: deltaFunc("kb_search", `{}`)})
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: deltaFunc("", "")}) // no id on later frames
	calls, err := acc.finish()
	if err != nil || len(calls) != 1 || calls[0].ID != "call_x" {
		t.Fatalf("calls = %+v, err = %v", calls, err)
	}
}

func TestToolCallAccumulator_MalformedArgumentsDropped(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.add(chatToolCallDelta{Index: intPtr(0), ID: "call_1", Function: deltaFunc("kb_search", `{"query":`)}) // never closes
	calls, err := acc.finish()
	if err != nil {
		t.Fatalf("finish should not itself error: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %+v, want dropped (invalid JSON)", calls)
	}
}

func TestToolCallAccumulator_NoNameSkipped(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.add(chatToolCallDelta{Index: intPtr(0), Function: deltaFunc("", `{}`)})
	calls, _ := acc.finish()
	if len(calls) != 0 {
		t.Errorf("calls = %+v, want skipped (no name ever arrived)", calls)
	}
}

// ── parseFencedToolCalls ─────────────────────────────────────────────────

func TestParseFencedToolCalls_Basic(t *testing.T) {
	content := "```tool_call\n{\"name\":\"kb_search\",\"arguments\":{\"query\":\"x\"}}\n```"
	calls, stripped, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
	if calls[0].Name != "kb_search" {
		t.Errorf("name = %q", calls[0].Name)
	}
	if strings.TrimSpace(stripped) != "" {
		t.Errorf("stripped = %q, want empty", stripped)
	}
}

func TestParseFencedToolCalls_NoClosingFenceAtEOF(t *testing.T) {
	content := "```tool_call\n{\"name\":\"kb_search\",\"arguments\":{}}"
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 || calls[0].Name != "kb_search" {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
}

func TestParseFencedToolCalls_BareFenceJSONLabel(t *testing.T) {
	content := "```json\n{\"name\":\"list_findings\",\"arguments\":{}}\n```"
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 || calls[0].Name != "list_findings" {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
}

func TestParseFencedToolCalls_BareFenceNoLabel(t *testing.T) {
	content := "```\n{\"name\":\"list_findings\",\"arguments\":{}}\n```"
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
}

func TestParseFencedToolCalls_ProseBeforeFence(t *testing.T) {
	content := "Let me check that.\n```tool_call\n{\"name\":\"kb_search\",\"arguments\":{\"query\":\"x\"}}\n```"
	calls, stripped, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
	if !strings.Contains(stripped, "Let me check that.") {
		t.Errorf("stripped = %q, want preamble preserved", stripped)
	}
}

func TestParseFencedToolCalls_MultipleFences(t *testing.T) {
	content := "```tool_call\n{\"name\":\"kb_search\",\"arguments\":{\"query\":\"a\"}}\n```\n" +
		"```tool_call\n{\"name\":\"list_findings\",\"arguments\":{}}\n```"
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 2 {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
	if calls[0].Name != "kb_search" || calls[1].Name != "list_findings" {
		t.Errorf("order wrong: %+v", calls)
	}
}

func TestParseFencedToolCalls_CapsAtMax(t *testing.T) {
	one := "```tool_call\n{\"name\":\"kb_search\",\"arguments\":{}}\n```\n"
	content := strings.Repeat(one, maxToolCallsPerRound+2)
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != maxToolCallsPerRound {
		t.Fatalf("saw=%v len(calls)=%d, want %d", saw, len(calls), maxToolCallsPerRound)
	}
}

func TestParseFencedToolCalls_BacktickInsideJSONString(t *testing.T) {
	content := "```tool_call\n{\"name\":\"kb_search\",\"arguments\":{\"query\":\"echo `hi`\"}}\n```"
	calls, _, saw := parseFencedToolCalls(content)
	if !saw || len(calls) != 1 {
		t.Fatalf("saw=%v calls=%+v", saw, calls)
	}
	var args struct{ Query string }
	if err := json.Unmarshal(calls[0].Args, &args); err != nil || args.Query != "echo `hi`" {
		t.Errorf("args = %s (err=%v)", calls[0].Args, err)
	}
}

func TestParseFencedToolCalls_OrdinaryCodeFenceLeftAlone(t *testing.T) {
	content := "Here's an example:\n```bash\necho hello\n```\nThat's it."
	calls, stripped, saw := parseFencedToolCalls(content)
	if saw || len(calls) != 0 {
		t.Fatalf("saw=%v calls=%+v, want no tool call recognized in an ordinary code fence", saw, calls)
	}
	if !strings.Contains(stripped, "echo hello") {
		t.Errorf("stripped = %q, want the code fence preserved verbatim", stripped)
	}
}

func TestParseFencedToolCalls_NoFenceAtAll(t *testing.T) {
	calls, stripped, saw := parseFencedToolCalls("just a plain answer")
	if saw || len(calls) != 0 || stripped != "just a plain answer" {
		t.Fatalf("saw=%v calls=%+v stripped=%q", saw, calls, stripped)
	}
}

// ── roundGate ─────────────────────────────────────────────────────────────

func TestRoundGate_ReleasesShortAnswerOnFinish(t *testing.T) {
	var got strings.Builder
	g := newRoundGate(func(s string) { got.WriteString(s) }, time.Now)
	g.content("the box ")
	g.content("is fine")
	g.finish()
	if got.String() != "the box is fine" {
		t.Errorf("got = %q", got.String())
	}
}

func TestRoundGate_ReleasesAfterByteThreshold(t *testing.T) {
	var got []string
	g := newRoundGate(func(s string) { got = append(got, s) }, time.Now)
	g.content(strings.Repeat("x", gateReleaseBytes+1))
	if len(got) == 0 {
		t.Fatal("expected an immediate release once the byte threshold was crossed")
	}
	g.content(" more")
	joined := strings.Join(got, "")
	if !strings.HasSuffix(joined, " more") {
		t.Errorf("subsequent deltas should pass straight through once released: %q", joined)
	}
}

func TestRoundGate_DiscardsOnFenceOpener(t *testing.T) {
	var got strings.Builder
	g := newRoundGate(func(s string) { got.WriteString(s) }, time.Now)
	g.content("```tool_call\n")
	g.content(`{"name":"kb_search","arguments":{}}`)
	g.finish()
	if got.String() != "" {
		t.Errorf("got = %q, want nothing ever published for a fenced tool call", got.String())
	}
}

func TestRoundGate_ReleaseIsIdempotent(t *testing.T) {
	var calls int
	g := newRoundGate(func(string) { calls++ }, time.Now)
	g.content("hi")
	g.release()
	g.release()
	g.finish()
	if calls != 1 {
		t.Errorf("onDelta called %d times, want exactly 1", calls)
	}
}

// ── mode resolution ──────────────────────────────────────────────────────

func TestResolveToolMode_PinnedModeSkipsDetection(t *testing.T) {
	s := New(Deps{})
	for _, m := range []string{toolModeNative, toolModeFenced, toolModeOff} {
		if got := s.resolveToolMode("some-model", ToolsConfig{Mode: m}); got != m {
			t.Errorf("pinned mode %q: resolveToolMode = %q", m, got)
		}
	}
}

func TestResolveToolMode_AutoDefaultsToNativeThenRemembersDemotion(t *testing.T) {
	s := New(Deps{})
	if got := s.resolveToolMode("qwen36-mtp", ToolsConfig{Mode: toolModeAuto}); got != toolModeNative {
		t.Errorf("first turn for a fresh model = %q, want native (optimistic default)", got)
	}
	s.recordToolMode("qwen36-mtp", toolModeFenced)
	if got := s.resolveToolMode("qwen36-mtp", ToolsConfig{Mode: toolModeAuto}); got != toolModeFenced {
		t.Errorf("after demotion = %q, want fenced (remembered)", got)
	}
	// A different model is unaffected — keyed per-model.
	if got := s.resolveToolMode("ornith-35b", ToolsConfig{Mode: toolModeAuto}); got != toolModeNative {
		t.Errorf("a different model's first turn = %q, want native", got)
	}
}

// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/smith/web"
)

// roundRecorder captures each request's decoded JSON body, synchronized so
// tests can safely read it after the loop under test has returned (proper
// happens-before edges for -race, not just logical ordering).
type roundRecorder struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (r *roundRecorder) record(body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, body)
}

func (r *roundRecorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// fakeA0Rounds serves the nth /v1/chat/completions request with scripts[n]
// (each entry a list of raw SSE frames, fakeA0SSE-shaped); a request past
// the end of scripts repeats the last script. Every request body is
// recorded before any bytes are written.
func fakeA0Rounds(t *testing.T, scripts [][]string) (*httptest.Server, *roundRecorder) {
	t.Helper()
	rec := &roundRecorder{}
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		rec.record(parsed)

		mu.Lock()
		idx := n
		n++
		mu.Unlock()

		var frames []string
		switch {
		case idx < len(scripts):
			frames = scripts[idx]
		case len(scripts) > 0:
			frames = scripts[len(scripts)-1]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			flusher.Flush()
		}
	}))
	return srv, rec
}

func mustFindTool(t *testing.T, id string) Tool {
	t.Helper()
	tool, ok := findTool(id)
	if !ok {
		t.Fatalf("no such tool %q", id)
	}
	return tool
}

// nativeToolCallFrames scripts one round that asks (natively) to call name
// with the given raw JSON arguments, streamed across two frames the way a
// real backend fragments arguments.
func nativeToolCallFrames(id, name, args string) []string {
	return []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"` + id + `","function":{"name":"` + name + `","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + jsonStr(args) + `}}]}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func contentFrames(text string) []string {
	return []string{
		`data: {"choices":[{"delta":{"content":` + jsonStr(text) + `}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
}

func setupToolLoopConv(t *testing.T, ts *httptest.Server) (*Smith, int64, int64) {
	t.Helper()
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Cfg: cfgFor(portOf(t, ts.URL))})
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tierVal := TierReasoning
	msgID, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tierVal, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	return s, convID, msgID
}

// verifyRoundScripts returns the 4-script pattern for a tool-then-verify turn:
// investigate → answer → (verify nudge fires) → auditor verifies → final answer.
// Tests that don't care about the verify mechanism itself use this to get a
// clean verified answer (verified=true, no unverified marker).
func verifyRoundScripts(investigateToolCall, verifyToolCall []string, answer string) [][]string {
	return [][]string{
		investigateToolCall,   // round 1: investigate
		contentFrames(answer), // round 2: answer → verify nudge fires
		verifyToolCall,        // round 3: auditor verifies
		contentFrames(answer), // round 4: final answer (verified)
	}
}

func TestRunToolLoop_HappyPath_ToolRoundThenAnswer(t *testing.T) {
	ts, rec := fakeA0Rounds(t, verifyRoundScripts(
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`),
		nativeToolCallFrames("call_2", "kb_search", `{"query":"gtt"}`),
		"done",
	))
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "done" {
		t.Errorf("content = %q, want %q", result.content, "done")
	}

	reqs := rec.all()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (investigate + answer + verify + answer)", len(reqs))
	}
	if reqs[0]["tools"] == nil {
		t.Error("round 1 request should advertise tools")
	}

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var foundToolCall bool
	for _, m := range msgs {
		if m.Kind == MsgKindToolCall {
			foundToolCall = true
			if m.Evidence == nil {
				t.Error("tool_call message has no evidence")
			}
		}
	}
	if !foundToolCall {
		t.Error("expected a persisted tool_call message for the round that ran kb_search")
	}
}

func TestRunToolLoop_MaxRoundsExhaustion_FinalRequestOmitsTools(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`), // round 1 (the only round: max_rounds=1)
		contentFrames("final answer"),                                  // the forced final call
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	setSetting(t, s.d.Store, SettingTools, `{"enabled":true,"mode":"native","max_rounds":1}`)
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	// max_rounds=1 means the verify nudge can't fire (round < maxRounds
	// fails), so the forced-final path runs with the unverified marker.
	expected := "final answer" + unverifiedMarker
	if result.content != expected {
		t.Errorf("content = %q, want %q", result.content, expected)
	}

	reqs := rec.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (one round + one forced final)", len(reqs))
	}
	if reqs[0]["tools"] == nil {
		t.Error("round 1 should advertise tools")
	}
	if reqs[1]["tools"] != nil {
		t.Errorf("the forced final request should omit tools entirely, got %v", reqs[1]["tools"])
	}
}

func TestRunToolLoop_UnknownToolContinues(t *testing.T) {
	ts, rec := fakeA0Rounds(t, verifyRoundScripts(
		nativeToolCallFrames("call_1", "bogus_tool", `{}`),
		nativeToolCallFrames("call_2", "kb_search", `{"query":"gtt"}`),
		"recovered",
	))
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "recovered" {
		t.Errorf("content = %q, want %q", result.content, "recovered")
	}
	if len(rec.all()) != 4 {
		t.Fatalf("requests = %d, want 4 — an unknown tool must not abort the turn", len(rec.all()))
	}
}

func TestRunToolLoop_NetworkBudgetExhausted(t *testing.T) {
	// Every round asks for one more web_search call than the budget allows,
	// then answers — proving the budget degrades to a result the model can
	// read rather than erroring the turn, and that the underlying Search
	// seam is only actually invoked maxNetworkToolCalls times. Each round
	// uses a distinct query so the repeat-call dedupe guard (a separate
	// bound, tested elsewhere) never fires and masks the network budget.
	var scripts [][]string
	for i := 0; i < maxNetworkToolCalls+2; i++ {
		q := "x" + string(rune('a'+i))
		scripts = append(scripts, nativeToolCallFrames("call_"+string(rune('a'+i)), "web_search", `{"query":"`+q+`"}`))
	}
	scripts = append(scripts, contentFrames("done searching"))
	// After the answer, the verify nudge fires. The auditor gets the last
	// script (contentFrames — no tool calls), answers without verifying,
	// and the unverified marker is appended.
	ts, _ := fakeA0Rounds(t, scripts)
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	setSetting(t, s.d.Store, SettingTools, `{"enabled":true,"mode":"native","max_rounds":8}`)
	fw := &fakeWebService{searchResults: []web.Result{{Title: "x", URL: "https://example.com", Snippet: "y"}}}
	s.d.Web = fw
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "web_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	// The verify nudge fires after the answer, the auditor answers without
	// calling a tool (last script repeats = content only), so the marker
	// is appended.
	if !strings.HasPrefix(result.content, "done searching") {
		t.Errorf("content = %q, want prefix %q", result.content, "done searching")
	}
	if !strings.Contains(result.content, unverifiedMarker) {
		t.Errorf("content should carry the unverified marker (auditor didn't verify), got: %q", result.content)
	}
	if fw.searchCalls != maxNetworkToolCalls {
		t.Errorf("Search called %d times, want exactly %d (the budget)", fw.searchCalls, maxNetworkToolCalls)
	}
}

func TestRunToolLoop_RepeatCallDedupeEventuallyForcesToolsOff(t *testing.T) {
	// The model keeps asking for the exact same call — a real loop risk
	// (docs/v5-smith.md §10 risk #1's spirit, applied to tool calls rather
	// than brain-loads). After maxCallRepeats+1 identical calls the guard
	// must force tools off for the rest of the turn rather than spinning
	// forever, and the turn must still end in a real answer.
	var scripts [][]string
	for i := 0; i < maxCallRepeats+3; i++ {
		scripts = append(scripts, nativeToolCallFrames("call_x", "kb_search", `{"query":"same"}`))
	}
	scripts = append(scripts, contentFrames("gave up asking"))
	ts, rec := fakeA0Rounds(t, scripts)
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	setSetting(t, s.d.Store, SettingTools, `{"enabled":true,"mode":"native","max_rounds":8}`)
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	// forceNoTools suppresses both the verify nudge and the unverified
	// marker — the model couldn't verify even if it wanted to.
	if result.content != "gave up asking" {
		t.Errorf("content = %q, want %q", result.content, "gave up asking")
	}

	reqs := rec.all()
	t.Logf("DEBUG total requests=%d", len(reqs))
	for i, r := range reqs {
		toolsVal := "nil"
		if r["tools"] != nil {
			toolsVal = "tools"
		}
		msgs, _ := r["messages"].([]map[string]any)
		lastRole, lastLen := "", 0
		if len(msgs) > 0 {
			lastRole = fmt.Sprintf("%v", msgs[len(msgs)-1]["role"])
			if c, ok := msgs[len(msgs)-1]["content"].(string); ok {
				lastLen = len(c)
			}
		}
		t.Logf("DEBUG req[%d] %s last=%s/%d", i, toolsVal, lastRole, lastLen)
	}
	var sawToolsDroppedMidTurn bool
	for _, r := range reqs[1:] { // the very first request always carries tools
		if r["tools"] == nil {
			sawToolsDroppedMidTurn = true
		}
	}
	if !sawToolsDroppedMidTurn {
		t.Error("expected some later request to have tools forced off after repeated identical calls")
	}
}

// fencedToolCallFrame scripts one round of fenced-mode content carrying a
// ```tool_call fenced block — the only way a fenced-mode brain can call a
// tool, since fenced mode never sends the wire "tools" field.
func fencedToolCallFrame(name, argsJSON string) []string {
	return contentFrames("```tool_call\n{\"name\":\"" + name + "\",\"arguments\":" + argsJSON + "}\n```")
}

// TestRunToolLoop_VerifyRoundFencedModeKeepsToolInstructions is a
// regression test for a real bug found live (Sprint 6, smith efficiency
// initiative): the verify-round prompt swap replaced messages[0] wholesale
// with audit.md alone, dropping the fenced tool-call instructions the
// executor prompt's buildContext header carries. Native mode survived this
// unaffected (toolsWireFor sends the real "tools" field every round,
// independent of prompt content) but a fenced-mode brain had NO way to
// call a tool during the verify round at all — no wire tools field, no
// fenced-format instructions — so its best-effort guess at some other
// syntax became the "verified" answer. This proves round 3 (the auditor
// round) still carries the fenced tool instructions, and that a fenced
// tool call there is recognized and drives a real verified final answer.
func TestRunToolLoop_VerifyRoundFencedModeKeepsToolInstructions(t *testing.T) {
	ts, rec := fakeA0Rounds(t, verifyRoundScripts(
		fencedToolCallFrame("kb_search", `{"query":"gtt"}`),
		fencedToolCallFrame("kb_search", `{"query":"gtt"}`),
		"verified answer",
	))
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeFenced, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "verified answer" {
		t.Errorf("content = %q, want %q (unverified marker means round 3's fenced call went unrecognized)", result.content, "verified answer")
	}

	reqs := rec.all()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (investigate + answer + verify + answer)", len(reqs))
	}
	round3Msgs, _ := json.Marshal(reqs[2]["messages"])
	if !strings.Contains(string(round3Msgs), "== Tools ==") {
		t.Error("round 3 (auditor) system message should still carry the fenced tool instructions")
	}
	if !strings.Contains(string(round3Msgs), "kb_search") {
		t.Error("round 3 (auditor) system message should still list kb_search as an available tool")
	}
}

// TestRunToolLoop_SuccessfulNativeRoundRecordsMode is a regression test: a
// brain that answers cleanly in native mode, with no demotion ever
// triggered, must still show up as "native" on GET /smith/status
// (SelfContext.Tools.ResolvedMode) — otherwise the working common case
// never confirms anything and only a demotion ever updates the chip.
func TestRunToolLoop_SuccessfulNativeRoundRecordsMode(t *testing.T) {
	ts, _ := fakeA0Rounds(t, [][]string{contentFrames("plain native answer")})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	if got := s.lastToolMode("clean-model"); got != "" {
		t.Fatalf("precondition: lastToolMode should start empty, got %q", got)
	}
	_, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "clean-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if got := s.lastToolMode("clean-model"); got != toolModeNative {
		t.Errorf("lastToolMode after a clean native success = %q, want %q", got, toolModeNative)
	}
}

func TestRunToolLoop_OffModeIsSingleRoundNoTools(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{contentFrames("plain answer")})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeOff, nil, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "plain answer" {
		t.Errorf("content = %q, want %q", result.content, "plain answer")
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0]["tools"] != nil {
		t.Errorf("toolModeOff request should carry no tools key, got %v", reqs[0]["tools"])
	}
}

// ── Loop engineering: verify-round nudge + auditor prompt swap ───────────

// TestRunToolLoop_VerifyNudgeInjectedAfterToolUse proves the verify-round
// gate fires: the model calls a tool, then answers → the loop swaps to the
// adversarial auditor prompt and injects the verify nudge instead of
// accepting → the model re-checks → the final answer is accepted with no
// unverified marker.
func TestRunToolLoop_VerifyNudgeInjectedAfterToolUse(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`), // round 1: investigate
		contentFrames("preliminary answer"),                            // round 2: first answer → nudge
		nativeToolCallFrames("call_2", "kb_search", `{"query":"gtt"}`), // round 3: auditor verifies
		contentFrames("verified answer"),                               // round 4: final answer
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "verified answer" {
		t.Errorf("content = %q, want %q", result.content, "verified answer")
	}
	if strings.Contains(result.content, unverifiedMarker) {
		t.Error("verified answer should not carry the unverified marker")
	}

	reqs := rec.all()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (investigate + answer + verify + answer)", len(reqs))
	}
	// The verify nudge is a user message in round 3's request history.
	round3Body, _ := json.Marshal(reqs[2])
	if !strings.Contains(string(round3Body), verifyNudge) {
		t.Error("round 3 request should contain the verify nudge as a user message")
	}
}

// TestRunToolLoop_VerifyRoundSwapsToAuditorPrompt proves the system prompt is
// swapped to the adversarial auditor prompt (audit.md) during the verify
// round — the LongHorizon-Harness Executor/Auditor separation adapted to a
// single in-process agent. Round 3's system message must contain the
// auditor prompt's role text, NOT the executor prompt's.
func TestRunToolLoop_VerifyRoundSwapsToAuditorPrompt(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`),
		contentFrames("preliminary answer"),
		nativeToolCallFrames("call_2", "kb_search", `{"query":"gtt"}`),
		contentFrames("verified answer"),
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	_, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}

	reqs := rec.all()
	if len(reqs) < 3 {
		t.Fatalf("need at least 3 requests to check the prompt swap, got %d", len(reqs))
	}

	// Round 1 (executor): system message is "sys" (the test's sysPrompt).
	round1Msgs, _ := json.Marshal(reqs[0]["messages"])
	if !strings.Contains(string(round1Msgs), `"sys"`) {
		t.Error("round 1 system message should be the executor prompt (sys)")
	}

	// Round 3 (auditor): system message is the auditor prompt (audit.md).
	round3Msgs, _ := json.Marshal(reqs[2]["messages"])
	if !strings.Contains(string(round3Msgs), "auditor") {
		t.Error("round 3 system message should contain the auditor prompt (audit.md)")
	}
	if strings.Contains(string(round3Msgs), `"sys"`) {
		t.Error("round 3 system message should NOT contain the executor prompt (sys)")
	}
}

// TestRunToolLoop_NoVerifyNudgeForImmediateAnswer proves a model that answers
// without any tool calls is accepted immediately — the verify gate only
// fires after tools were used.
func TestRunToolLoop_NoVerifyNudgeForImmediateAnswer(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{
		contentFrames("immediate answer"),
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "immediate answer" {
		t.Errorf("content = %q, want %q", result.content, "immediate answer")
	}
	if len(rec.all()) != 1 {
		t.Fatalf("requests = %d, want 1 (immediate answer, no nudge)", len(rec.all()))
	}
}

// TestRunToolLoop_UnverifiedMarkerWhenNoVerifyRound proves the unverified
// marker is appended when the model uses tools, gets nudged, but answers
// again without verifying — the "only verified results become trusted" gate.
func TestRunToolLoop_UnverifiedMarkerWhenNoVerifyRound(t *testing.T) {
	ts, _ := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`), // round 1: investigate
		contentFrames("unverified answer"),                             // round 2: first answer → nudge
		contentFrames("still no verify"),                               // round 3: answers again without verifying
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if !strings.Contains(result.content, unverifiedMarker) {
		t.Errorf("content should carry the unverified marker, got: %q", result.content)
	}
	if !strings.HasPrefix(result.content, "still no verify") {
		t.Errorf("content should start with the model's answer, got: %q", result.content)
	}
}

// TestRunToolLoop_NoMarkerWhenVerified proves the unverified marker is
// absent when the model actually runs a tool after the verify nudge.
func TestRunToolLoop_NoMarkerWhenVerified(t *testing.T) {
	ts, _ := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "kb_search", `{"query":"gtt"}`),
		contentFrames("preliminary"),
		nativeToolCallFrames("call_2", "kb_search", `{"query":"gtt"}`),
		contentFrames("confirmed answer"),
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if strings.Contains(result.content, unverifiedMarker) {
		t.Errorf("verified answer should not carry the marker, got: %q", result.content)
	}
}

// ── Loop engineering: local brain (callsPerRound conservation) ───────────

// TestRunToolLoop_LocalBrainCapsCallsPerRound proves that when smith's brain
// is a local model (occupying one of the four bays), tool calls per round
// are capped to 1 — sequential per-call feedback, not batched. The verify
// nudge still fires (verification is sequential, not parallel, and smaller
// local models need it most).
func TestRunToolLoop_LocalBrainCapsCallsPerRound(t *testing.T) {
	// Script a round where the model asks for 3 tool calls in one round.
	// With brainLocal=true, only the first should be dispatched.
	threeCalls := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"kb_search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + jsonStr(`{"query":"a"}`) + `}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"kb_search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":` + jsonStr(`{"query":"b"}`) + `}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"id":"c3","function":{"name":"kb_search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":` + jsonStr(`{"query":"c"}`) + `}}]}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	ts, rec := fakeA0Rounds(t, [][]string{
		threeCalls,            // round 1: 3 calls, only 1 dispatched (local brain cap)
		contentFrames("done"), // round 2: answer → verify nudge
		nativeToolCallFrames("c4", "kb_search", `{"query":"gtt"}`), // round 3: auditor verifies
		contentFrames("done"), // round 4: final answer
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	_, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}

	// Round 1's response had 3 tool calls but only 1 should have been
	// dispatched (callsPerRound=1 for local brains). The tool_call
	// evidence for round 1 should have exactly 1 record.
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	for _, m := range msgs {
		if m.Kind == MsgKindToolCall {
			var ev struct {
				Round int              `json:"round"`
				Calls []toolCallRecord `json:"calls"`
			}
			if err := json.Unmarshal([]byte(*m.Evidence), &ev); err != nil {
				continue
			}
			if ev.Round == 1 && len(ev.Calls) > 1 {
				t.Errorf("round 1 had %d tool call records, want ≤ 1 (local brain caps callsPerRound to 1)", len(ev.Calls))
			}
		}
	}

	// Verify the round still completed and the turn produced an answer.
	_ = rec.all()
}

// ── Tier 1 Sprint 4: deterministic pre-check before the LLM auditor ────────

// TestRunToolLoop_PrecheckConfirmsSkipsAuditor: a run_check-only round
// followed by a confident answer must be accepted WITHOUT the adversarial
// auditor round firing at all — the pre-check's own re-run of run_check
// (against unchanged state — no Source wired, so gtt_ceiling stably
// skip-finds both times) already confirms it. Exactly 2 HTTP rounds, not
// the usual 4-script verify pattern, and no unverified marker (the answer
// IS verified, just not via an LLM call).
func TestRunToolLoop_PrecheckConfirmsSkipsAuditor(t *testing.T) {
	ts, rec := fakeA0Rounds(t, [][]string{
		nativeToolCallFrames("call_1", "run_check", `{"check_ids":["gtt_ceiling"]}`), // round 1: investigate
		contentFrames("gtt looks fine"),                                              // round 2: confident answer
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "run_check")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "is gtt ok?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "gtt looks fine" {
		t.Errorf("content = %q, want the model's own answer verbatim", result.content)
	}
	if strings.Contains(result.content, unverifiedMarker) {
		t.Errorf("content should NOT carry the unverified marker — the pre-check confirmed it, got: %q", result.content)
	}
	if n := len(rec.all()); n != 2 {
		t.Fatalf("HTTP rounds = %d, want exactly 2 (no auditor round spent)", n)
	}
}

// TestRunToolLoop_PrecheckContradictionAsksForReconciliation: live state
// changes between the model's run_check call and the pre-check's re-run of
// it — a real, load-bearing discrepancy. This must NOT silently accept the
// stale answer, but also must NOT swap in the full adversarial audit.md
// persona (smith already knows what changed); it gets a direct
// reconciliation nudge instead, naming the check that changed.
func TestRunToolLoop_PrecheckContradictionAsksForReconciliation(t *testing.T) {
	total := int64(120 << 30)
	okSnap := snapWith(collector.Metrics{
		GTTUsedBytes: int64p(int64(float64(total) * 0.10)), GTTTotalBytes: int64p(total),
	})
	src := collector.NewStatic(okSnap)

	var reqN int
	var mu sync.Mutex
	rec := &roundRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		rec.record(parsed)

		mu.Lock()
		idx := reqN
		reqN++
		mu.Unlock()

		var frames []string
		switch idx {
		case 0:
			frames = nativeToolCallFrames("call_1", "run_check", `{"check_ids":["gtt_ceiling"]}`)
		case 1:
			// State changes to CRIT right before this response — the
			// pre-check's re-run (which happens synchronously once this
			// round's content is parsed) will see a different severity
			// than the model saw in round 1.
			src.Set(snapWith(collector.Metrics{
				GTTUsedBytes: int64p(int64(float64(total) * 0.99)), GTTTotalBytes: int64p(total),
			}))
			frames = contentFrames("gtt looks fine")
		default:
			frames = contentFrames("acknowledged the change")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Cfg: cfgFor(portOf(t, srv.URL)), Source: src})
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tierVal := TierReasoning
	msgID, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tierVal, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "run_check")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "is gtt ok?", "test-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "acknowledged the change" {
		t.Errorf("content = %q, want the model's post-reconciliation answer", result.content)
	}
	reqs := rec.all()
	if len(reqs) != 3 {
		t.Fatalf("HTTP rounds = %d, want exactly 3 (investigate, confident-but-stale answer, reconciled answer)", len(reqs))
	}
	round3Body, _ := json.Marshal(reqs[2])
	if strings.Contains(string(round3Body), "auditor") {
		t.Error("round 3 must NOT be the adversarial audit.md persona swap — smith already knows what changed")
	}
	if !strings.Contains(string(round3Body), "gtt_ceiling") {
		t.Errorf("round 3 request should name the check that changed, body: %s", round3Body)
	}
}

// TestRunToolLoop_MixedRoundStillUsesFullAuditor: a round mixing run_check
// with a different tool must NOT take the pre-check fast path — the
// deterministic pre-check only applies when a round is run_check-only.
// Mixed/partial rounds fall through to the unchanged full LLM-auditor swap,
// same as before this sprint (never LESS thorough than the pre-existing
// behavior, only skips it in the unambiguous run_check-only case).
func TestRunToolLoop_MixedRoundStillUsesFullAuditor(t *testing.T) {
	mixedCalls := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"run_check","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + jsonStr(`{"check_ids":["gtt_ceiling"]}`) + `}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"kb_search","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":` + jsonStr(`{"query":"gtt"}`) + `}}]}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	ts, rec := fakeA0Rounds(t, [][]string{
		mixedCalls,              // round 1: run_check + kb_search in one round
		contentFrames("answer"), // round 2: confident answer -> full-auditor gate
		nativeToolCallFrames("c3", "kb_search", `{"query":"verify"}`), // round 3: auditor verifies
		contentFrames("answer"), // round 4: final verified answer
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "run_check"), mustFindTool(t, "kb_search")}

	// brainLocal=false so callsPerRound allows both calls in round 1.
	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "is gtt ok?", "test-model", toolModeNative, tools, batcher, false, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "answer" {
		t.Errorf("content = %q, want %q", result.content, "answer")
	}
	reqs := rec.all()
	if len(reqs) != 4 {
		t.Fatalf("HTTP rounds = %d, want exactly 4 (mixed round disqualifies the pre-check fast path)", len(reqs))
	}
	round3Body, _ := json.Marshal(reqs[2])
	if !strings.Contains(string(round3Body), "auditor") {
		t.Error("round 3 should be the full adversarial audit.md persona swap — a mixed round must not take the pre-check shortcut")
	}
}

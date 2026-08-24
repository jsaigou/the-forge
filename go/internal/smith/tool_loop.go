// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// tool_loop.go — the P7 multi-round Tier 2 orchestration (docs/v5-smith.md
// §9). runToolLoop replaces P3's single streamed round with up to
// ToolsConfig.MaxRounds rounds of native-or-fenced tool calling, each round
// executing its calls against a fresh *ToolEnv (tools.go) and feeding the
// results back as message history, until the model answers in plain text
// or the loop's bounds are exhausted. Every bound here degrades — a tool
// error, an unparseable call, a repeat call, a round budget, a network
// budget, or the whole turn's time budget all continue or gracefully end
// the turn; none of them wedge it or lose what was already published
// (docs §4.3 "never lose the transcript", extended per-round).

const (
	// defaultToolTimeout/runCheckToolTimeout live in tools.go beside the
	// tools they bound.
	maxToolResultChars  = 6000
	maxNetworkToolCalls = 4 // per turn
	maxCallRepeats      = 2 // a 3rd identical call forces tools off for the rest of the turn
)

// verifyNudge is injected as a user message when the model produces its first
// no-tool-call answer after having used tools — the "Auditor" step adapted
// from LongHorizon-Harness's Plan→Act→Verify→Checkpoint loop. A small local
// model that jumps straight from one tool call to a confident answer is the
// failure mode this gate exists to catch. If the model then calls a tool,
// verified is set and the next answer is accepted cleanly. If it answers
// again without verifying, the answer is accepted (never loop forever) but
// the unverifiedMarker is appended so the operator sees the gap.
const verifyNudge = "Before answering: re-run the check most relevant to your conclusion (via run_check) to confirm it holds against live state. If it confirms, answer now and cite it. If you cannot verify, say so explicitly."

// unverifiedMarker is appended to the final answer when tools were used but
// no verification round ran — the "only verified results become trusted"
// principle. forceNoTools suppresses it: if tools were taken away (repeat
// dedupe), the model couldn't verify even if it wanted to.
const unverifiedMarker = "\n\n[unverified — no verification check was run before this answer]"

// toolLoopResult is runToolLoop's success shape.
type toolLoopResult struct {
	content string
	sources []MessageSource
	// Rounds is how many streaming rounds the loop consumed (S4 turn
	// instrumentation). 0 for the no-tools short-circuit (one implicit
	// round).
	rounds int
}

// clientErrorStatus extracts an HTTP status from streamChatCompletion's
// "smith: chat completions HTTP %d: …" error, or 0 when the error isn't
// that shape (a transport failure, a config error, etc).
var httpStatusInError = regexp.MustCompile(`chat completions HTTP (\d{3})`)

func clientErrorStatus(err error) int {
	if err == nil {
		return 0
	}
	m := httpStatusInError.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	var code int
	fmt.Sscanf(m[1], "%d", &code)
	return code
}

// runToolLoop drives one Tier 2 turn once mode/tools have been resolved by
// the caller (runReasoningTurn). mode == toolModeOff or an empty tools list
// short-circuits to a single P3-shaped round (byte-identical wire request —
// TestChatRequest_NoToolsByteIdenticalToP3 covers the request itself; this
// is the orchestration-level mirror of that guarantee).
func (s *Smith) runToolLoop(ctx context.Context, convID, msgID int64, sysPrompt, userText, model, mode string, tools []Tool, batcher *tokenBatcher, brainLocal bool, baseURLOverride string) (toolLoopResult, error) {
	if mode == toolModeOff || len(tools) == 0 {
		cr, err := s.streamChatCompletion(ctx, chatRequest{
			Model:           model,
			Messages:        []chatWireMessage{{Role: "system", Content: sysPrompt}, {Role: "user", Content: userText}},
			BaseURLOverride: baseURLOverride,
		}, batcher.add)
		if err != nil {
			return toolLoopResult{}, err
		}
		return toolLoopResult{content: cr.Content}, nil
	}

	tctx, cancel := context.WithTimeout(ctx, turnBudget)
	defer cancel()


	messages := []chatWireMessage{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userText},
	}

	maxRounds := s.ToolsConfig(ctx).MaxRounds
	env := s.toolEnv(ctx)

	// Local brains occupy one of the four bays — each inference round is
	// slot time taken from real inference work. Cap calls-per-round to 1
	// (sequential per-call feedback, not batched) and skip the verify nudge
	// (the brainLocal gate below). Remote brains don't occupy a local slot,
	// so the full loop-engineering behavior applies.
	callsPerRound := maxToolCallsPerRound
	if brainLocal {
		callsPerRound = 1
	}

	var allSources []MessageSource
	repeatCount := map[string]int{}
	netCalls := 0
	forceNoTools := false
	zeroStreak := 0
	firstRoundRetried := false
	publishedAny := false

	// Phase tracking (loop-engineering adaptation): toolsUsed marks that
	// the model called at least one tool; verifyNudged marks that the
	// verify-round nudge has been injected; verified marks that a tool was
	// called AFTER the nudge (the model took another look before answering).
	toolsUsed := false
	verifyNudged := false
	verified := false

	// NOTE: deliberately NOT a post-statement loop — this body ends with
	// its own `round++`, and the verify-gate below also does `round++;
	// continue` (consuming an extra round number per nudge by design).
	// A `for round := 1; ...; round++` here double-steps every iteration
	// and burns the script/round budget twice as fast (S4 regression,
	// caught by TestRunToolLoop_NetworkBudgetExhausted).
	round := 1
	for round <= maxRounds {
		if tctx.Err() != nil {
			return toolLoopResult{content: budgetExhaustedNotice(publishedAny), sources: allSources}, nil
		}

		wireTools := toolsWireFor(mode, tools)
		if forceNoTools {
			wireTools = nil
		}
		req := chatRequest{Model: model, Messages: messages, Tools: wireTools, BaseURLOverride: baseURLOverride}
		roundCtx, roundCancel := context.WithTimeout(tctx, chatTimeout)

		var cr *chatRound
		var err error
		if mode == toolModeFenced {
			gate := newRoundGate(func(d string) { batcher.add(d); publishedAny = true }, s.d.Now)
			cr, err = s.streamChatCompletion(roundCtx, req, gate.content)
			gate.finish()
		} else {
			cr, err = s.streamChatCompletion(roundCtx, req, func(d string) { batcher.add(d); publishedAny = true })
		}
		roundCancel()

		if err != nil {
			// Demotion signal A: the model's native tools request was
			// rejected outright — demote and retry THIS round immediately
			// (doesn't consume the round budget; the retried attempt does).
			if mode == toolModeNative && len(wireTools) > 0 {
				if code := clientErrorStatus(err); code >= 400 && code < 500 {
					s.recordToolMode(model, toolModeFenced)
					mode = toolModeFenced
					continue
				}
			}
			if tctx.Err() != nil {
				return toolLoopResult{content: budgetExhaustedNotice(publishedAny), sources: allSources}, nil
			}
			// One silent retry of round 1 only, and only when nothing has
			// been published yet — the narrowed retry rule (P7 fixes the
			// pre-existing P3 double-publish bug: a retry after partial
			// content re-publishes into the FE's append-only stream slot).
			if round == 1 && !firstRoundRetried && !publishedAny {
				firstRoundRetried = true
				continue
			}
			return toolLoopResult{content: "", sources: allSources}, err
		}

		content, calls := s.parseRoundCalls(cr, model, &mode)
		// Record the mode that just worked, not only a demotion — otherwise
		// a brain that's been native all along never confirms it on
		// GET /smith/status, defeating the "run one turn, read the chip"
		// verification story §A4 exists for.
		s.recordToolMode(model, mode)

		if !cr.SawDelta {
			zeroStreak++
		} else {
			zeroStreak = 0
		}

		if len(calls) == 0 {
			// Verify-round gate: if the model used tools and hasn't been
			// nudged yet (and tools aren't forced off and rounds remain),
			// inject the verification nudge instead of accepting. This is
			// the single highest-impact loop-engineering adaptation — a
			// small model that calls one tool and jumps to a confident
			// answer gets one more chance to confirm against live state.
			// Applies to local brains too: the verify round is sequential
			// (not parallel), so it costs one extra short inference call, and
			// smaller local models are the ones that need verification most.
			if toolsUsed && !verifyNudged && !forceNoTools && round < maxRounds {
				// Swap to the adversarial auditor system prompt — same model,
				// different role. The auditor sees the full investigation
				// history but is instructed not to trust it and to verify
				// independently. This is the LongHorizon-Harness separation
				// of Executor (prompt.md) from Auditor (audit.md) adapted to
				// a single in-process agent.
				//
				// toolsInstructionBlock is re-appended here, not just carried
				// over from the executor prompt: replacing messages[0]
				// wholesale drops whatever tool text the executor's
				// buildContext header had. In native mode that's harmless
				// (toolsWireFor sends the real `tools` field every round,
				// unaffected by system-prompt content), but in fenced mode
				// tools are ONLY ever offered via prompt text — with no
				// instructions here, a fenced-mode brain has no way to
				// re-verify via a real tool call, and its best-effort guess
				// at a call syntax becomes the "verified" answer instead
				// (found live, Sprint 6, smith efficiency initiative — smith's
				// real fenced-mode production brain was silently affected).
				messages[0] = chatWireMessage{Role: "system", Content: embeddedAuditPrompt + "\n" + toolsInstructionBlock(tools, mode)}
				messages = append(messages, chatWireMessage{Role: "user", Content: verifyNudge})
				verifyNudged = true
				round++
				continue
			}
			// Accept the answer. If tools were used but no verification
			// round ran, mark the answer as unverified (forceNoTools
			// suppresses — the model couldn't verify even if it wanted to).
			if toolsUsed && !verified && !forceNoTools {
				content += unverifiedMarker
			}
			return toolLoopResult{content: content, sources: allSources}, nil
		}
		if zeroStreak >= 2 {
			return toolLoopResult{content: "", sources: allSources}, fmt.Errorf("smith: the reasoning tier stopped producing output")
		}
		// Phase tracking: tools were used this round. If the verify nudge
		// already fired, any tool call counts as a verification step.
		toolsUsed = true
		if verifyNudged {
			verified = true
		}
		if len(calls) > callsPerRound {
			calls = calls[:callsPerRound]
		}

		wireCalls := make([]toolCallWire, len(calls))
		for i, c := range calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", round, i)
			}
			wireCalls[i] = toolCallWire{ID: id, Type: "function", Function: toolCallFunctionWire{Name: c.Name, Arguments: string(c.Args)}}
		}
		if mode == toolModeNative {
			messages = append(messages, chatWireMessage{Role: "assistant", Content: content, ToolCalls: wireCalls})
		} else {
			messages = append(messages, chatWireMessage{Role: "assistant", Content: content})
		}

		records := make([]toolCallRecord, 0, len(calls))
		for i, c := range calls {
			id := wireCalls[i].ID
			dupeKey := c.Name + ":" + string(c.Args)
			dupeHash := sha256Hex(dupeKey)

			started := s.d.Now()
			s.publishToolCall(convID, msgID, round, c.Name, "started", "")

			resultVal, callErr := s.dispatchTool(tctx, env, c, dupeHash, repeatCount, &netCalls, &forceNoTools)
			dur := s.d.Now().Sub(started)

			var src []MessageSource
			if wtr, ok := resultVal.(webToolResult); ok {
				src = wtr.Sources
				resultVal = wtr.Payload
			}
			allSources = mergeSources(allSources, src)

			status, errStr := "done", ""
			if callErr != nil {
				status = "error"
				errStr = scrubSecretPatterns(callErr.Error())
				resultVal = map[string]any{"error": errStr}
			}
			summary := summarizeToolResult(resultVal)
			s.publishToolCall(convID, msgID, round, c.Name, status, summary)

			resultJSON := truncateJSON(resultVal, maxToolResultChars)
			role, callID, name, msgContent := "tool", id, c.Name, resultJSON
			if mode != toolModeNative {
				role, callID, name = "user", "", ""
				msgContent = fmt.Sprintf("TOOL RESULT (%s): %s", c.Name, resultJSON)
			}
			messages = append(messages, chatWireMessage{Role: role, ToolCallID: callID, Name: name, Content: msgContent})

			var errPtr *string
			if errStr != "" {
				errPtr = &errStr
			}
			records = append(records, toolCallRecord{ID: id, Name: c.Name, Args: json.RawMessage(c.Args), OK: callErr == nil, Summary: summary, DurationMS: dur.Milliseconds(), Error: errPtr, Verified: verifyNudged})
		}
		if aerr := s.appendToolCallMessage(ctx, convID, round, records); aerr != nil {
			s.logf("tool loop: append tool_call message: %v", aerr)
		}

		round++
	}

	// Rounds exhausted while the model was still calling tools — force a
	// final answer with Tools omitted entirely (removing the affordance
	// beats asking politely) and a nudge appended to the transcript.
	messages = append(messages, chatWireMessage{Role: "user", Content: "You have used all available tool calls for this turn. Answer now in plain text using everything above — do not attempt another tool call. Cite which tool result confirms each claim. Mark anything you couldn't verify as [unverified]."})
	roundCtx, roundCancel := context.WithTimeout(tctx, chatTimeout)
	defer roundCancel()
	cr, err := s.streamChatCompletion(roundCtx, chatRequest{Model: model, Messages: messages, BaseURLOverride: baseURLOverride}, func(d string) { batcher.add(d); publishedAny = true })
	if err != nil {
		if tctx.Err() != nil {
			return toolLoopResult{content: budgetExhaustedNotice(publishedAny), sources: allSources}, nil
		}
		return toolLoopResult{content: "", sources: allSources}, err
	}
	finalContent := cr.Content
	if toolsUsed && !verified && !forceNoTools {
		finalContent += unverifiedMarker
	}
	return toolLoopResult{content: finalContent, sources: allSources}, nil
}

// parseRoundCalls extracts a round's tool calls per the current mode,
// implementing demotion signal B: a native round whose model wanted to
// tool-call but whose template can't emit tool_calls will instead leak a
// ```tool_call fence into plain content — recognizing that here (with zero
// wasted rounds, unlike signal A) flips *mode to fenced for the rest of the
// turn and for this model's future turns (recordToolMode).
func (s *Smith) parseRoundCalls(cr *chatRound, model string, mode *string) (content string, calls []toolCallReq) {
	if *mode == toolModeNative {
		if len(cr.ToolCalls) > 0 {
			return cr.Content, cr.ToolCalls
		}
		fencedCalls, stripped, sawFence := parseFencedToolCalls(cr.Content)
		if sawFence {
			s.recordToolMode(model, toolModeFenced)
			*mode = toolModeFenced
			return stripped, fencedCalls
		}
		return cr.Content, nil
	}
	fencedCalls, stripped, _ := parseFencedToolCalls(cr.Content)
	return stripped, fencedCalls
}

// dispatchTool runs one tool call, applying the network/dedupe/panic
// guardrails, and never returns a callErr for a budget-exhausted network
// tool — that degrades to a result the model can read and route around
// (docs §9: "the budget, not the checkbox, is what makes web-on-every-turn
// safe").
func (s *Smith) dispatchTool(ctx context.Context, env *ToolEnv, c toolCallReq, dupeHash string, repeatCount map[string]int, netCalls *int, forceNoTools *bool) (any, error) {
	tool, ok := findTool(c.Name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q — valid tools: %s", c.Name, strings.Join(toolIDs(toolRegistry), ", "))
	}

	repeatCount[dupeHash]++
	if repeatCount[dupeHash] > maxCallRepeats+1 {
		*forceNoTools = true
		return nil, fmt.Errorf("%s: called with identical arguments too many times this turn — stop calling this and answer with what you already have", c.Name)
	}

	if tool.Network {
		if *netCalls >= maxNetworkToolCalls {
			return map[string]any{"unavailable": "outbound network budget for this turn is exhausted"}, nil
		}
		*netCalls++
	}

	timeout := tool.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return runTool(toolCtx, env, tool, c.Args)
}

// runTool invokes one tool's Run, recovering a panic into an error exactly
// as checks.go's runOne does — non-negotiable here, since this runs on
// s.bgCtx and an unrecovered panic would kill forge, not just this turn.
func runTool(ctx context.Context, env *ToolEnv, t Tool, args json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("%s: panicked: %v", t.ID, r)
		}
	}()
	return t.Run(ctx, env, args)
}

// toolsWireFor returns the OpenAI tools array for a native-mode request, or
// nil for fenced/off (fenced mode advertises tools via the system prompt's
// toolsInstructionBlock instead — reasoning.go).
func toolsWireFor(mode string, tools []Tool) []toolWire {
	if mode != toolModeNative || len(tools) == 0 {
		return nil
	}
	out := make([]toolWire, len(tools))
	for i, t := range tools {
		out[i] = toolWire{Type: "function", Function: toolFunctionWire{Name: t.ID, Description: t.Description, Parameters: t.Params}}
	}
	return out
}

// ── persistence + SSE ────────────────────────────────────────────────────

// toolCallRecord is one call's outcome inside a round's tool_call message
// evidence (MsgKindToolCall, appendToolCallMessage).
type toolCallRecord struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	OK         bool            `json:"ok"`
	Summary    string          `json:"summary"`
	DurationMS int64           `json:"duration_ms"`
	Error      *string         `json:"error"`
	// Verified is true when this call was made after the verify-round nudge
	// (the "Auditor" phase). Additive, omitempty — records from before this
	// field existed decode fine.
	Verified bool `json:"verified,omitempty"`
}

// appendToolCallMessage persists one round's calls as a tool_call message
// (conversations.go's MsgKindToolCall, reusing the evidence column — no new
// column, see docs/v5-smith.md §4.6's P7 amendment). Written as the round
// completes, before the next round is issued, so a turn that dies partway
// through still shows every round that finished (P5's "never lose what you
// read", extended to tool activity).
func (s *Smith) appendToolCallMessage(ctx context.Context, convID int64, round int, calls []toolCallRecord) error {
	evidence, err := json.Marshal(map[string]any{"round": round, "calls": calls})
	if err != nil {
		return fmt.Errorf("smith: marshal tool_call evidence: %w", err)
	}
	evStr := string(evidence)
	content := summarizeRound(round, calls)
	_, err = s.appendMessage(ctx, convID, MsgKindToolCall, content, &evStr, nil, nil, nil, nil)
	return err
}

func summarizeRound(round int, calls []toolCallRecord) string {
	names := make([]string, len(calls))
	worst := "ok"
	for i, c := range calls {
		names[i] = c.Name
		if !c.OK {
			worst = "error"
		}
	}
	return fmt.Sprintf("round %d: ran %s (%s)", round, strings.Join(names, ", "), worst)
}

func summarizeToolResult(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return truncateForContext(string(b), 200)
}

// truncateJSON marshals v and, if it exceeds max, replaces it with a
// smaller-but-still-valid JSON object carrying a preview — never dropped
// outright, since an unanswered tool_call_id breaks the next round's wire
// contract (the one place buildContext's drop-whole-blocks rule must not
// be copied).
func truncateJSON(v any, max int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"result was not serializable"}`
	}
	if len(b) <= max {
		return string(b)
	}
	preview := string(b[:max])
	out, _ := json.Marshal(map[string]any{"truncated": true, "preview": preview})
	return string(out)
}

// mergeSources unions two source lists, deduped by URL — the web tools'
// results merge into whatever researchForTurn (P5) already collected this
// turn, so both light up the same SourcesList.tsx rendering.
func mergeSources(existing, added []MessageSource) []MessageSource {
	if len(added) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing))
	out := make([]MessageSource, 0, len(existing)+len(added))
	for _, s := range existing {
		if !seen[s.URL] {
			seen[s.URL] = true
			out = append(out, s)
		}
	}
	for _, s := range added {
		if !seen[s.URL] {
			seen[s.URL] = true
			out = append(out, s)
		}
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func budgetExhaustedNotice(publishedAny bool) string {
	if publishedAny {
		return "\n\nsmith: this turn hit its time budget mid-investigation — the above reflects what was found so far."
	}
	return "smith: this turn hit its time budget before producing an answer. Try a narrower question, or try again."
}

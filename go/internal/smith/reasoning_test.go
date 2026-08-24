// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith/web"
)

// fakeA0SSE serves a scripted /v1/chat/completions SSE stream. frames are
// written verbatim (each already "data: ...\n\n"-shaped, or a raw line for
// malformed-frame tolerance tests), flushed individually to exercise the
// real streaming path rather than one buffered write.
func fakeA0SSE(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			// decideTier probes /healthz before escalating to Tier 2.
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, f := range frames {
			w.Write([]byte(f))
			flusher.Flush()
		}
	}))
}

func cfgFor(port int) func() *config.Config {
	return func() *config.Config {
		return &config.Config{Server: config.Server{RouterListen: ":" + strconv.Itoa(port)}}
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ── streamChatCompletion (the SSE client parser) ────────────────────────

func TestStreamChatCompletion_ParsesDeltas(t *testing.T) {
	ts := fakeA0SSE(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo, \"}}]}\n\n",
		": this is an SSE comment line, ignored\n\n",
		"data: {\"choices\":[{\"delta\":{}}]}\n\n", // no content field — no delta call
		"not a data line at all\n",
		"data: {malformed json\n\n", // tolerated, not fatal
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer ts.Close()

	s := New(Deps{Cfg: cfgFor(portOf(t, ts.URL))})
	var got strings.Builder
	round, err := s.streamChatCompletion(context.Background(),
		chatRequest{Model: "test-model", Messages: []chatWireMessage{{Role: "user", Content: "hi"}}},
		func(delta string) { got.WriteString(delta) })
	if err != nil {
		t.Fatalf("streamChatCompletion: %v", err)
	}
	if got.String() != "Hello, world" {
		t.Errorf("assembled content = %q, want %q", got.String(), "Hello, world")
	}
	if round.Content != "Hello, world" {
		t.Errorf("round.Content = %q, want %q", round.Content, "Hello, world")
	}
}

// TestStreamChatCompletion_SurvivesShortHTTPClientTimeout is a regression
// test for a real bug found live on ForgeHost: s.d.HTTPClient's blanket
// http.Client.Timeout (New()'s default 3s, sized for quick healthz probes)
// was being applied directly to the streaming call, cutting every real
// generation off at 3s regardless of the much longer ctx deadline
// streamChatCompletion is supposed to be bounded by instead.
func TestStreamChatCompletion_SurvivesShortHTTPClientTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"slow\"}}]}\n\n"))
		flusher.Flush()
		time.Sleep(150 * time.Millisecond) // longer than the short client.Timeout below
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer ts.Close()

	s := New(Deps{
		Cfg:        cfgFor(portOf(t, ts.URL)),
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var got strings.Builder
	_, err := s.streamChatCompletion(ctx,
		chatRequest{Model: "test-model", Messages: []chatWireMessage{{Role: "user", Content: "hi"}}},
		func(delta string) { got.WriteString(delta) })
	if err != nil {
		t.Fatalf("streamChatCompletion: %v (should be bounded by ctx, not HTTPClient.Timeout)", err)
	}
	if got.String() != "slow" {
		t.Errorf("content = %q, want %q", got.String(), "slow")
	}
}

func TestStreamChatCompletion_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"backend down"}`))
	}))
	defer ts.Close()

	s := New(Deps{Cfg: cfgFor(portOf(t, ts.URL))})
	_, err := s.streamChatCompletion(context.Background(),
		chatRequest{Model: "test-model", Messages: []chatWireMessage{{Role: "user", Content: "hi"}}}, func(string) {})
	if err == nil {
		t.Fatal("expected an error for HTTP 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want it to mention 502", err)
	}
}

func TestStreamChatCompletion_EmptyModel(t *testing.T) {
	s := New(Deps{})
	_, err := s.streamChatCompletion(context.Background(), chatRequest{}, func(string) {})
	if err == nil {
		t.Fatal("expected an error for empty model")
	}
}

func TestStreamChatCompletion_NoConfig(t *testing.T) {
	s := New(Deps{})
	_, err := s.streamChatCompletion(context.Background(), chatRequest{Model: "m"}, func(string) {})
	if !errors.Is(err, ErrCfgUnwired) {
		t.Errorf("err = %v, want ErrCfgUnwired", err)
	}
}

// ── Chat() orchestration ─────────────────────────────────────────────────

func TestChat_DeterministicWhenNoBrain(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		_, msgs, err := s.GetConversation(ctx, convID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.ID == msgID && m.Content != "" {
				return true
			}
		}
		return false
	})

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var reply *Message
	for i := range msgs {
		if msgs[i].ID == msgID {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("assistant message not found")
	}
	// S4: kind stays whatever the placeholder was created as (reasoning when
	// escalation intent existed); the degrade contract is the deterministic
	// TIER plus the grounded answer text.
	if reply.Tier == nil || *reply.Tier != TierDeterministic {
		t.Errorf("tier = %v, want %q", reply.Tier, TierDeterministic)
	}
	if !strings.Contains(reply.Content, "Brain:") {
		t.Errorf("content = %q, want a grounded answer", reply.Content)
	}
}

func TestChat_ReasoningEscalation_Succeeds(t *testing.T) {
	ts := fakeA0SSE(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"the box \"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"is fine\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer ts.Close()

	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	pub := &stubPublisher{}

	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched:     newStubSched(map[string]string{"a3": "ornith-35b"}),
		Cfg:       cfgFor(portOf(t, ts.URL)),
		Publisher: pub,
	})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	// escalate=true so decideTier picks reasoning without depending on
	// autoEscalate's finding-count heuristic.
	msgID, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{Escalate: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	waitFor(t, time.Second, func() bool { return pub.has(EventMessageDone) })

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var reply *Message
	for i := range msgs {
		if msgs[i].ID == msgID {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("assistant message not found")
	}
	if reply.Content != "the box is fine" {
		t.Errorf("content = %q, want %q", reply.Content, "the box is fine")
	}
	if reply.Kind != MsgKindReasoning {
		t.Errorf("kind = %q, want %q", reply.Kind, MsgKindReasoning)
	}
	if reply.Model == nil || *reply.Model != "ornith-35b" {
		t.Errorf("model = %v, want ornith-35b", reply.Model)
	}
	if reply.Error != nil {
		t.Errorf("error = %v, want nil", reply.Error)
	}
	if !pub.has(EventToken) {
		t.Error("expected at least one smith:token event")
	}

	conv, _, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.Tier != TierReasoning {
		t.Errorf("conversation tier = %q, want %q", conv.Tier, TierReasoning)
	}
}

// TestChat_ReasoningEscalation_LoadsBrainOnDemand proves decideTier's new
// on-demand path end-to-end: a configured local brain that ISN'T loaded
// anywhere yet still reaches the reasoning tier, because an escalating
// turn triggers ensureBrainLoaded before giving up — the gap this session
// found (Brain() alone never triggers a load, and nothing else did either).
func TestChat_ReasoningEscalation_LoadsBrainOnDemand(t *testing.T) {
	ts := fakeA0SSE(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"loaded and answered\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer ts.Close()

	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	pub := &stubPublisher{}

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, TargetSlotToReport: "a4"} // starts with nothing loaded
	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched:     stub,
		Cfg:       cfgFor(portOf(t, ts.URL)),
		Publisher: pub,
	})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{Escalate: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	waitFor(t, time.Second, func() bool { return pub.has(EventMessageDone) })

	if stub.callCount() != 1 {
		t.Errorf("EnsureLoaded calls = %d, want 1 (on-demand load)", stub.callCount())
	}

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var reply *Message
	for i := range msgs {
		if msgs[i].ID == msgID {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("assistant message not found")
	}
	if reply.Content != "loaded and answered" {
		t.Errorf("content = %q, want %q", reply.Content, "loaded and answered")
	}
	if reply.Kind != MsgKindReasoning {
		t.Errorf("kind = %q, want reasoning — the on-demand load should have let this escalate", reply.Kind)
	}
}

// TestChat_ReasoningEscalation_LoadFailureDegradesGracefully proves the
// failure side: when the on-demand load can't complete, the turn degrades
// to deterministic cleanly rather than erroring or hanging.
func TestChat_ReasoningEscalation_LoadFailureDegradesGracefully(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	stub := &ensureLoadedStub{Stub: &sched.Stub{}, Err: errors.New("no idle slot available")}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: stub})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{Escalate: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		_, msgs, err := s.GetConversation(ctx, convID)
		if err != nil {
			return false
		}
		for _, m := range msgs {
			if m.ID == msgID && m.Content != "" {
				return true
			}
		}
		return false
	})

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var reply *Message
	for i := range msgs {
		if msgs[i].ID == msgID {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("assistant message not found")
	}
	// S4: load failure degrades asynchronously — tier flips to
	// deterministic and a notice explains why; kind may remain reasoning.
	if reply.Tier != nil && *reply.Tier != TierDeterministic {
		t.Errorf("tier = %v, want deterministic after load-failure degrade", *reply.Tier)
	}
	if !strings.Contains(reply.Content, "Brain:") {
		t.Errorf("content = %q, want the grounded fallback answer", reply.Content)
	}
}

// TestChat_A0DownDirectConnectFallback proves the a0-down fallback: a0 is
// unreachable, but the brain IS resident on a real slot — the turn still
// reaches the reasoning tier by connecting directly to that slot's own
// port (config.Config.Slots, never a hardcoded map), not through a0.
func TestChat_A0DownDirectConnectFallback(t *testing.T) {
	slotTS := fakeA0SSE(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"direct answer\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer slotTS.Close()
	slotPort := portOf(t, slotTS.URL)

	// A real port that nothing is listening on — stands in for a0 being down.
	deadTS := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	a0Port := portOf(t, deadTS.URL)
	deadTS.Close()

	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	pub := &stubPublisher{}

	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched: newStubSched(map[string]string{"a3": "ornith-35b"}), // resident, not a1
		Cfg: func() *config.Config {
			return &config.Config{
				Server: config.Server{RouterListen: ":" + strconv.Itoa(a0Port)},
				Slots:  map[string]config.Slot{"a3": {Port: slotPort}},
			}
		},
		Publisher: pub,
	})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "why is the box slow?", ChatOptions{Escalate: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	waitFor(t, time.Second, func() bool { return pub.has(EventMessageDone) })

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	var reply *Message
	for i := range msgs {
		if msgs[i].ID == msgID {
			reply = &msgs[i]
		}
	}
	if reply == nil {
		t.Fatal("assistant message not found")
	}
	if reply.Content != "direct answer" {
		t.Errorf("content = %q, want %q (should have connected directly to the slot)", reply.Content, "direct answer")
	}
	if reply.Kind != MsgKindReasoning {
		t.Errorf("kind = %q, want reasoning — a0 being down should not block a resident local brain", reply.Kind)
	}
}

func TestRunReasoningTurn_DegradesOnFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	pub := &stubPublisher{}

	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched:     newStubSched(map[string]string{"a3": "ornith-35b"}),
		Cfg:       cfgFor(portOf(t, ts.URL)),
		Publisher: pub,
		Logf:      func(string, ...any) {},
	})
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

	// Direct, synchronous call — exercises the retry-then-degrade path
	// without the Chat()/goroutine indirection.
	s.runReasoningTurn(ctx, convID, msgID, "why is the box slow?", nil, nil, true)

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs) != 2 { // finalized (degraded) reply + the notice
		t.Fatalf("messages = %d, want 2 (degraded reply + notice)", len(msgs))
	}
	reply := msgs[0]
	if reply.Content == "" || !strings.Contains(reply.Content, "Brain:") {
		t.Errorf("degraded reply content = %q, want a grounded answer", reply.Content)
	}
	notice := msgs[1]
	if notice.Kind != MsgKindNotice {
		t.Errorf("second message kind = %q, want %q", notice.Kind, MsgKindNotice)
	}
	if !strings.Contains(notice.Content, "thinking failed") {
		t.Errorf("notice content = %q, want it to explain the degrade", notice.Content)
	}
	if !pub.has(EventTierChanged) {
		t.Error("expected a smith:tier_changed event")
	}
	if !pub.has(EventMessageDone) {
		t.Error("expected a smith:message_done event")
	}

	if s.chatFailures[convID] != 1 {
		t.Errorf("chatFailures[convID] = %d, want 1", s.chatFailures[convID])
	}
}

func TestRunReasoningTurn_BudgetExceededSkipsA0Entirely(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	db := openDB(t)
	s := New(Deps{Store: db, Cfg: cfgFor(portOf(t, ts.URL))})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	for i := 0; i < maxChatFailuresPerConversation; i++ {
		s.recordChatFailure(convID)
	}

	tierVal := TierReasoning
	msgID, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tierVal, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	s.runReasoningTurn(ctx, convID, msgID, "hello", nil, nil, true)

	if called {
		t.Error("a0 should never have been called once the retry budget is exceeded")
	}
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if !strings.Contains(msgs[1].Content, "too many thinking failures") {
		t.Errorf("notice = %q, want the budget-exceeded message", msgs[1].Content)
	}
}

// ── context assembly + redaction ────────────────────────────────────────

func TestBuildContext_RedactsFindingEvidence(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Now: func() time.Time { return time.Unix(1000, 0) }})
	ctx := context.Background()

	_, err := s.persistFindings(ctx, []Finding{{
		CheckID:  "compressor_reachability",
		Severity: SeverityWarn,
		Summary:  "proxy degraded",
		Evidence: map[string]any{"active_token": "sk-router-shouldnotleak-abcdefgh", "proxy": "deepseek"},
	}}, "manual", time.Unix(1000, 0), nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	got := s.buildContext(ctx, "what's wrong with compressor?", nil, nil, "")
	if strings.Contains(got, "sk-router-shouldnotleak") {
		t.Errorf("buildContext leaked a secret: %s", got)
	}
	if !strings.Contains(got, "proxy degraded") {
		t.Errorf("buildContext dropped the finding summary entirely: %s", got)
	}
	if !strings.Contains(got, redactedPlaceholder) {
		t.Errorf("buildContext should show the redacted placeholder in evidence: %s", got)
	}
}

func TestBuildContext_DropsLowestPriorityBlocksOverBudget(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	s := New(Deps{Store: db, Catalog: db.Catalog()})
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_, err := s.persistFindings(ctx, []Finding{{
			CheckID:  "check_" + strconv.Itoa(i),
			Severity: SeverityInfo,
			Summary:  strings.Repeat("x", 2000),
			Evidence: map[string]any{"i": i},
		}}, "manual", time.Unix(int64(i), 0), nil)
		if err != nil {
			t.Fatalf("persistFindings: %v", err)
		}
	}

	got := s.buildContext(ctx, "mentions ornith-35b by name", nil, nil, "")
	if len(got) > contextCharBudget+2000 { // header + self-context always survive whole
		t.Errorf("buildContext len = %d, want it capped near contextCharBudget (%d)", len(got), contextCharBudget)
	}
	if !strings.Contains(got, "Self context") {
		t.Error("self context block must never be dropped")
	}
}

func TestBuildContext_WebBlockPositionedSecond(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	_, err := s.persistFindings(ctx, []Finding{{
		CheckID: "gtt_ceiling", Severity: SeverityWarn, Summary: "some finding",
	}}, "manual", time.Unix(1000, 0), nil)
	if err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	docs := []*web.Document{{Provider: "direct", URL: "https://example.com", Title: "Example", Text: "web content here"}}
	got := s.buildContext(ctx, "x", docs, nil, "")

	selfIdx := strings.Index(got, "Self context")
	webIdx := strings.Index(got, "Web sources")
	findingsIdx := strings.Index(got, "Recent findings")
	if selfIdx < 0 || webIdx < 0 || findingsIdx < 0 {
		t.Fatalf("expected all three blocks present, got: %s", got)
	}
	if !(selfIdx < webIdx && webIdx < findingsIdx) {
		t.Errorf("block order wrong: self=%d web=%d findings=%d, want self < web < findings", selfIdx, webIdx, findingsIdx)
	}
	if !strings.Contains(got, "untrusted external content") {
		t.Error("web block should carry the untrusted-content disclaimer")
	}
	if !strings.Contains(got, "web content here") {
		t.Error("web block should carry the fetched document text")
	}
}

func TestBuildContext_WebBlockSurvivesBudgetPressureOverKB(t *testing.T) {
	// A web:true turn's own research must never be the thing dropped in
	// favour of unrequested background evidence (KB matches) when the
	// context budget is tight — web.go's kbBlock is the lowest-priority
	// block, so it should be the one to go.
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_, err := s.persistFindings(ctx, []Finding{{
			CheckID: "check_" + strconv.Itoa(i), Severity: SeverityInfo,
			Summary: strings.Repeat("f", 2000),
		}}, "manual", time.Unix(int64(i), 0), nil)
		if err != nil {
			t.Fatalf("persistFindings: %v", err)
		}
	}

	docs := []*web.Document{{Provider: "direct", URL: "https://example.com", Title: "Example", Text: strings.Repeat("w", webDocContextChars)}}
	got := s.buildContext(ctx, "x", docs, nil, "")
	if !strings.Contains(got, "Web sources") {
		t.Error("web block should survive budget trimming — it's inserted second, not appended last")
	}
}

// TestBuildContext_EmbeddedPromptPresent asserts the structured system
// prompt (go:embed prompt.md) is the header of every Tier 2 context —
// the one-liner was replaced by a workflow/scope/answer-discipline prompt
// (loop-engineering adaptation of LongHorizon-Harness's Manager concept).
func TestBuildContext_EmbeddedPromptPresent(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	got := s.buildContext(ctx, "x", nil, nil, "")
	if !strings.Contains(got, "You are the smith") {
		t.Errorf("buildContext missing the embedded prompt header: %s", got[:min(len(got), 200)])
	}
	if !strings.Contains(got, "Workflow") {
		t.Error("buildContext missing the Workflow section of the embedded prompt")
	}
	if !strings.Contains(got, "Verify") {
		t.Error("buildContext missing the Verify step — the loop-engineering core")
	}
}

// ── P7: the wire-compatibility freeze ────────────────────────────────────

// TestChatRequest_NoToolsByteIdenticalToP3 is the proof the deployed a0
// path is untouched when the tool loop is disabled: a Tools-nil request
// must carry exactly {"model","messages","stream"} on the wire, and no
// message may leak a tool_calls/tool_call_id/name key. Regression coverage
// for "P7 must not change P3's behavior when tools are off."
func TestChatRequest_NoToolsByteIdenticalToP3(t *testing.T) {
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	s := New(Deps{Cfg: cfgFor(portOf(t, ts.URL))})
	_, err := s.streamChatCompletion(context.Background(),
		chatRequest{Model: "test-model", Messages: []chatWireMessage{{Role: "user", Content: "hi"}}},
		func(string) {})
	if err != nil {
		t.Fatalf("streamChatCompletion: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("request body has a \"tools\" key with Tools nil: %s", gotBody)
	}
	for _, k := range []string{"model", "messages", "stream"} {
		if _, ok := got[k]; !ok {
			t.Errorf("request body missing key %q: %s", k, gotBody)
		}
	}
	if len(got) != 3 {
		t.Errorf("request body has %d top-level keys, want exactly 3 (model/messages/stream): %s", len(got), gotBody)
	}

	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(got["messages"], &messages); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	for _, m := range messages {
		for _, k := range []string{"tool_calls", "tool_call_id", "name"} {
			if _, ok := m[k]; ok {
				t.Errorf("message leaked P7 key %q: %v", k, m)
			}
		}
	}
}

// ── P7: native/fenced mode demotion ──────────────────────────────────────

// TestRunToolLoop_DemotionSignalA_ClientErrorRetriesFenced covers a native
// tools request rejected outright (HTTP 4xx while carrying tools) —
// tool_loop.go must record the model as fenced and retry the SAME round
// (not burn a round on it), then continue in fenced mode.
func TestRunToolLoop_DemotionSignalA_ClientErrorRetriesFenced(t *testing.T) {
	var reqN int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		reqN++
		if parsed["tools"] != nil {
			// This backend 400s on any request carrying "tools" — the
			// signal-A trigger.
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"unsupported parameter: tools"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, f := range contentFrames("fenced answer") {
			w.Write([]byte(f))
			flusher.Flush()
		}
	}))
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "demote-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if result.content != "fenced answer" {
		t.Errorf("content = %q, want %q", result.content, "fenced answer")
	}
	if reqN != 2 {
		t.Errorf("requests = %d, want 2 (the rejected native attempt + the retried fenced one)", reqN)
	}
	if got := s.lastToolMode("demote-model"); got != toolModeFenced {
		t.Errorf("recorded mode for demote-model = %q, want %q", got, toolModeFenced)
	}
}

// TestRunToolLoop_DemotionSignalB_FenceInContentRecordsFenced covers a
// native round that never populates tool_calls but leaks a fenced call
// into plain content (the template can't emit structured calls) — no
// wasted round, the fenced parse is used immediately and the mode is
// remembered for next time.
func TestRunToolLoop_DemotionSignalB_FenceInContentRecordsFenced(t *testing.T) {
	ts, _ := fakeA0Rounds(t, [][]string{
		contentFrames("```tool_call\n{\"name\":\"kb_search\",\"arguments\":{\"query\":\"gtt\"}}\n```"),
		contentFrames("fenced-derived answer"),
	})
	defer ts.Close()

	s, convID, msgID := setupToolLoopConv(t, ts)
	ctx := context.Background()
	batcher := s.newTokenBatcher(convID, msgID)
	tools := []Tool{mustFindTool(t, "kb_search")}

	result, err := s.runToolLoop(ctx, convID, msgID, "sys", "why?", "leaky-model", toolModeNative, tools, batcher, true, "")
	batcher.flush()
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	// The model called a fenced tool (toolsUsed=true), then answered. The
	// verify nudge fires, the auditor answers without verifying (last script
	// repeats = content only), so the unverified marker is appended.
	expected := "fenced-derived answer" + unverifiedMarker
	if result.content != expected {
		t.Errorf("content = %q, want %q", result.content, expected)
	}
	if got := s.lastToolMode("leaky-model"); got != toolModeFenced {
		t.Errorf("recorded mode for leaky-model = %q, want %q", got, toolModeFenced)
	}
}

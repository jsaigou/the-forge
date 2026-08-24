// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
)

// fakeWebService is a hand-rolled web.Service test double — no real network,
// full control over Search/Fetch outcomes per test case.
type fakeWebService struct {
	searchResults []web.Result
	searchErr     error
	searchCalls   int
	fetchDocs     map[string]*web.Document
	fetchErr      map[string]error
	providers     []web.ProviderStatus
	probeCalls    int
}

func (f *fakeWebService) Search(_ context.Context, _ string, _ int) ([]web.Result, error) {
	f.searchCalls++
	return f.searchResults, f.searchErr
}

func (f *fakeWebService) Fetch(ctx context.Context, url string) (*web.Document, error) {
	if f.fetchErr != nil {
		if err, ok := f.fetchErr[url]; ok {
			return nil, err
		}
	}
	if f.fetchDocs != nil {
		if d, ok := f.fetchDocs[url]; ok {
			return d, nil
		}
	}
	return nil, errors.New("fake: no doc configured for " + url)
}

func (f *fakeWebService) FetchWithTTL(ctx context.Context, url string, _ time.Duration) (*web.Document, error) {
	return f.Fetch(ctx, url)
}

func (f *fakeWebService) FetchDirect(ctx context.Context, url string, _ time.Duration) (*web.Document, error) {
	return f.Fetch(ctx, url)
}

func (f *fakeWebService) Providers(context.Context) []web.ProviderStatus { return f.providers }

func (f *fakeWebService) Probe(context.Context) { f.probeCalls++ }

func TestResearchForTurn_NilWeb(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	sources, docs, notice := s.researchForTurn(ctx, 0, "x")
	if sources != nil || docs != nil {
		t.Fatalf("expected nil sources/docs, got %v %v", sources, docs)
	}
	if !strings.Contains(notice, "unavailable") {
		t.Errorf("notice = %q, want it to mention unavailable", notice)
	}
}

func TestResearchForTurn_Disabled(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Web: &fakeWebService{searchErr: web.ErrDisabled}})
	_, _, notice := s.researchForTurn(context.Background(), 0, "x")
	if !strings.Contains(notice, "disabled") {
		t.Errorf("notice = %q, want it to mention disabled", notice)
	}
}

func TestResearchForTurn_SearchError(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Web: &fakeWebService{searchErr: errors.New("boom")}})
	_, _, notice := s.researchForTurn(context.Background(), 0, "x")
	if !strings.Contains(notice, "web search failed") || !strings.Contains(notice, "boom") {
		t.Errorf("notice = %q, want it to carry the underlying error", notice)
	}
}

func TestResearchForTurn_NoResults(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Web: &fakeWebService{searchResults: nil}})
	_, _, notice := s.researchForTurn(context.Background(), 0, "x")
	if !strings.Contains(notice, "no results") {
		t.Errorf("notice = %q, want it to mention no results", notice)
	}
}

func TestResearchForTurn_Success(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Now: func() time.Time { return time.Unix(5000, 0) }})
	ctx := context.Background()
	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tier := TierReasoning
	msgID, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}

	fake := &fakeWebService{
		searchResults: []web.Result{
			{Title: "A", URL: "https://a.example"},
			{Title: "B", URL: "https://b.example"},
		},
		fetchDocs: map[string]*web.Document{
			"https://a.example": {Provider: "direct", URL: "https://a.example", Title: "A", Text: "content A", FetchedAt: time.Unix(5000, 0)},
			"https://b.example": {Provider: "firecrawl", URL: "https://b.example", Title: "B", Text: "content B", FetchedAt: time.Unix(5000, 0), Cached: true},
		},
	}
	s.d.Web = fake

	sources, docs, notice := s.researchForTurn(ctx, msgID, "x")
	if notice != "" {
		t.Fatalf("unexpected notice: %q", notice)
	}
	if len(sources) != 2 || len(docs) != 2 {
		t.Fatalf("sources=%d docs=%d, want 2/2", len(sources), len(docs))
	}
	if sources[1].Cached != true {
		t.Errorf("sources[1].Cached = %v, want true (matches the fetched Document)", sources[1].Cached)
	}

	// Persisted immediately, before the caller does anything else with it.
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs[0].Sources) != 2 {
		t.Fatalf("persisted sources = %+v, want 2", msgs[0].Sources)
	}
}

func TestResearchForTurn_PartialFetchFailure(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()
	convID, _ := s.CreateConversation(ctx, "")
	tier := TierReasoning
	msgID, _ := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)

	s.d.Web = &fakeWebService{
		searchResults: []web.Result{
			{Title: "A", URL: "https://a.example"},
			{Title: "B", URL: "https://b.example"},
		},
		fetchDocs: map[string]*web.Document{
			"https://a.example": {Provider: "direct", URL: "https://a.example", Title: "A", Text: "content A"},
		},
		fetchErr: map[string]error{"https://b.example": errors.New("fetch failed")},
	}

	sources, docs, notice := s.researchForTurn(ctx, msgID, "x")
	if notice != "" {
		t.Fatalf("a partial success should not produce a notice, got %q", notice)
	}
	if len(sources) != 1 || len(docs) != 1 {
		t.Fatalf("sources=%d docs=%d, want 1/1 (one fetch failed)", len(sources), len(docs))
	}
}

func TestResearchForTurn_AllFetchFail(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()
	convID, _ := s.CreateConversation(ctx, "")
	tier := TierReasoning
	msgID, _ := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)

	s.d.Web = &fakeWebService{
		searchResults: []web.Result{{Title: "A", URL: "https://a.example"}},
		fetchErr:      map[string]error{"https://a.example": errors.New("down")},
	}

	sources, docs, notice := s.researchForTurn(ctx, msgID, "x")
	if sources != nil || docs != nil {
		t.Fatalf("expected nil sources/docs, got %v %v", sources, docs)
	}
	if !strings.Contains(notice, "fetch failed for every search result") {
		t.Errorf("notice = %q", notice)
	}
}

// ── Chat() end-to-end with web:true ─────────────────────────────────────────

func TestChat_WebTrue_DeterministicRendersSources(t *testing.T) {
	db := openDB(t)
	fake := &fakeWebService{
		searchResults: []web.Result{{Title: "llama.cpp", URL: "https://github.com/ggml-org/llama.cpp"}},
		fetchDocs: map[string]*web.Document{
			"https://github.com/ggml-org/llama.cpp": {
				Provider: "direct", URL: "https://github.com/ggml-org/llama.cpp",
				Title: "llama.cpp", Text: "LLM inference in C/C++",
			},
		},
	}
	// No Settings/Catalog wired ⇒ Brain() resolves deterministic_only, so
	// this exercises the "web:true with no brain" degrade-cleanly path.
	s := New(Deps{Store: db, Web: fake})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "what is llama.cpp?", ChatOptions{Web: true})
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
	if !strings.Contains(reply.Content, "Web search results") || !strings.Contains(reply.Content, "llama.cpp") {
		t.Errorf("deterministic reply should render the search results verbatim, got: %q", reply.Content)
	}
	if len(reply.Sources) != 1 {
		t.Fatalf("Sources = %+v, want 1 persisted source", reply.Sources)
	}
}

func TestChat_WebTrue_ReasoningTurnPersistsSourcesAndContext(t *testing.T) {
	ts := fakeA0SSE(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer ts.Close()

	db := openDB(t)
	seedBrainCatalog(t, db)
	setSetting(t, db, SettingModel, `"ornith-35b"`)
	fake := &fakeWebService{
		searchResults: []web.Result{{Title: "A", URL: "https://a.example"}},
		fetchDocs: map[string]*web.Document{
			"https://a.example": {Provider: "direct", URL: "https://a.example", Title: "A", Text: "unique-marker-xyz"},
		},
	}
	pub := &stubPublisher{}
	s := New(Deps{
		Store: db, Settings: db.Settings(), Catalog: db.Catalog(),
		Sched:     newStubSched(map[string]string{"a3": "ornith-35b"}),
		Cfg:       cfgFor(portOf(t, ts.URL)),
		Publisher: pub,
		Web:       fake,
	})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	msgID, err := s.Chat(ctx, convID, "tell me about A", ChatOptions{Web: true})
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
	if reply.Tier == nil || *reply.Tier != TierReasoning {
		t.Errorf("tier = %v, want reasoning (web:true should still escalate when a brain is reachable)", reply.Tier)
	}
	if len(reply.Sources) != 1 || reply.Sources[0].URL != "https://a.example" {
		t.Fatalf("Sources = %+v, want the one fetched document", reply.Sources)
	}
}

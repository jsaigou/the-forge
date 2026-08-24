// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"testing"
)

func TestConversations_UnwiredStore(t *testing.T) {
	s := New(Deps{})
	ctx := context.Background()
	if _, err := s.CreateConversation(ctx, "x"); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("CreateConversation err = %v, want ErrStoreUnwired", err)
	}
	if _, err := s.ListConversations(ctx); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("ListConversations err = %v, want ErrStoreUnwired", err)
	}
	if _, _, err := s.GetConversation(ctx, 1); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("GetConversation err = %v, want ErrStoreUnwired", err)
	}
	if err := s.DeleteConversation(ctx, 1); !errors.Is(err, ErrStoreUnwired) {
		t.Errorf("DeleteConversation err = %v, want ErrStoreUnwired", err)
	}
}

func TestConversations_CRUD(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	id, err := s.CreateConversation(ctx, "diagnosing a0 outage")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateConversation returned id=0")
	}

	list, err := s.ListConversations(ctx)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("ListConversations = %+v, want one row with id %d", list, id)
	}
	if list[0].Tier != TierDeterministic {
		t.Errorf("initial tier = %q, want %q", list[0].Tier, TierDeterministic)
	}

	uid, err := s.AppendUserMessage(ctx, id, "why is the box slow?")
	if err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}

	model := "qwen36-mtp"
	tier := TierReasoning
	placeholderID, err := s.appendMessage(ctx, id, MsgKindReasoning, "", nil, &model, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage (placeholder): %v", err)
	}
	tc := 42
	if err := s.finalizeMessage(ctx, placeholderID, "here is my diagnosis", &model, &tier, &tc); err != nil {
		t.Fatalf("finalizeMessage: %v", err)
	}

	_, msgs, err := s.GetConversation(ctx, id)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].ID != uid || msgs[0].Kind != MsgKindUser {
		t.Errorf("msgs[0] = %+v, want user message %d", msgs[0], uid)
	}
	got := msgs[1]
	if got.Content != "here is my diagnosis" {
		t.Errorf("finalized content = %q", got.Content)
	}
	if got.Model == nil || *got.Model != model {
		t.Errorf("model = %v, want %q", got.Model, model)
	}
	if got.TokenCount == nil || *got.TokenCount != tc {
		t.Errorf("token_count = %v, want %d", got.TokenCount, tc)
	}
	if got.Error != nil {
		t.Errorf("error = %v, want nil", got.Error)
	}

	if err := s.DeleteConversation(ctx, id); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if _, _, err := s.GetConversation(ctx, id); err == nil {
		t.Error("GetConversation after delete should error")
	}
}

func TestFailMessage_PreservesPartialContent(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tier := TierReasoning
	id, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	if err := s.failMessage(ctx, id, "partial answer before a0 timed out", "a0 request timed out"); err != nil {
		t.Fatalf("failMessage: %v", err)
	}

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].Content != "partial answer before a0 timed out" {
		t.Errorf("content = %q, partial content lost", msgs[0].Content)
	}
	if msgs[0].Error == nil || *msgs[0].Error != "a0 request timed out" {
		t.Errorf("error = %v, want set", msgs[0].Error)
	}
}

func TestMessage_SourcesDefaultsToEmptySlice(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.AppendUserMessage(ctx, convID, "hi"); err != nil {
		t.Fatalf("AppendUserMessage: %v", err)
	}
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if msgs[0].Sources == nil {
		t.Fatal("Sources should default to [] not nil")
	}
	if len(msgs[0].Sources) != 0 {
		t.Fatalf("Sources = %+v, want empty", msgs[0].Sources)
	}
}

func TestSetMessageSources_RoundTrip(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tier := TierReasoning
	id, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}

	sources := []MessageSource{
		{Provider: "searxng", URL: "https://example.com/a", Title: "A", FetchedAt: 1000},
		{Provider: "direct", URL: "https://example.com/b", Title: "B", FetchedAt: 1001, Cached: true},
	}
	if err := s.setMessageSources(ctx, id, sources); err != nil {
		t.Fatalf("setMessageSources: %v", err)
	}

	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Sources) != 2 {
		t.Fatalf("Sources = %+v, want 2 entries", msgs[0].Sources)
	}
	if msgs[0].Sources[0].Provider != "searxng" || msgs[0].Sources[1].Cached != true {
		t.Fatalf("Sources round-trip mismatch: %+v", msgs[0].Sources)
	}
}

func TestSetMessageSources_SurvivesFinalizeAndFail(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	tier := TierReasoning
	id, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	sources := []MessageSource{{Provider: "direct", URL: "https://example.com", Title: "X"}}
	if err := s.setMessageSources(ctx, id, sources); err != nil {
		t.Fatalf("setMessageSources: %v", err)
	}

	// finalizeMessage must not clobber sources written earlier in the turn.
	model := "qwen36-mtp"
	tc := 10
	if err := s.finalizeMessage(ctx, id, "answer", &model, &tier, &tc); err != nil {
		t.Fatalf("finalizeMessage: %v", err)
	}
	_, msgs, err := s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs[0].Sources) != 1 {
		t.Fatalf("finalizeMessage clobbered sources: %+v", msgs[0].Sources)
	}

	// Same for a failed turn — the whole point of writing sources mid-turn
	// rather than only at finalize.
	id2, err := s.appendMessage(ctx, convID, MsgKindReasoning, "", nil, nil, &tier, nil, nil)
	if err != nil {
		t.Fatalf("appendMessage: %v", err)
	}
	if err := s.setMessageSources(ctx, id2, sources); err != nil {
		t.Fatalf("setMessageSources: %v", err)
	}
	if err := s.failMessage(ctx, id2, "partial", "a0 timed out"); err != nil {
		t.Fatalf("failMessage: %v", err)
	}
	_, msgs, err = s.GetConversation(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(msgs[1].Sources) != 1 {
		t.Fatalf("failMessage clobbered sources: %+v", msgs[1].Sources)
	}
}

func TestSetConversationTier(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db})
	ctx := context.Background()

	id, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.setConversationTier(ctx, id, TierReasoning); err != nil {
		t.Fatalf("setConversationTier: %v", err)
	}
	list, err := s.ListConversations(ctx)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if list[0].Tier != TierReasoning {
		t.Errorf("tier = %q, want %q", list[0].Tier, TierReasoning)
	}
}

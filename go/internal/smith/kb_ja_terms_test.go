// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"sort"
	"testing"
)

// TestExpandJapaneseTerms_KnownVocabulary pins the map's behavior directly,
// independent of the corpus — a corpus edit shouldn't be able to silently
// break the term expansion itself.
func TestExpandJapaneseTerms_KnownVocabulary(t *testing.T) {
	got := expandJapaneseTerms("メモリの使用率")
	sort.Strings(got)
	want := []string{"gtt", "memory"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("expandJapaneseTerms(メモリの使用率) = %v, want %v", got, want)
	}
}

// TestExpandJapaneseTerms_NoMatch confirms a query with no recognized
// Japanese domain vocabulary degrades to nil, not an error or a panic.
func TestExpandJapaneseTerms_NoMatch(t *testing.T) {
	if got := expandJapaneseTerms("こんにちは"); got != nil {
		t.Errorf("expandJapaneseTerms(こんにちは) = %v, want nil", got)
	}
}

// TestTokenizeKBQuery_KeepsEmbeddedLatinInJapaneseText confirms a Japanese
// question that quotes an ASCII identifier verbatim (GTT, a slot ID, a
// config name) still tokenizes it — this worked before Phase 3 too, and
// must keep working now that ja expansion is layered on top additively.
func TestTokenizeKBQuery_KeepsEmbeddedLatinInJapaneseText(t *testing.T) {
	tokens := tokenizeKBQuery("GTTの上限に達しましたか？")
	found := false
	for _, tok := range tokens {
		if tok == "gtt" {
			found = true
		}
	}
	if !found {
		t.Errorf("tokenizeKBQuery didn't keep the embedded 'GTT' term: %v", tokens)
	}
}

// TestTokenizeKBQuery_JapaneseExpansionAdditive confirms the ja domain-term
// expansion adds to, rather than replaces, the ASCII-token extraction.
func TestTokenizeKBQuery_JapaneseExpansionAdditive(t *testing.T) {
	tokens := tokenizeKBQuery("スロットのメモリ状況")
	want := map[string]bool{"memory": false, "gtt": false, "slot": false}
	for _, tok := range tokens {
		if _, ok := want[tok]; ok {
			want[tok] = true
		}
	}
	for term, ok := range want {
		if !ok {
			t.Errorf("tokenizeKBQuery(スロットのメモリ状況) missing expected term %q: got %v", term, tokens)
		}
	}
}

// TestKBSearch_JapaneseGTTQuestion is the plan's own verification bar
// (multilanguage plan Phase 3): "KB retrieval is non-empty for a Japanese
// GTT/memory question." Before kb_ja_terms.go, kbTokenRe's [a-z0-9._-]+
// regex dropped every CJK character, so a pure-Japanese question about
// memory/GTT retrieved nothing at all.
func TestKBSearch_JapaneseGTTQuestion(t *testing.T) {
	s := New(Deps{})
	results, err := s.KBSearch(context.Background(), "メモリの上限について教えて", 5)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one match for a Japanese memory/GTT question")
	}
	if results[0].Ref != "pitfalls:gtt-ceiling" {
		t.Errorf("expected pitfalls:gtt-ceiling to rank first, got %q (score %.2f)", results[0].Ref, results[0].Score)
	}
}

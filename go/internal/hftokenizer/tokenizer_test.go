// SPDX-License-Identifier: Apache-2.0

//go:build compressor_integration

// Gated behind the compressor_integration build tag so a plain `go test
// ./...` never needs to link libtokenizers (see integration_test.go in
// internal/compress for the full rationale). Run with:
//
//	CGO_LDFLAGS="-L<libdir>" HFTOKENIZER_TEST_TOKENIZER_JSON=<path> \
//	go test -tags compressor_integration ./internal/hftokenizer/... -v
package hftokenizer

import (
	"os"
	"strings"
	"testing"
)

// tokenizerPath returns the real ModernBERT-base tokenizer.json to test
// against, skipping if it isn't available. This package is cgo-backed
// (needs libtokenizers linked in via CGO_LDFLAGS) and needs a real model
// artifact, so it's exercised locally / on ForgeHost, not in the default `go
// test ./...` CI run — see docs/v5-headroom-replacement.md Sprint 3's
// "Build & deploy" section.
func tokenizerPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("HFTOKENIZER_TEST_TOKENIZER_JSON")
	if p == "" {
		t.Skip("HFTOKENIZER_TEST_TOKENIZER_JSON not set; skipping real-tokenizer test")
	}
	return p
}

func TestEncodeWords_WordAlignment(t *testing.T) {
	tk, err := New(tokenizerPath(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tk.Close()

	// Mirrors the must-keep CJK bug's exact shape (docs/v5-headroom-replacement.md
	// Sprint 1/3): a number glued to CJK text with no separating whitespace,
	// plus a plain ASCII word on each side, so word boundaries are exercised
	// on both a multi-byte and a single-byte-per-rune case.
	words := strings.Fields("hello 104件 2,100円 world")
	enc, err := tk.EncodeWords(words)
	if err != nil {
		t.Fatalf("EncodeWords: %v", err)
	}

	if len(enc.IDs) == 0 {
		t.Fatal("expected at least one token")
	}
	if enc.WordIndex[0] != -1 {
		t.Errorf("expected [CLS] at position 0 to have WordIndex -1, got %d", enc.WordIndex[0])
	}
	if last := enc.WordIndex[len(enc.WordIndex)-1]; last != -1 {
		t.Errorf("expected [SEP] at the last position to have WordIndex -1, got %d", last)
	}

	// Every non-special token's WordIndex must be in range and the
	// sequence must be monotonically non-decreasing (tokens for word i all
	// precede tokens for word i+1 — this package's byte-offset
	// reconstruction depends on that).
	seenWords := map[int]bool{}
	prev := -1
	for i, wi := range enc.WordIndex {
		if wi == -1 {
			continue
		}
		if wi < 0 || wi >= len(words) {
			t.Fatalf("token %d: WordIndex %d out of range [0,%d)", i, wi, len(words))
		}
		if wi < prev {
			t.Fatalf("token %d: WordIndex %d went backwards from %d", i, wi, prev)
		}
		prev = wi
		seenWords[wi] = true
	}
	for i, w := range words {
		if !seenWords[i] {
			t.Errorf("word %d (%q) got no token mapped to it", i, w)
		}
	}

	if len(enc.AttentionMask) != len(enc.IDs) {
		t.Fatalf("AttentionMask length %d != IDs length %d", len(enc.AttentionMask), len(enc.IDs))
	}
	for i, m := range enc.AttentionMask {
		if m != 1 {
			t.Errorf("AttentionMask[%d] = %d, want 1 (this engine never pads)", i, m)
		}
	}
}

func TestEncodeWords_Empty(t *testing.T) {
	tk, err := New(tokenizerPath(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tk.Close()

	enc, err := tk.EncodeWords(nil)
	if err != nil {
		t.Fatalf("EncodeWords(nil): %v", err)
	}
	if len(enc.IDs) != 0 {
		t.Errorf("expected empty encoding for empty input, got %d ids", len(enc.IDs))
	}
}

func TestEncodeWords_Deterministic(t *testing.T) {
	tk, err := New(tokenizerPath(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer tk.Close()

	words := strings.Fields("the quick brown fox jumps over 104件 lazy dogs")
	a, err := tk.EncodeWords(words)
	if err != nil {
		t.Fatalf("EncodeWords: %v", err)
	}
	b, err := tk.EncodeWords(words)
	if err != nil {
		t.Fatalf("EncodeWords: %v", err)
	}
	if len(a.IDs) != len(b.IDs) {
		t.Fatalf("non-deterministic: id length %d vs %d", len(a.IDs), len(b.IDs))
	}
	for i := range a.IDs {
		if a.IDs[i] != b.IDs[i] || a.WordIndex[i] != b.WordIndex[i] {
			t.Fatalf("non-deterministic at token %d: (%d,%d) vs (%d,%d)", i, a.IDs[i], a.WordIndex[i], b.IDs[i], b.WordIndex[i])
		}
	}
}

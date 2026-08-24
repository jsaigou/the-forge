// SPDX-License-Identifier: Apache-2.0

//go:build compressor_integration

// Black-box (package compress_test) so it can import the real cgo-backed
// adapters (hftokenizer, onnxscorer) alongside compress without an import
// cycle — they each import compress to implement its Tokenizer/Scorer
// interfaces.
//
// Gated behind the compressor_integration build tag (rather than just
// skipping at runtime when its env vars are unset) so that a plain `go
// build ./...` / `go test ./...` at the repo root never even attempts to
// compile this file — and therefore never needs CGO_LDFLAGS pointing at
// real native libtokenizers/libonnxruntime — anywhere except a session
// that deliberately opts in. Run with:
//
//	CGO_LDFLAGS="-L<libdir>" HFTOKENIZER_TEST_TOKENIZER_JSON=<path> \
//	ONNXSCORER_TEST_MODEL=<path> ONNXSCORER_TEST_LIBONNXRUNTIME=<path> \
//	go test -tags compressor_integration ./internal/compress/... -v
package compress_test

import (
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/compress"
	"github.com/jsaigou/the-forge/internal/hftokenizer"
	"github.com/jsaigou/the-forge/internal/onnxscorer"
)

// realEngine wires the actual ModernBERT tokenizer + Kompress ONNX model
// end-to-end. Needs real model artifacts plus CGO_LDFLAGS/CGO_CFLAGS
// pointing at the native libtokenizers/libonnxruntime — not part of the
// default `go test ./...` CI run, see docs/v5-headroom-replacement.md
// Sprint 3's "Build & deploy" section. Skips if the env vars aren't set.
func realEngine(t *testing.T) *compress.Engine {
	t.Helper()
	tokPath := os.Getenv("HFTOKENIZER_TEST_TOKENIZER_JSON")
	modelPath := os.Getenv("ONNXSCORER_TEST_MODEL")
	libPath := os.Getenv("ONNXSCORER_TEST_LIBONNXRUNTIME")
	if tokPath == "" || modelPath == "" || libPath == "" {
		t.Skip("real-model env vars not set; skipping end-to-end integration test")
	}

	tok, err := hftokenizer.New(tokPath)
	if err != nil {
		t.Fatalf("hftokenizer.New: %v", err)
	}
	t.Cleanup(func() { tok.Close() })

	sc, err := onnxscorer.New(modelPath, libPath, 4)
	if err != nil {
		t.Fatalf("onnxscorer.New: %v", err)
	}
	t.Cleanup(func() { sc.Close() })

	return &compress.Engine{Tokenizer: tok, Scorer: sc, Config: compress.DefaultConfig()}
}

// TestCompress_RealModel_FactRetention mirrors the Sprint 1 bake-off's fact
// harness on a single fixture: must-keep numbers glued to CJK text (the
// exact shape of the original CJK-boundary bug, see mustkeep.go) must
// survive compression even though the surrounding prose is well over the
// byte/word threshold and gets genuinely compressed.
func TestCompress_RealModel_FactRetention(t *testing.T) {
	engine := realEngine(t)

	base := `The quarterly earnings report released on 2024年8月22日 showed that revenue increased by 104件 transactions compared to the previous quarter, generating 2,100円 in additional profit per unit sold across all regional distribution centers. Management commentary during the earnings call emphasized continued investment in supply chain resilience, noting that despite ongoing macroeconomic headwinds including elevated freight costs and currency volatility in key export markets, the company's diversified sourcing strategy allowed it to maintain gross margins within the previously guided range. Analysts on the call pressed executives about capital allocation priorities for the coming fiscal year, particularly around whether the recently announced buyback program would be expanded given the strength of free cash flow generation in the most recent period, to which the CFO responded that the board continues to evaluate all options while prioritizing organic growth investments in the company's highest-return product lines. The discussion then turned to headcount planning, where leadership reiterated that hiring would remain selective through the remainder of the fiscal year, with the bulk of new investment directed toward engineering and go-to-market roles supporting the two product lines that grew fastest last quarter, while corporate functions would see minimal net growth outside of a handful of specialized finance and legal hires needed to support the expanded regulatory footprint in newly entered markets.`
	// Repeated (with a paragraph break) so the fixture clears the default
	// 2048-byte threshold comfortably and also exercises multi-chunk
	// aggregation (this content tokenizes well past the 512-token budget
	// one chunk can hold).
	content := base + "\n\n" + base

	if len(content) < 2048 {
		t.Fatalf("test fixture is only %d bytes, must exceed the default 2048-byte threshold", len(content))
	}

	res, err := engine.Compress(content)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if res.Passthrough {
		t.Fatalf("expected real compression on a %d-byte fixture, got passthrough", len(content))
	}
	if res.CompressedWords >= res.OriginalWords {
		t.Errorf("expected compression to shrink word count: original=%d compressed=%d", res.OriginalWords, res.CompressedWords)
	}
	if res.OriginalTokens == 0 {
		t.Error("expected a real non-zero OriginalTokens count from the real tokenizer")
	}
	if res.CompressedTokens >= res.OriginalTokens {
		t.Errorf("expected compression to shrink real token count too: original=%d compressed=%d", res.OriginalTokens, res.CompressedTokens)
	}

	for _, fact := range []string{"2024年8月22日", "104件", "2,100円"} {
		if !strings.Contains(res.Compressed, fact) {
			t.Errorf("must-keep fact %q did not survive compression; output: %s", fact, res.Compressed)
		}
	}
}

// TestCompress_RealModel_Deterministic is the real-model half of the
// frozen-prefix contract (decision 5, docs/v5-headroom-replacement.md):
// the same content must compress to byte-identical output across calls,
// against the real tokenizer + ONNX model, not just the fakes.
func TestCompress_RealModel_Deterministic(t *testing.T) {
	engine := realEngine(t)

	content := strings.Repeat("The quick brown fox jumps over the lazy dog near 104件 buildings in Tokyo on 2024年8月22日. ", 10)

	a, err := engine.Compress(content)
	if err != nil {
		t.Fatalf("Compress (1st): %v", err)
	}
	b, err := engine.Compress(content)
	if err != nil {
		t.Fatalf("Compress (2nd): %v", err)
	}
	if a.Compressed != b.Compressed {
		t.Fatalf("non-deterministic output:\n1st: %s\n2nd: %s", a.Compressed, b.Compressed)
	}
}

// TestCompress_RealModel_PathologicalNoWhitespaceBlobAgainstRealModel
// reproduces the 2026-08-20 production incident (docs/v5-headroom-replacement.md
// Sprint 9) against the REAL ModernBERT tokenizer and Kompress ONNX model,
// not just fakes: a single unbroken run of non-whitespace content (a
// base64/JSON-blob shape, no spaces for chunkWords to cut on) large enough
// to tokenize into many thousands of tokens, embedded in otherwise-ordinary
// content. Before the scoreChunk fix, this sent an unbounded sequence
// straight into ModernBERT's O(n^2) self-attention and briefly exhausted
// 123GB of RAM on the real production host. This test asserts it now
// completes quickly and successfully — the surrounding 30s test timeout is
// the actual safety assertion (a real attention blow-up on this input size
// would not finish in anything close to 30s even if it didn't OOM first).
func TestCompress_RealModel_PathologicalNoWhitespaceBlobAgainstRealModel(t *testing.T) {
	engine := realEngine(t)

	// A real base64 alphabet blob (not a degenerate repeated character BPE
	// could collapse into a handful of tokens) big enough to tokenize into
	// several thousand tokens — comfortably past maxChunkTokens (512) many
	// times over, matching deepseek's real ~262K-token-average requests.
	rng := rand.New(rand.NewSource(1))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	blob := make([]byte, 40000)
	for i := range blob {
		blob[i] = alphabet[rng.Intn(len(alphabet))]
	}

	content := "Here is a large embedded payload with no internal whitespace: " + string(blob) + " — please summarize the request above."

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := engine.Compress(content)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Compress: %v", err)
		}
		t.Logf("completed in %s", time.Since(start))
	case <-time.After(30 * time.Second):
		t.Fatal("Compress did not return within 30s — this is the exact shape of the 2026-08-20 OOM incident, not a slow-but-safe pass")
	}
}

// TestCompress_RealModel_BelowThreshold confirms short content still
// passes through untouched with the real engine wired in, not just fakes.
func TestCompress_RealModel_BelowThreshold(t *testing.T) {
	engine := realEngine(t)

	content := "short content well under the byte threshold"
	res, err := engine.Compress(content)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if !res.Passthrough {
		t.Errorf("expected passthrough for %d-byte content under the %d-byte threshold", len(content), engine.Config.ByteThreshold)
	}
	if res.Compressed != content {
		t.Errorf("passthrough must return content unchanged; got %q", res.Compressed)
	}
}

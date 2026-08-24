// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// completionTimings is the per-request timing object llama-server returns in
// the /v1/completions response body. The rates (prompt_per_second,
// predicted_per_second) are per-request, not running averages — the canonical
// source for a reproducible T/s figure.
type completionTimings struct {
	PromptN            int     `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PredictedN         int     `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
}

type completionsResponse struct {
	Timings completionTimings `json:"timings"`
}

// completions sends a POST /v1/completions to the llama-server on port and
// returns the response timings. The prompt fills the KV cache; maxTokens
// controls the generation length. temperature=0 for determinism.
//
// Uses 127.0.0.1 (not "localhost") to avoid the ::1-first resolution stall
// (docs/pitfalls.md "curl localhost vs 127.0.0.1").
func (r *Runner) completions(ctx context.Context, port int, prompt string, maxTokens int) (completionTimings, error) {
	body := map[string]any{
		"prompt":      prompt,
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return completionTimings{}, fmt.Errorf("completions: marshal: %w", err)
	}

	url := r.baseURL(port) + "/v1/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return completionTimings{}, fmt.Errorf("completions: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return completionTimings{}, fmt.Errorf("completions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return completionTimings{}, fmt.Errorf("completions: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return completionTimings{}, fmt.Errorf("completions: read: %w", err)
	}

	var out completionsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return completionTimings{}, fmt.Errorf("completions: parse: %w", err)
	}
	return out.Timings, nil
}

// tokenizeCount uses llama-server's POST /tokenize endpoint to count the
// exact number of tokens in text. This lets us size the fill to ~95% of n_ctx
// without guessing a char-to-token ratio (which varies wildly between prose
// and code). Returns -1 if the endpoint is unavailable (older builds).
func (r *Runner) tokenizeCount(ctx context.Context, port int, text string) (int, error) {
	body := map[string]any{"content": text}
	raw, err := json.Marshal(body)
	if err != nil {
		return -1, fmt.Errorf("tokenize: marshal: %w", err)
	}

	url := r.baseURL(port) + "/tokenize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return -1, fmt.Errorf("tokenize: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return -1, fmt.Errorf("tokenize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, fmt.Errorf("tokenize: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return -1, fmt.Errorf("tokenize: read: %w", err)
	}

	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return -1, fmt.Errorf("tokenize: parse: %w", err)
	}
	return len(out.Tokens), nil
}

// sizeFillMaxIterations bounds the trim-and-reverify loop in sizeFill. Each
// iteration re-tokenizes only the current candidate (cheap relative to the
// inference this fill feeds into), and the safety factor per iteration
// converges fast — this is a backstop against a pathological corpus, not a
// budget expected to be exhausted in practice.
const sizeFillMaxIterations = 6

// sizeFill generates a heterogeneous corpus and trims it to approximately
// targetTokens using /tokenize for an exact count. Falls back to a 3.5
// chars/token heuristic if /tokenize is unavailable.
//
// A single average-density proportional cut is not safe: it assumes the
// corpus's token density is uniform, but the bundled prose/code/JSON/table
// paragraphs are not evenly distributed through a shuffled, repeated corpus.
// Found live profiling nemotron-puzzle at its full 1,048,576-token context —
// a one-shot cut sized from the whole corpus's average density produced a
// fill measuring 1,086,904 tokens (over both the target and the actual
// context size), because the retained prefix happened to be locally denser
// than the corpus-wide average. sizeFill now re-tokenizes the actual
// candidate slice after each cut and iterates until it's verified to fit,
// rather than trusting the first estimate.
func (r *Runner) sizeFill(ctx context.Context, port, targetTokens int) (string, error) {
	return r.sizeFillSeeded(ctx, port, targetTokens, 42)
}

// sizeFillSeeded is sizeFill parameterized by seed — see
// generateFillSeeded's doc comment for why the depth sweep needs this.
func (r *Runner) sizeFillSeeded(ctx context.Context, port, targetTokens int, seed int64) (string, error) {
	// Generate a large corpus — much more than needed.
	corpus := generateFillSeeded(targetTokens*2, seed)

	tokenizeSized := func(text string) (int, error) {
		tctx, cancel := context.WithTimeout(ctx, r.completionTimeout(targetTokens*2))
		defer cancel()
		return r.tokenizeCount(tctx, port, text)
	}
	charHeuristic := func(text string) string {
		targetChars := int(float64(targetTokens) * 3.5)
		if targetChars > len(text) {
			targetChars = len(text)
		}
		return text[:targetChars]
	}

	count, err := tokenizeSized(corpus)
	if err != nil || count <= 0 {
		// Fallback: heuristic (3.5 chars/token, conservative for code/JSON).
		r.logf("profile: /tokenize unavailable (%v), using char heuristic", err)
		return charHeuristic(corpus), nil
	}
	if count <= targetTokens {
		// Corpus is already small enough.
		return corpus, nil
	}

	candidate := corpus
	for i := 0; i < sizeFillMaxIterations; i++ {
		// Truncate proportionally against the CURRENT candidate's own
		// measured count (not the original corpus's), slightly under to
		// leave room for generation tokens and per-iteration safety margin.
		ratio := float64(targetTokens) / float64(count) * 0.95
		targetChars := int(float64(len(candidate)) * ratio)
		if targetChars < 100 {
			targetChars = 100
		}
		if targetChars >= len(candidate) {
			targetChars = len(candidate) - 1
		}
		candidate = candidate[:targetChars]

		count, err = tokenizeSized(candidate)
		if err != nil || count <= 0 {
			r.logf("profile: /tokenize unavailable mid-trim (%v), using char heuristic", err)
			return charHeuristic(candidate), nil
		}
		if count <= targetTokens {
			return candidate, nil
		}
	}
	r.logf("profile: sizeFill did not converge under %d tokens after %d iterations (last measured %d) — using best effort",
		targetTokens, sizeFillMaxIterations, count)
	return candidate, nil
}

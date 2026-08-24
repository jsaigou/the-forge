// SPDX-License-Identifier: Apache-2.0

package router

// usage.go — real remote API spend recording (cost/savings sprint Phase 4,
// 2026-07-30). Watches successful "remote"-backend responses as they stream
// past (never buffering a streaming body — see usageTap's doc comment,
// verified against TestChatCompletions_StreamingNoBuffering), parses the
// provider's own usage object, and records one kind="external_request"
// store.UsageEvent per completed request.
//
// Two spend sources exist and must never be blended: this file answers
// "what did Forge's own traffic cost, by model/day" from usage_events;
// internal/providers' credit-balance polling answers "what has actually
// been billed" from the provider's own ledger. The read side (httpapi) is
// responsible for showing both plus their delta — this file only writes.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// settingInjectStreamUsage is a router-internal behavior toggle, not a
// Contract-3 vocabulary key — mirrors compressor.passthrough_all/
// router.busy_mode (ungated Settings KV, read ad-hoc), NOT
// registeredSettingKeys (internal/httpapi/settings_handlers.go), which has
// its own exact-membership test.
const settingInjectStreamUsage = "usage.inject_stream_usage"

// injectStreamUsageEnabled reads usage.inject_stream_usage, defaulting to
// true when unset (unlike passthroughAll/busyMode's "unset → zero value"
// defaults, this one must default true, so the unset/error path is explicit
// rather than falling through to a bool zero value).
func (s *Server) injectStreamUsageEnabled(ctx context.Context) bool {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, settingInjectStreamUsage)
		if err == nil {
			var v bool
			if json.Unmarshal(raw, &v) == nil {
				return v
			}
		}
	}
	return true
}

// applyStreamUsageOptions injects {"include_usage": true} into
// body["stream_options"] when the request is streaming, the caller didn't
// already supply stream_options (respecting an explicit client choice), and
// the operator hasn't disabled injection. Without this, most
// OpenAI-compatible streaming responses omit the trailing usage chunk
// entirely and remote spend tracking is structurally blind to streamed
// requests — the majority of real agent traffic.
func applyStreamUsageOptions(body map[string]any, enabled bool) map[string]any {
	if !enabled {
		return body
	}
	streaming, _ := body["stream"].(bool)
	if !streaming {
		return body
	}
	if _, has := body["stream_options"]; has {
		return body // respect the client's own choice
	}
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out["stream_options"] = map[string]any{"include_usage": true}
	return out
}

// usageTap wraps a response body, watching bytes as the proxy's own copy
// loop reads them through untouched, and invoking onClose exactly once
// (from Close, after the proxy has finished reading — i.e. after the client
// already has the full response) with whatever was accumulated.
//
// It NEVER buffers a streaming response in full: for streaming (SSE) bodies
// it keeps only a small rolling tail (maxBuf, re-sliced on every chunk),
// bounded regardless of total response length — a long-running generation
// must stream to the client exactly as fast as the upstream sends it, with
// zero added buffering delay. This is the load-bearing property
// TestChatCompletions_StreamingNoBuffering guards; do not add an
// io.ReadAll/replace-body step here. Non-streaming bodies are a single,
// bounded JSON object (never large), so accumulating up to maxBuf and
// giving up cleanly beyond that is safe.
type usageTap struct {
	rc        io.ReadCloser
	streaming bool
	buf       []byte
	maxBuf    int
	onClose   func(buf []byte)
	closed    bool
}

const (
	nonStreamingUsageBufCap = 1 << 20  // 1 MiB — a chat completion body is never this large
	streamingUsageTailCap   = 64 << 10 // 64 KiB rolling tail — enough for the trailing usage chunk
)

func newUsageTap(rc io.ReadCloser, streaming bool, onClose func([]byte)) *usageTap {
	max := nonStreamingUsageBufCap
	if streaming {
		max = streamingUsageTailCap
	}
	return &usageTap{rc: rc, streaming: streaming, maxBuf: max, onClose: onClose}
}

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.accumulate(p[:n])
	}
	return n, err
}

func (t *usageTap) accumulate(chunk []byte) {
	if t.streaming {
		t.buf = append(t.buf, chunk...)
		if len(t.buf) > t.maxBuf {
			t.buf = append([]byte(nil), t.buf[len(t.buf)-t.maxBuf:]...)
		}
		return
	}
	if len(t.buf) >= t.maxBuf {
		return
	}
	room := t.maxBuf - len(t.buf)
	if len(chunk) > room {
		chunk = chunk[:room]
	}
	t.buf = append(t.buf, chunk...)
}

func (t *usageTap) Close() error {
	err := t.rc.Close()
	if !t.closed {
		t.closed = true
		if t.onClose != nil {
			t.onClose(t.buf)
		}
	}
	return err
}

// usageParse is the parsed shape of an OpenAI-compatible "usage" object,
// including the two real-world cache-hit field names in use (OpenAI's
// nested prompt_tokens_details.cached_tokens and DeepSeek's flat
// prompt_cache_hit_tokens) — never a third, invented field name.
type usageParse struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

func (u usageParse) cachedTokens() int64 {
	if u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	return u.PromptCacheHitTokens
}

// parseNonStreamingUsage parses a plain JSON completion body's top-level
// "usage" object. ok=false when no usage object (or an all-zero one, which
// is indistinguishable from "absent" and must not be recorded as real data).
func parseNonStreamingUsage(buf []byte) (prompt, completion, cached int64, ok bool) {
	var body struct {
		Usage usageParse `json:"usage"`
	}
	if err := json.Unmarshal(buf, &body); err != nil {
		return 0, 0, 0, false
	}
	if body.Usage.PromptTokens == 0 && body.Usage.CompletionTokens == 0 {
		return 0, 0, 0, false
	}
	return body.Usage.PromptTokens, body.Usage.CompletionTokens, body.Usage.cachedTokens(), true
}

// parseStreamingUsage scans a rolling SSE tail backwards for the last
// "data: {...}" frame carrying a usage object — the trailing chunk
// stream_options.include_usage causes OpenAI-compatible APIs to emit just
// before "data: [DONE]". Scanning backwards from the tail means a truncated
// leading partial line (from the rolling-buffer trim) is never mistaken for
// a real frame — the real usage chunk, if present, is always intact near
// the end since it's the last thing written.
func parseStreamingUsage(tail []byte) (prompt, completion, cached int64, ok bool) {
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if p, c, ch, ok := parseNonStreamingUsage(payload); ok {
			return p, c, ch, true
		}
	}
	return 0, 0, 0, false
}

// computeCostNative prices promptTokens/completionTokens/cachedTokens
// against resolved's per-1M rates. ok=false ("" PriceCurrency) means no
// offering matched this request — the caller must not fabricate a cost.
// When the provider discounts cache hits but PriceCachedInPer1M is nil
// (unmodelled), cached tokens are priced at the full input rate — a
// documented upper bound, never an under-estimate.
func computeCostNative(resolved ResolvedBackend, promptTokens, completionTokens, cachedTokens int64) (cost float64, ok bool) {
	if resolved.PriceCurrency == "" {
		return 0, false
	}
	billableIn := promptTokens
	if cachedTokens > 0 && resolved.PriceCachedInPer1M != nil {
		billableIn = promptTokens - cachedTokens
		if billableIn < 0 {
			billableIn = 0
		}
		cost += float64(cachedTokens) / 1e6 * *resolved.PriceCachedInPer1M
	}
	cost += float64(billableIn) / 1e6 * resolved.PriceInPer1M
	cost += float64(completionTokens) / 1e6 * resolved.PriceOutPer1M
	return cost, true
}

// recordExternalUsage builds and persists one kind="external_request"
// UsageEvent from a completed remote response. Called from usageTap.onClose
// (after the client has already received the full response) — never on the
// request-serving path, so a slow/failed store write can't add latency or
// fail the user's request.
// nonZeroInt64Ptr maps 0 -> nil (ResolvedBackend.ProviderID uses 0 as "no
// provider", matching the old ""-string convention Provider used).
func nonZeroInt64Ptr(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func (s *Server) recordExternalUsage(resolved ResolvedBackend, model string, buf []byte, streaming bool) {
	if s.deps.Usage == nil {
		return
	}
	var prompt, completion, cached int64
	var ok bool
	if streaming {
		prompt, completion, cached, ok = parseStreamingUsage(buf)
	} else {
		prompt, completion, cached, ok = parseNonStreamingUsage(buf)
	}

	ev := store.UsageEvent{
		TS: time.Now(), Kind: "external_request",
		Model: resolved.WireModel, ProviderID: nonZeroInt64Ptr(resolved.ProviderID),
	}
	if !ok {
		// Response completed but carried no parseable usage object — record
		// the fact that a request happened without fabricating token counts.
		ev.Unmetered = true
	} else {
		ev.PromptTokens = prompt
		ev.CompletionTokens = completion
		if cached > 0 {
			ev.CachedPromptTokens = &cached
		}
		if cost, ok := computeCostNative(resolved, prompt, completion, cached); ok {
			ev.CostNative = &cost
			ev.CostCurrency = resolved.PriceCurrency
		}
	}

	// Best-effort: never block or fail the (already-completed) request over
	// a usage-recording error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.deps.Usage.Record(ctx, ev); err != nil {
		log.Printf("router: external usage record: %v", err)
	}
}

// SPDX-License-Identifier: Apache-2.0

package ctxledger

import (
	"bytes"
	"encoding/json"
)

// Names of the reuse parsers (enginekind.Spec.Ledger).
const (
	ParserLlamaCppTimings = "llamacpp-timings"
	ParserOpenAICached    = "openai-cached-tokens"
)

// ReuseParser reads one response and reports what the ticket's Reuse takes: tokens served from cache and tokens newly computed. ok is false when
// the response does not say (unmetered), which is not the same as zero reuse.
type ReuseParser func(buf []byte, streaming bool) (cached, computed int64, ok bool)

// ParserFor returns the parser a kind's Spec.Ledger names. ok is false for a name no parser implements, so the caller can log it instead of
// silently measuring with the wrong dialect.
func ParserFor(name string) (ReuseParser, bool) {
	switch name {
	case ParserLlamaCppTimings:
		return ParseTimings, true
	case ParserOpenAICached:
		return ParseCachedTokens, true
	}
	return nil, false
}

// ParseCachedTokens reads the OpenAI usage object: usage.prompt_tokens_details.cached_tokens and usage.prompt_tokens, from the whole JSON body
// (non-stream) or the last SSE frame carrying a usage object (stream). It returns (cached, prompt-cached), the same pair ParseTimings returns for
// llama.cpp, so Ticket.Reuse(cached, computed) means the same thing for both.
//
// Streaming responses carry usage only when the request asked for it (stream_options.include_usage). A response without a usage object, with a
// null one, without prompt_tokens, or whose cached count is negative or larger than the prompt is reported ok=false: unmetered, never zero reuse.
// A usage object with prompt_tokens but no prompt_tokens_details is also unmetered (the engine did not say how much was cached).
//
// Engines that cache in fixed blocks (Radiance) report only whole blocks as cached, so the ratio reads lower than llama.cpp's exact token reuse.
func ParseCachedTokens(buf []byte, streaming bool) (cached, computed int64, ok bool) {
	if !bytes.Contains(buf, []byte(`"usage"`)) {
		return 0, 0, false
	}
	if !streaming {
		return cachedFrom(buf)
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !bytes.Contains(payload, []byte(`"usage"`)) {
			continue
		}
		if c, p, ok := cachedFrom(payload); ok {
			return c, p, true
		}
	}
	return 0, 0, false
}

func cachedFrom(b []byte) (cached, computed int64, ok bool) {
	var v struct {
		Usage *struct {
			PromptTokens *int64 `json:"prompt_tokens"`
			Details      *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Usage == nil ||
		v.Usage.PromptTokens == nil || v.Usage.Details == nil || v.Usage.Details.CachedTokens == nil {
		return 0, 0, false
	}
	p, c := *v.Usage.PromptTokens, *v.Usage.Details.CachedTokens
	if p < 0 || c < 0 || c > p {
		return 0, 0, false
	}
	return c, p - c, true
}

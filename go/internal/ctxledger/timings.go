// SPDX-License-Identifier: Apache-2.0

package ctxledger

import (
	"bytes"
	"encoding/json"
)

// ParseTimings extracts llama.cpp's timings.cache_n / timings.prompt_n from a
// local-slot chat-completion response. buf is what the router's usageTap
// already accumulates: the whole (bounded) JSON body for non-stream
// responses, or a rolling tail of the SSE stream — in which case the last
// "data:" frame carrying a "timings" object wins (llama-server puts it on the
// final chunk). Nothing is buffered here; ok=false when absent or malformed.
func ParseTimings(buf []byte, streaming bool) (cacheN, promptN int64, ok bool) {
	if !bytes.Contains(buf, []byte(`"timings"`)) {
		return 0, 0, false
	}
	if !streaming {
		return timingsFrom(buf)
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) ||
			!bytes.Contains(payload, []byte(`"timings"`)) {
			continue
		}
		if c, p, ok := timingsFrom(payload); ok {
			return c, p, true
		}
	}
	return 0, 0, false
}

func timingsFrom(b []byte) (cacheN, promptN int64, ok bool) {
	var v struct {
		Timings *struct {
			CacheN  *int64 `json:"cache_n"`
			PromptN *int64 `json:"prompt_n"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Timings == nil ||
		v.Timings.CacheN == nil || v.Timings.PromptN == nil {
		return 0, 0, false
	}
	return *v.Timings.CacheN, *v.Timings.PromptN, true
}

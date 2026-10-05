// SPDX-License-Identifier: Apache-2.0

package ctxledger

// analyze.go — pure, content-blind measurement of one inbound chat body.
// Everything returned is a count (chars, messages, tools) or a normalized
// tool name; no function here returns, stores or logs any message text.

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/jsaigou/the-forge/internal/store"
)

// Thresholds for "big result" counters, in characters (C5's decimal budget).
const (
	Big8kChars  = 8000
	Big24kChars = 24000
)

// ToolStat is one tool's share of the new tail.
type ToolStat struct {
	Results int64 // number of new outputs
	Chars   int64
	Big8k   int64
	Big24k  int64
	Max     int64
	Hist    store.Hist
}

// Created is the per-request creation measurement over new messages only.
type Created struct {
	User, AssistantText, AssistantReasoning, ToolArgs, Other int64
	Tools                                                    map[string]*ToolStat
	MaxResult                                                int64
	MaxResultTool                                            string
}

// parsedBody is the minimal shape needed from the inbound JSON.
type parsedBody struct {
	Messages []any
	Tools    json.RawMessage
}

func parseBody(body []byte) (parsedBody, bool) {
	var v struct {
		Messages []any           `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Messages) == 0 {
		return parsedBody{}, false
	}
	return parsedBody{Messages: v.Messages, Tools: v.Tools}, true
}

// schemaStats returns len(json(tools)) and tools[] length (0,0 when absent).
func schemaStats(raw json.RawMessage) (chars int64, count int) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return int64(len(raw)), 0
	}
	return int64(len(raw)), len(arr)
}

// textChars counts the text characters of a message `content` value (string
// or an array of parts; only parts' "text" fields count).
func textChars(content any) int64 {
	switch c := content.(type) {
	case string:
		return int64(utf8.RuneCountInString(c))
	case []any:
		var n int64
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					n += int64(utf8.RuneCountInString(t))
				}
			}
		}
		return n
	}
	return 0
}

func strChars(v any) int64 {
	if s, ok := v.(string); ok {
		return int64(utf8.RuneCountInString(s))
	}
	return 0
}

// hashMessages returns sha256 of each message's canonical JSON (map keys
// sorted by encoding/json). Only digests leave this function.
func hashMessages(msgs []any) [][32]byte {
	out := make([][32]byte, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			b = []byte{0}
		}
		out[i] = sha256.Sum256(b)
	}
	return out
}

func roleOf(m any) string {
	if mm, ok := m.(map[string]any); ok {
		r, _ := mm["role"].(string)
		return r
	}
	return ""
}

func isSystemRole(r string) bool { return r == "system" || r == "developer" }

// measure computes Created over msgs[from:] (names resolved against the full
// slice) and the total chars of the whole slice.
func measure(msgs []any, from int) (c Created, total int64) {
	c.Tools = map[string]*ToolStat{}
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role, _ := mm["role"].(string)
		chars := textChars(mm["content"])
		var args, reasoning int64
		if role == "assistant" {
			reasoning = strChars(mm["reasoning_content"]) + strChars(mm["reasoning"])
			if calls, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range calls {
					if tcm, ok := tc.(map[string]any); ok {
						if fn, ok := tcm["function"].(map[string]any); ok {
							args += strChars(fn["arguments"])
						}
					}
				}
			}
		}
		total += chars + args + reasoning
		if i < from {
			continue
		}
		switch role {
		case "user":
			c.User += chars
		case "assistant":
			c.AssistantText += chars
			c.AssistantReasoning += reasoning
			c.ToolArgs += args
		case "tool", "function":
			name, ok := resolveTool(msgs, i)
			if ok {
				name = normalizeTool(name)
			}
			if !ok || name == "" {
				name = "unknown"
			}
			ts := c.Tools[name]
			if ts == nil {
				ts = &ToolStat{Hist: store.Hist{}}
				c.Tools[name] = ts
			}
			ts.Results++
			ts.Chars += chars
			ts.Hist[SizeBucket(chars)]++
			if chars > ts.Max {
				ts.Max = chars
			}
			if chars > Big8kChars {
				ts.Big8k++
			}
			if chars > Big24kChars {
				ts.Big24k++
			}
			if chars > c.MaxResult {
				c.MaxResult, c.MaxResultTool = chars, name
			}
		default:
			c.Other += chars
		}
	}
	return c, total
}

// SizeBucket maps a char count to a log-scale bucket (4 per octave, ~19%
// wide); 0 chars -> bucket 0.
func SizeBucket(chars int64) int {
	if chars <= 0 {
		return 0
	}
	return int(4*math.Log2(float64(chars))) + 1
}

// SizeBucketValue is the representative char count for a bucket.
func SizeBucketValue(b int) float64 {
	if b <= 0 {
		return 0
	}
	return math.Pow(2, (float64(b-1)+0.5)/4)
}

// ReuseBucket maps a 0..1 reuse ratio to one of 20 5%-wide buckets.
func ReuseBucket(r float64) int {
	b := int(r * 20)
	if b < 0 {
		b = 0
	}
	if b > 19 {
		b = 19
	}
	return b
}

// ReuseBucketValue is the bucket midpoint.
func ReuseBucketValue(b int) float64 { return (float64(b) + 0.5) / 20 }

// Percentile returns the p-quantile (0..1) of a sparse histogram, using val to
// map a bucket to a number; ok=false when empty.
func Percentile(h store.Hist, p float64, val func(int) float64) (float64, bool) {
	var total int64
	keys := make([]int, 0, len(h))
	for k, v := range h {
		if v > 0 {
			total += v
			keys = append(keys, k)
		}
	}
	if total == 0 {
		return 0, false
	}
	sort.Ints(keys)
	target := int64(math.Ceil(p * float64(total)))
	if target < 1 {
		target = 1
	}
	var acc int64
	for _, k := range keys {
		acc += h[k]
		if acc >= target {
			return val(k), true
		}
	}
	return val(keys[len(keys)-1]), true
}

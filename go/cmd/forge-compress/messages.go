// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/compress"
)

// compressMessages walks body["messages"] (the standard OpenAI chat
// request shape) and compresses each string "content" field in place.
// Non-string content (multimodal content-part arrays, missing fields, a
// malformed body) is left completely untouched — compression is a
// best-effort optimization, never a requirement for the request to
// succeed. Returns the real ModernBERT-tokenizer token counts summed
// across every message actually run through the engine (0 for either if
// nothing in the request was tokenizable/compressible), for the caller to
// fold into the tokens_saved metric.
func compressMessages(engine *compress.Engine, body map[string]any, budget time.Duration) (originalTokens, compressedTokens int64, failOpenTimeout, failOpenError int64) {
	messagesRaw, ok := body["messages"].([]any)
	if !ok {
		return 0, 0, 0, 0
	}
	for _, mRaw := range messagesRaw {
		msg, ok := mRaw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			continue
		}
		res, reason := compressOne(engine, content, budget)
		msg["content"] = res.Compressed
		originalTokens += int64(res.OriginalTokens)
		compressedTokens += int64(res.CompressedTokens)
		switch reason {
		case "timeout":
			failOpenTimeout++
		case "error":
			failOpenError++
		}
	}
	return originalTokens, compressedTokens, failOpenTimeout, failOpenError
}

// compressOne runs engine.Compress with a wall-clock fail-open budget and
// panic recovery, so a compressor bug or a stuck native call never turns
// into a 500 for a real chat request. This is safe specifically because
// Compress is a pure function of content with no side effects (decision 5,
// the frozen-prefix contract — see compress.Engine's doc comment): an
// abandoned/slow call left running past the budget can't corrupt any
// shared state, and its eventual result (if any) is simply discarded when
// it lands on resultCh after nothing is left listening.
func compressOne(engine *compress.Engine, content string, budget time.Duration) (compress.Result, string) {
	type outcome struct {
		res compress.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- outcome{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		res, err := engine.Compress(content)
		ch <- outcome{res: res, err: err}
	}()

	select {
	case out := <-ch:
		if out.err != nil {
			return failOpenResult(content), "error"
		}
		return out.res, ""
	case <-time.After(budget):
		return failOpenResult(content), "timeout"
	}
}

// failOpenResult mirrors compress.passthroughResult's shape (unexported in
// that package, so reproduced here): content unchanged, no real token
// counts reported — a fail-open call never claims a savings figure it
// didn't actually measure.
func failOpenResult(content string) compress.Result {
	n := len(strings.Fields(content))
	return compress.Result{Compressed: content, OriginalWords: n, CompressedWords: n, Passthrough: true}
}

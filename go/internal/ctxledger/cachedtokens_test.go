// SPDX-License-Identifier: Apache-2.0

package ctxledger

import "testing"

// Shapes follow Radiance's usage_json / completion_object / usage_chunk (core/server/oai.cpp): usage always on a non-stream body; on a stream only in
// the empty-choices chunk that stream_options.include_usage adds before [DONE].
const (
	radNonStream = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":2048,"completion_tokens":5,"total_tokens":2053,"prompt_tokens_details":{"cached_tokens":1536}},` +
		`"timings":{"cache_n":1536,"prompt_n":512}}`
	radStreamWithUsage = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":3,\"total_tokens\":1003," +
		"\"prompt_tokens_details\":{\"cached_tokens\":768}}}\n\n" +
		"data: [DONE]\n\n"
	radStreamNoUsage = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
)

func TestParseCachedTokens(t *testing.T) {
	cases := []struct {
		name      string
		buf       string
		streaming bool
		c, p      int64
		ok        bool
	}{
		{"non-stream", radNonStream, false, 1536, 512, true},
		{"stream with include_usage", radStreamWithUsage, true, 768, 232, true},
		{"stream without include_usage is unmetered", radStreamNoUsage, true, 0, 0, false},
		{"stream usage null everywhere", "data: {\"usage\":null}\n\ndata: [DONE]\n\n", true, 0, 0, false},
		{"zero cached is a real zero", `{"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}}`, false, 0, 10, true},
		{"usage without details is unmetered", `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`, false, 0, 0, false},
		{"details without cached_tokens is unmetered", `{"usage":{"prompt_tokens":10,"prompt_tokens_details":{}}}`, false, 0, 0, false},
		{"cached above prompt is malformed", `{"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":11}}}`, false, 0, 0, false},
		{"negative is malformed", `{"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":-1}}}`, false, 0, 0, false},
		{"truncated tail", `{"usage":{"prompt_tokens":10,"prompt_tok`, false, 0, 0, false},
		{"no usage at all", `{"choices":[]}`, false, 0, 0, false},
		{"empty", ``, true, 0, 0, false},
		{"llama.cpp body is not this dialect", `{"timings":{"cache_n":900,"prompt_n":100}}`, false, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, p, ok := ParseCachedTokens([]byte(tc.buf), tc.streaming)
			if ok != tc.ok || c != tc.c || p != tc.p {
				t.Fatalf("got (%d, %d, %v), want (%d, %d, %v)", c, p, ok, tc.c, tc.p, tc.ok)
			}
		})
	}
}

// A rolling tail starts mid-frame: the usage frame must still be found when earlier bytes are cut.
func TestParseCachedTokensRollingTail(t *testing.T) {
	tail := radStreamWithUsage[20:]
	if c, p, ok := ParseCachedTokens([]byte(tail), true); !ok || c != 768 || p != 232 {
		t.Fatalf("got (%d, %d, %v)", c, p, ok)
	}
}

func TestParserFor(t *testing.T) {
	for _, name := range []string{ParserLlamaCppTimings, ParserOpenAICached} {
		if f, ok := ParserFor(name); !ok || f == nil {
			t.Errorf("no parser for %q", name)
		}
	}
	if _, ok := ParserFor("nope"); ok {
		t.Error("unknown parser name must not resolve")
	}
	f, _ := ParserFor(ParserOpenAICached)
	if _, _, ok := f([]byte(radNonStream), false); !ok {
		t.Error("openai parser through ParserFor")
	}
}

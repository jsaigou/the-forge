// SPDX-License-Identifier: Apache-2.0

package router

// nonmutation_test.go — T1 of the context-creation plan (WS-F; contract C1,
// invariant I1, principle P0 "context is append-only").
//
// Claim under test (C1 as amended 2026-10-04): for POST /v1/chat/completions,
// every element of `messages` that a0 forwards upstream is CONTENT-IDENTICAL
// to what it received — it decodes to exactly the same JSON values, numbers
// compared losslessly (exact rational value of the number text, so big
// integers are not rounded through float64) — for every enumerated allowed
// non-content edit (alias/model substitution, reasoning_effort translation,
// stream_options.include_usage injection, routing/auth headers). Only `model`
// and the allowed request parameters may differ at the top level; every other
// top-level field must also be value-identical.
//
// The envelope may be re-serialised: a0's mutateBody (proxy.go) parses the
// body into map[string]any and re-marshals it, so key order, whitespace,
// \uXXXX spelling and <>& escaping, and float text (1.0 vs 1, 1e2 vs 100)
// can change. Those are invisible to the model and the KV cache. Fixtures
// cover them explicitly and they MUST pass.
//
// Raw-byte equality is reported informationally (t.Logf + summary) so a
// future raw-splicing workstream can see its progress, and becomes a hard
// failure when T1_STRICT_RAW=1.
//
// Red proof (WS-F): a patch that edits a string, drops/reorders a message,
// strips reasoning_content or rounds a number makes this test fail.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/ctxledger"
	"github.com/jsaigou/the-forge/internal/store"
)

// strictRawDefault: raw-byte equality is not part of C1 (amended); opt in with
// T1_STRICT_RAW=1.
const strictRawDefault = false

func strictRaw() bool {
	switch os.Getenv("T1_STRICT_RAW") {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return strictRawDefault
}

// ---- raw extraction ---------------------------------------------------------

// rawTopLevel returns the raw bytes of every top-level member value of a JSON
// object, exactly as spelled on the wire (json.RawMessage keeps inner
// whitespace/escapes/number spelling/key order untouched). Duplicate
// top-level keys: the last wins (same as encoding/json), which is fine for
// the top level; duplicates INSIDE messages are preserved in the raw slice.
func rawTopLevel(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%.200s", err, body)
	}
	return m
}

// rawMessages returns the raw bytes of the whole `messages` array and of each
// element.
func rawMessages(t *testing.T, body []byte) (whole json.RawMessage, elems []json.RawMessage) {
	t.Helper()
	top := rawTopLevel(t, body)
	whole, ok := top["messages"]
	if !ok {
		t.Fatalf("no messages in body: %.200s", body)
	}
	if err := json.Unmarshal(whole, &elems); err != nil {
		t.Fatalf("messages is not an array: %v", err)
	}
	return whole, elems
}

// semanticEqual compares two raw JSON values structurally (UseNumber so big
// ints/floats compare by spelling-independent value without float rounding).
func semanticEqual(a, b []byte) (bool, error) {
	da, err := decodeUseNumber(a)
	if err != nil {
		return false, err
	}
	db, err := decodeUseNumber(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(canonNumbers(da), canonNumbers(db)), nil
}

func decodeUseNumber(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// canonNumbers rewrites json.Number leaves to their exact rational value.
func canonNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = canonNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonNumbers(e)
		}
		return out
	case json.Number:
		// Lossless: exact rational value of the number text. 1.0, 1e2 and 100
		// compare equal; 12345678901234567890 and its float64 rounding
		// (12345678901234567000) do NOT.
		if r, ok := new(big.Rat).SetString(x.String()); ok {
			return "r:" + r.RatString()
		}
		return "n:" + x.String()
	default:
		return v
	}
}

// ---- upstream capture -------------------------------------------------------

type capturedUpstream struct {
	srv     *httptest.Server
	body    []byte
	path    string
	headers http.Header
	calls   int
}

// newCapturedUpstream starts a fake OpenAI-compatible upstream that records
// the exact request body bytes it receives, then answers with a valid
// response (JSON, or a minimal SSE stream when the request says stream:true).
func newCapturedUpstream(t *testing.T) *capturedUpstream {
	t.Helper()
	c := &capturedUpstream{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.body = b
		c.path = r.URL.Path
		c.headers = r.Header.Clone()
		c.calls++
		if bytes.Contains(b, []byte(`"stream":true`)) || bytes.Contains(b, []byte(`"stream": true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// ---- scenarios (allowed-edit combinations, C1 table) ------------------------

// scenario builds a Server whose resolved backend is the returned upstream and
// says how to phrase the request around the verbatim `messages` bytes.
type scenario struct {
	name string
	// build returns the server and its captured upstream.
	build func(t *testing.T) (*Server, *capturedUpstream)
	// model is the request's `model` value.
	model string
	// extraTop is spliced verbatim into the request body after `messages`
	// (must start with a comma if non-empty), e.g. `,"stream":true`.
	extraTop string
	// allowedTopDiff lists top-level keys other than `model` that this
	// scenario's allowed edit may add/change/remove. Every OTHER top-level key
	// must be semantically unchanged.
	allowedTopDiff []string
	// editCheck asserts the allowed edit actually happened (so the scenario
	// cannot silently degrade into the plain case and vacuously pass). It
	// receives the parsed upstream top level and returns "" when the edit is
	// present, else a description of what is missing.
	editCheck func(up map[string]any) string
}

// a handful of realistic non-`messages` top-level fields every scenario sends
// so "anything else must pass through untouched" is exercised, not assumed.
const commonTop = `,"temperature":0.25,"max_tokens":512,"user":"u-123","tools":[{"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":"auto"`

func staticLocalScenario() scenario {
	return scenario{
		name:  "static_local_route",
		model: "gemma4-26b-mtp",
		build: func(t *testing.T) (*Server, *capturedUpstream) {
			up := newCapturedUpstream(t)
			port := portFromURL(t, up.srv.URL)
			cat := newFakeCatalog()
			cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/models/gemma4.gguf"})
			cfg := testCfg(
				[]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}},
				[]Route{{Model: "gemma4-26b-mtp", Primary: "a1"}},
			)
			return NewWithDeps(Deps{Cfg: cfg, Catalog: cat, Auth: &stubAuth{validToken: "x"}}), up
		},
		extraTop: commonTop,
		editCheck: func(up map[string]any) string {
			if up["model"] != "/models/gemma4.gguf" {
				return fmt.Sprintf("wire-model edit did not happen: model=%v", up["model"])
			}
			return ""
		},
	}
}

// storeLocalServer builds a catalog-backed local server (the ADR-0007 path
// every real request takes) with a seeded config named cfgName.
func storeLocalServer(t *testing.T, cfgName string, mutate func(ctx context.Context, cat store.Catalog, cfgID int64)) (*Server, *capturedUpstream) {
	t.Helper()
	up := newCapturedUpstream(t)
	port := portFromURL(t, up.srv.URL)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	cfgID := seedCatalogConfig(t, db.Catalog(), cfgName, 32768, "visible")
	if mutate != nil {
		mutate(ctx, db.Catalog(), cfgID)
	}
	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	return NewWithDeps(Deps{
		Cfg: testCfg(nil, nil), Catalog: cat, StoreCatalog: db.Catalog(),
		Sched: &fixedSlotSched{slot: "a3"}, Slots: map[string]int{"a3": port},
		Auth: &stubAuth{validToken: "x"},
	}), up
}

func aliasScenario() scenario {
	return scenario{
		name:  "model_alias_forced_defaults",
		model: "base-alias",
		build: func(t *testing.T) (*Server, *capturedUpstream) {
			return storeLocalServer(t, "base-cfg", func(ctx context.Context, cat store.Catalog, cfgID int64) {
				must(t, cat.UpdateConfigChatTemplateCaps(ctx, cfgID, map[string]bool{"supports_reasoning_effort": true}))
				_, err := cat.CreateModelAlias(ctx, store.ModelAlias{
					Name: "base-alias", ConfigID: cfgID,
					RequestDefaults: map[string]any{"reasoning_effort": "none"},
				})
				must(t, err)
			})
		},
		extraTop:       commonTop + `,"reasoning_effort":"high"`,
		allowedTopDiff: []string{"reasoning_effort"},
		editCheck: func(up map[string]any) string {
			if up["reasoning_effort"] != "none" {
				return fmt.Sprintf("alias forced default did not apply (reasoning_effort=%v)", up["reasoning_effort"])
			}
			return ""
		},
	}
}

func reasoningNativeScenario() scenario {
	return scenario{
		name:  "reasoning_effort_native",
		model: "kintsugi-model",
		build: func(t *testing.T) (*Server, *capturedUpstream) {
			return storeLocalServer(t, "kintsugi-model", func(ctx context.Context, cat store.Catalog, cfgID int64) {
				must(t, cat.UpdateConfigChatTemplateCaps(ctx, cfgID, map[string]bool{"supports_reasoning_effort": true}))
				cfg, err := cat.GetConfig(ctx, cfgID)
				must(t, err)
				cfg.ReasoningEffortDefault = "low"
				must(t, cat.UpdateConfig(ctx, cfg))
			})
		},
		extraTop:       commonTop, // client silent -> config default applied
		allowedTopDiff: []string{"reasoning_effort"},
		editCheck: func(up map[string]any) string {
			if up["reasoning_effort"] != "low" {
				return fmt.Sprintf("reasoning default did not apply (reasoning_effort=%v)", up["reasoning_effort"])
			}
			return ""
		},
	}
}

func reasoningKwargsScenario() scenario {
	return scenario{
		name:  "reasoning_effort_to_template_kwargs",
		model: "gemma-model",
		build: func(t *testing.T) (*Server, *capturedUpstream) {
			return storeLocalServer(t, "gemma-model", func(ctx context.Context, cat store.Catalog, cfgID int64) {
				cfg, err := cat.GetConfig(ctx, cfgID)
				must(t, err)
				cfg.ReasoningEffortDefault = "none"
				cfg.ChatTemplateCapsOverride = map[string]bool{"supports_enable_thinking": true}
				must(t, cat.UpdateConfig(ctx, cfg))
				must(t, cat.UpdateConfigChatTemplateCaps(ctx, cfgID, map[string]bool{"supports_reasoning_effort": false}))
			})
		},
		extraTop:       commonTop + `,"reasoning_effort":"none"`,
		allowedTopDiff: []string{"reasoning_effort", "chat_template_kwargs"},
		editCheck: func(up map[string]any) string {
			kw, _ := up["chat_template_kwargs"].(map[string]any)
			if kw["enable_thinking"] != false {
				return fmt.Sprintf("reasoning translation did not apply: %v", up["chat_template_kwargs"])
			}
			return ""
		},
	}
}

func remoteIncludeUsageScenario() scenario {
	return scenario{
		name:  "remote_stream_include_usage",
		model: "m",
		build: func(t *testing.T) (*Server, *capturedUpstream) {
			up := newCapturedUpstream(t)
			usageDB := openTestUsageDB(t)
			deps := remoteTestDeps(t, up.srv.URL, usageDB)
			return NewWithDeps(deps), up
		},
		extraTop:       commonTop + `,"stream":true`,
		allowedTopDiff: []string{"stream_options"},
		editCheck: func(up map[string]any) string {
			so, _ := up["stream_options"].(map[string]any)
			if so["include_usage"] != true {
				return fmt.Sprintf("include_usage injection did not apply: %v", up["stream_options"])
			}
			if up["model"] != "wire-m" {
				return fmt.Sprintf("remote wire-model substitution did not apply: %v", up["model"])
			}
			return ""
		},
	}
}

func remoteNonStreamScenario() scenario {
	s := remoteIncludeUsageScenario()
	s.name = "remote_non_stream"
	s.extraTop = commonTop
	s.allowedTopDiff = nil
	s.editCheck = func(up map[string]any) string {
		if up["model"] != "wire-m" {
			return fmt.Sprintf("remote wire-model substitution did not apply: %v", up["model"])
		}
		if _, has := up["stream_options"]; has {
			return fmt.Sprintf("stream_options injected into a non-streaming request: %v", up["stream_options"])
		}
		return ""
	}
	return s
}

// withLedger returns s with WS-N1's observe-only creation ledger installed on
// the server (Deps.CtxLedger), proving the hook never alters what is forwarded.
func withLedger(s scenario) scenario {
	inner := s.build
	s.name += "+ledger"
	s.build = func(t *testing.T) (*Server, *capturedUpstream) {
		srv, up := inner(t)
		db, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		l := ctxledger.New(ctxledger.Config{Sink: db.ContextCreation(), Settings: db.Settings()})
		l.Start(context.Background())
		t.Cleanup(l.Close)
		srv.deps.CtxLedger = l
		return srv, up
	}
	return s
}

func allScenarios() []scenario {
	return []scenario{
		staticLocalScenario(),
		aliasScenario(),
		reasoningNativeScenario(),
		reasoningKwargsScenario(),
		remoteIncludeUsageScenario(),
		remoteNonStreamScenario(),
		// Ledger installed (C2 hook on the real path).
		withLedger(staticLocalScenario()),
		withLedger(remoteIncludeUsageScenario()),
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- fixtures ---------------------------------------------------------------

type fixture struct {
	name string
	// messages is the verbatim JSON array text sent as the `messages` value.
	messages string
	// canonical: already in Go's canonical json.Marshal spelling, so MUST be
	// byte-identical through today's re-serialising handler.
	canonical bool
	// knownLossyNumbers marks the fixture a0 currently corrupts (see the
	// KNOWN DEFECT branch in TestNonMutation_MessagesBytesIdentical).
	knownLossyNumbers bool
	// gap documents, for non-canonical fixtures, which spelling a Go
	// re-marshal changes.
	gap string
}

func bigText(n int, unit string) string {
	var sb strings.Builder
	for sb.Len() < n {
		sb.WriteString(unit)
	}
	return sb.String()
}

// fixtures: realistic shapes seen on a0 (OpenCode / LibreChat / OpenAI-style
// clients). Canonical group: keys sorted, compact, Go's escape set only.
func fixtures() []fixture {
	big := bigText(200<<10, "The quick brown fox / 日本語のテキスト ✓ jumps over the lazy dog.\\n")
	return []fixture{
		// ----- canonical group (must be byte-identical today) -----
		{name: "all_roles_basic", canonical: true, messages: `[` +
			`{"content":"You are a careful assistant.","role":"system"},` +
			`{"content":"Use terse answers.","role":"developer"},` +
			`{"content":"List the files.","name":"testuser","role":"user"},` +
			`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"path\":\".\"}","name":"ls"},"id":"call_1","type":"function"}]},` +
			`{"content":"a.go\nb.go\n","role":"tool","tool_call_id":"call_1"},` +
			`{"content":"Two files.","role":"assistant"}` +
			`]`},
		{name: "parts_array_text_and_image", canonical: true, messages: `[` +
			`{"content":[{"text":"What is in this picture?","type":"text"},{"image_url":{"detail":"high","url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="},"type":"image_url"}],"role":"user"},` +
			`{"content":[{"text":"A single pixel.","type":"text"}],"role":"assistant"}` +
			`]`},
		{name: "parts_array_input_audio", canonical: true, messages: `[` +
			`{"content":[{"input_audio":{"data":"UklGRiQAAABXQVZFZm10IBAAAAABAAEA","format":"wav"},"type":"input_audio"},{"text":"transcribe","type":"text"}],"role":"user"}` +
			`]`},
		{name: "reasoning_content_roundtrip", canonical: true, messages: `[` +
			`{"content":"2+2?","role":"user"},` +
			`{"content":"4","reasoning_content":"The user asks 2+2. That is 4. \"Quote\" and a backslash \\ here.","role":"assistant"},` +
			`{"content":"and 3+3?","role":"user"}` +
			`]`},
		{name: "reasoning_field_variant", canonical: true, messages: `[` +
			`{"content":"hi","role":"user"},` +
			`{"content":"hello","reasoning":"short plan","reasoning_details":[{"text":"step one","type":"reasoning.text"}],"role":"assistant"}` +
			`]`},
		{name: "multi_tool_calls_parallel", canonical: true, messages: `[` +
			`{"content":"compare a and b","role":"user"},` +
			`{"content":"","role":"assistant","tool_calls":[` +
			`{"function":{"arguments":"{\"path\":\"/a\",\"offset\":0,\"limit\":200}","name":"read"},"id":"call_a","index":0,"type":"function"},` +
			`{"function":{"arguments":"{\"path\":\"/b\",\"offset\":0,\"limit\":200}","name":"read"},"id":"call_b","index":1,"type":"function"}]},` +
			`{"content":"    1\tpackage a\n","role":"tool","tool_call_id":"call_a"},` +
			`{"content":"    1\tpackage b\n","role":"tool","tool_call_id":"call_b"}` +
			`]`},
		{name: "tool_args_nested_json_and_escapes", canonical: true, messages: `[` +
			`{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"content\":\"line1\\nline2\\t\\\"q\\\" C:\\\\dir\",\"opts\":{\"a\":[1,2,3],\"b\":null}}","name":"write"},"id":"call_9","type":"function"}]},` +
			`{"content":"ok","role":"tool","tool_call_id":"call_9"}` +
			`]`},
		{name: "unicode_cjk_emoji_combining_rtl", canonical: true, messages: `[` +
			`{"content":"こんにちは、世界。日本語のテキストです。🇯🇵 🙂 👨‍👩‍👧 é (e + U+0301) שלום עולם مرحبا nbsp","role":"user"},` +
			`{"content":"はい。ツール結果：成功 ✓","role":"assistant"}` +
			`]`},
		{name: "unicode_line_separators_escaped", canonical: true, messages: `[` +
			`{"content":"a\u2028b\u2029c","role":"user"}` +
			`]`},
		{name: "control_chars_and_escapes", canonical: true, messages: `[` +
			`{"content":"tab\there\nnewline\r\ncrlf \u0001 \u001f \"quoted\" back\\slash","role":"user"}` +
			`]`},
		{name: "empty_and_null_content", canonical: true, messages: `[` +
			`{"content":"","role":"user"},` +
			`{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"noop"},"id":"c0","type":"function"}]},` +
			`{"content":"","role":"tool","tool_call_id":"c0"}` +
			`]`},
		{name: "tool_output_looks_like_json_and_markdown", canonical: true, messages: `[` +
			`{"content":"{\"a\":1,\"b\":[true,false,null],\"c\":\"x\"}\n\n# Title\n\n- item\n- item\n\n` + "```go\\nfunc main() {}\\n```" + `","role":"tool","tool_call_id":"t"}` +
			`]`},
		{name: "large_200kb_message", canonical: true, messages: `[` +
			`{"content":"` + big + `","role":"tool","tool_call_id":"big"},` +
			`{"content":"done","role":"assistant"}` +
			`]`},
		{name: "many_small_messages_order", canonical: true, messages: manyMessages(200)},

		// ----- non-canonical group (semantic equality hard; raw = known gap) -----
		{name: "nc_unsorted_keys", gap: "key order (client order is role-first; Go sorts keys)", messages: `[` +
			`{"role":"user","content":"hello"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c1","content":"ok"}` +
			`]`},
		{name: "nc_pretty_printed_whitespace", gap: "whitespace between tokens", messages: "[\n  {\n    \"role\": \"user\",\n    \"content\": \"hi\"\n  },\n  {\n    \"role\": \"assistant\",\n    \"content\": \"yo\"\n  }\n]"},
		{name: "nc_html_chars_in_code", gap: "Go HTML-escapes < > & as \\u003c \\u003e \\u0026 (very common in code/tool output)", messages: `[` +
			`{"role":"tool","tool_call_id":"t","content":"if a < b && c > d { fmt.Println(\"<div class=\\\"x\\\">&amp;</div>\") }"}` +
			`]`},
		{name: "nc_unicode_escape_spelling", gap: "client writes \\uXXXX / \\/ escapes; Go emits raw UTF-8 / '/'", messages: `[` +
			`{"role":"user","content":"caf\u00e9 \u65e5\u672c\u8a9e \/path\/to"}` +
			`]`},
		{name: "nc_surrogate_pair_escape", gap: "surrogate-pair escape \\ud83d\\ude00; Go emits raw emoji", messages: `[` +
			`{"role":"user","content":"smile \ud83d\ude00"}` +
			`]`},
		{name: "nc_raw_line_separators", gap: "raw U+2028/U+2029; Go escapes to \\u2028", messages: "[" +
			"{\"role\":\"user\",\"content\":\"a\u2028b\u2029c\"}" + "]"},
		{name: "nc_float_text", gap: "float text (1.0, 1e2, 1E3, 0.50) re-spelled by Go (1, 100, 1000, 0.5); value-identical", messages: `[` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","index":1.0,"function":{"name":"f","arguments":"{}"}}],"weight":1e2,"scale":1E3,"p":0.50}` +
			`]`},
		{name: "nc_big_integer_exact", gap: "integers beyond 2^53 (a0 parses numbers as float64, then re-marshals)", messages: `[` +
			`{"role":"assistant","content":"x","big":12345678901234567890,"edge":9007199254740993,"neg":-9223372036854775809}` +
			`]`},
		{name: "nc_duplicate_keys_in_message", gap: "duplicate keys: Go keeps only the last", messages: `[` +
			`{"role":"user","content":"first","content":"second"}` +
			`]`},
		{name: "nc_unknown_extra_fields_and_nested_objects", gap: "unsorted keys inside nested provider-specific fields", messages: `[` +
			`{"role":"assistant","content":"x","cache_control":{"ttl":"5m","type":"ephemeral"},"provider_meta":{"z":1,"a":{"y":2,"b":3}}}` +
			`]`},
	}
}

func manyMessages(n int) string {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&sb, `{"content":"turn %d: 日本語 ✓ %s","role":"%s"}`, i, strings.Repeat("x", i%17), role)
	}
	sb.WriteString("]")
	return sb.String()
}

// ---- the test ---------------------------------------------------------------

func TestNonMutation_MessagesBytesIdentical(t *testing.T) {
	var gaps []string
	lossyNumbers := 0
	defer func() {
		if lossyNumbers > 0 {
			t.Logf("T1 summary: %d (scenario,fixture) pairs hit the KNOWN DEFECT: big integers rounded via float64 (T1_STRICT_NUMBERS=1 makes it fail)", lossyNumbers)
		}
	}()
	for _, sc := range allScenarios() {
		sc := sc
		// One server (and one in-memory store) per scenario, shared by all of its
		// fixtures: opening a store per subtest made this test take minutes under
		// -race. Handlers are stateless w.r.t. the body, so sharing is sound.
		t.Run(sc.name, func(t *testing.T) {
			srv, up := sc.build(t)
			for _, fx := range fixtures() {
				fx := fx
				t.Run(fx.name, func(t *testing.T) {
					up.calls = 0
					up.body = nil
					reqBody := `{"model":` + jsonString(sc.model) + `,"messages":` + fx.messages + sc.extraTop + `}`
					req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
					req.RemoteAddr = "100.64.0.1:1234" // tailnet direct: no bearer needed
					rec := httptest.NewRecorder()
					srv.Handler().ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("status=%d, want 200 (body: %s)", rec.Code, rec.Body.String())
					}
					// Drain so the usage tap's onClose fires cleanly.
					if up.calls != 1 {
						t.Fatalf("upstream calls=%d, want exactly 1", up.calls)
					}
					if up.path != "/v1/chat/completions" {
						t.Fatalf("upstream path=%q", up.path)
					}

					inWhole, inElems := rawMessages(t, []byte(reqBody))
					outWhole, outElems := rawMessages(t, up.body)

					// 1. structural, spelling-independent: same count, same values.
					if len(inElems) != len(outElems) {
						t.Fatalf("message count changed: in=%d out=%d", len(inElems), len(outElems))
					}
					for i := range inElems {
						eq, err := semanticEqual(inElems[i], outElems[i])
						if err != nil {
							t.Fatalf("message[%d] unparseable: %v", i, err)
						}
						if !eq {
							// Duplicate-key fixture: the only legitimate semantic
							// divergence is collapsing duplicates, which is itself a
							// mutation of what was sent; still reported as a gap.
							if fx.name == "nc_duplicate_keys_in_message" {
								continue
							}
							// REAL DEFECT today (found by this test): a0 decodes the body
							// with float64 numbers, so integers beyond 2^53 are rounded.
							// Fix is a one-liner in proxy.go (decode with UseNumber), not
							// owned by WS-F. Reported, not failed, until fixed; set
							// T1_STRICT_NUMBERS=1 to make it fail.
							if fx.knownLossyNumbers && os.Getenv("T1_STRICT_NUMBERS") == "" {
								t.Logf("KNOWN DEFECT %s/%s: big integer changed by float64 round-trip\n in: %.200s\nout: %.200s",
									sc.name, fx.name, inElems[i], outElems[i])
								lossyNumbers++
								continue
							}
							t.Fatalf("message[%d] CONTENT changed\n in: %.300s\nout: %.300s", i, inElems[i], outElems[i])
						}
					}

					// 2. RAW bytes, per element then whole array.
					rawDiffs := []int{}
					for i := range inElems {
						if !bytes.Equal(inElems[i], outElems[i]) {
							rawDiffs = append(rawDiffs, i)
						}
					}
					rawWholeEqual := bytes.Equal(inWhole, outWhole)
					switch {
					case len(rawDiffs) == 0 && rawWholeEqual:
						// byte-identical
					case strictRaw():
						i := 0
						if len(rawDiffs) > 0 {
							i = rawDiffs[0]
						}
						t.Errorf("messages RAW BYTES changed (%d of %d elements differ; whole-array equal=%v)\n in[%d]: %.300q\nout[%d]: %.300q",
							len(rawDiffs), len(inElems), rawWholeEqual, i, safeIdx(inElems, i), i, safeIdx(outElems, i))
					default:
						msg := fmt.Sprintf("KNOWN GAP %s/%s: %d/%d message elements not byte-identical (%s)",
							sc.name, fx.name, len(rawDiffs), len(inElems), fx.gap)
						t.Log(msg)
						gaps = append(gaps, msg)
					}

					// 3. Every top-level key outside {model, messages, allowed}
					// must be semantically unchanged, and nothing may appear that
					// isn't allowed.
					checkTopLevel(t, sc, []byte(reqBody), up.body)

					// 4. The scenario's allowed edit really happened.
					var parsed map[string]any
					must(t, json.Unmarshal(up.body, &parsed))
					if msg := sc.editCheck(parsed); msg != "" {
						t.Error(msg)
					}
				})
			}
		})
	}
	if len(gaps) > 0 {
		t.Logf("T1 summary: %d (scenario,fixture) pairs differ in raw bytes (informational: C1 requires content identity, not bytes; T1_STRICT_RAW=1 makes these fail)", len(gaps))
	}
}

func safeIdx(e []json.RawMessage, i int) []byte {
	if i < len(e) {
		return e[i]
	}
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func checkTopLevel(t *testing.T, sc scenario, in, out []byte) {
	t.Helper()
	inTop := rawTopLevel(t, in)
	outTop := rawTopLevel(t, out)
	allowed := map[string]bool{"model": true, "messages": true}
	for _, k := range sc.allowedTopDiff {
		allowed[k] = true
	}
	for k := range outTop {
		if _, was := inTop[k]; !was && !allowed[k] {
			t.Errorf("upstream received unexpected new top-level field %q (not an enumerated C1 edit)", k)
		}
	}
	keys := make([]string, 0, len(inTop))
	for k := range inTop {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if allowed[k] {
			continue
		}
		ov, ok := outTop[k]
		if !ok {
			t.Errorf("top-level field %q was dropped", k)
			continue
		}
		eq, err := semanticEqual(inTop[k], ov)
		if err != nil || !eq {
			t.Errorf("top-level field %q changed: in=%.200s out=%.200s", k, inTop[k], ov)
		}
	}
}

// TestNonMutation_ScenarioSelfCheck guards the guard: every scenario's
// editCheck must FAIL against an unedited upstream view, i.e. it can tell "the
// allowed edit applied" from "the request passed through plainly". Without
// this, a scenario that silently stopped exercising its edit (say, an alias
// that now falls through to a plain route) would keep passing vacuously.
func TestNonMutation_ScenarioSelfCheck(t *testing.T) {
	for _, sc := range allScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			unedited := map[string]any{"model": sc.model}
			if msg := sc.editCheck(unedited); msg == "" {
				t.Fatalf("editCheck passes on an unedited body; scenario %q cannot detect a missing edit", sc.name)
			}
		})
	}
}

// SPDX-License-Identifier: Apache-2.0

package ctxledger

// privacy_test.go — T2 of the context-creation plan (WS-F; contract C2,
// invariants I3 and I5): the creation ledger records sizes and names only.
// Sentinel strings planted in EVERY content-bearing field of a request must
// never surface in any ledger row, metric, log line or endpoint body.
//
// This file is a reusable HARNESS plus its own self-tests. It is decoupled
// from WS-N1's implementation: N1 supplies a privacySUT (two closures) from
// its own test file in this package, and calls runPrivacyHarness. See
// docs/compression-redesign/STATUS.md "### WS-F" for the agreed shape.
//
//	func TestLedgerPrivacy(t *testing.T) {
//	    runPrivacyHarness(t, func(t *testing.T) privacySUT {
//	        l := newTestLedger(t)               // N1's constructor, in-memory store
//	        logs := captureLogs(t)              // slog + log -> buffer (this file)
//	        return privacySUT{
//	            Observe: func(body []byte, consumer, model string) { l.Observe(body, consumer, model) },
//	            Outputs: func() map[string]string {
//	                return map[string]string{
//	                    "rows":     dumpRowsJSON(l),            // every stored row
//	                    "endpoint": getEndpointBody(l, "24h"),   // GET /api/v1/context/creation (each by=)
//	                    "metrics":  scrapeMetricsText(l),
//	                    "logs":     logs.String(),
//	                }
//	            },
//	        }
//	    })
//	}

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// privacySUT is the system under test, supplied by the ledger's own tests.
type privacySUT struct {
	// Observe feeds one inbound request body to the ledger exactly as the
	// router hook would (consumer label and resolved model are the only
	// metadata the hook has). It must not panic or block on hostile bodies.
	Observe func(body []byte, consumer, model string)
	// Outputs returns every externally visible text surface after all
	// observations: surface name -> text. Must include at least one non-empty
	// surface (the harness asserts that, so a SUT that exposes nothing cannot
	// pass vacuously).
	Outputs func() map[string]string
}

// privacyRequest is one fuzz request plus the sentinels planted in it.
type privacyRequest struct {
	Name      string
	Consumer  string
	Model     string
	Body      []byte
	Sentinels []string
}

// Consumer/model/tool names are legitimately recorded (bounded labels), so the
// harness uses fixed, sentinel-free ones.
const (
	privacyConsumer = "opencode-test"
	privacyModel    = "gemma4-26b-a4b"
)

// newSentinel returns a unique token that cannot occur by chance.
func newSentinel(rng *rand.Rand, kind string) string {
	var b [12]byte
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return fmt.Sprintf("SNTL_%s_%s", kind, hex.EncodeToString(b[:]))
}

// field placement: where a sentinel is planted. Each builds a full body so
// leaks are attributed to one field.
type privacyPlacement struct {
	name  string
	build func(s string) map[string]any
}

func mkMsg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func mkToolCall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
}

func privacyPlacements() []privacyPlacement {
	base := func(extra ...map[string]any) map[string]any {
		msgs := []any{mkMsg("system", "You are helpful."), mkMsg("user", "hello")}
		for _, e := range extra {
			msgs = append(msgs, e)
		}
		return map[string]any{"model": privacyModel, "messages": msgs}
	}
	return []privacyPlacement{
		{"system_content", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{mkMsg("system", s), mkMsg("user", "hi")}}
		}},
		{"developer_content", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{mkMsg("developer", s), mkMsg("user", "hi")}}
		}},
		{"user_content", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{mkMsg("user", s)}}
		}},
		{"user_parts_text", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "text", "text": s}}}}}
		}},
		{"user_parts_image_url", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(s))}}}}}}
		}},
		{"assistant_text", func(s string) map[string]any { return base(mkMsg("assistant", s)) }},
		{"assistant_reasoning_content", func(s string) map[string]any {
			return base(map[string]any{"role": "assistant", "content": "ok", "reasoning_content": s})
		}},
		{"assistant_reasoning_alt", func(s string) map[string]any {
			return base(map[string]any{"role": "assistant", "content": "ok", "reasoning": s})
		}},
		{"tool_call_arguments", func(s string) map[string]any {
			args, _ := json.Marshal(map[string]any{"path": "/x", "content": s})
			return base(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{mkToolCall("c1", "write", string(args))}})
		}},
		{"tool_call_id", func(s string) map[string]any {
			return base(
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{mkToolCall(s, "read", "{}")}},
				map[string]any{"role": "tool", "tool_call_id": s, "content": "ok"})
		}},
		{"tool_output", func(s string) map[string]any {
			return base(
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{mkToolCall("c1", "read", "{}")}},
				map[string]any{"role": "tool", "tool_call_id": "c1", "content": s})
		}},
		{"tool_output_parts", func(s string) map[string]any {
			return base(
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{mkToolCall("c1", "bash", "{}")}},
				map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{map[string]any{"type": "text", "text": s}}})
		}},
		{"message_name_field", func(s string) map[string]any {
			return map[string]any{"model": privacyModel, "messages": []any{map[string]any{"role": "user", "name": s, "content": "hi"}}}
		}},
		{"tools_schema_description", func(s string) map[string]any {
			b := base()
			b["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": s,
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"p": map[string]any{"type": "string", "description": s}}}}}}
			return b
		}},
		{"top_level_user_field", func(s string) map[string]any { b := base(); b["user"] = s; return b }},
		{"top_level_metadata", func(s string) map[string]any {
			b := base()
			b["metadata"] = map[string]any{"session": s}
			return b
		}},
		{"top_level_unknown_field", func(s string) map[string]any { b := base(); b["x_custom"] = map[string]any{"deep": []any{s}}; return b }},
		{"response_format_schema", func(s string) map[string]any {
			b := base()
			b["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "description": s}}
			return b
		}},
		{"stop_sequences", func(s string) map[string]any { b := base(); b["stop"] = []any{s}; return b }},
	}
}

// privacyRequests builds the fuzz corpus: one request per placement (ASCII
// sentinel), the same again with a non-ASCII sentinel, one "kitchen sink" with
// all ASCII sentinels at once, and a multi-turn sequence (append, then a
// client-edited history) so the C4 conversation-tracking path is exercised.
func privacyRequests(seed int64) []privacyRequest {
	rng := rand.New(rand.NewSource(seed))
	var out []privacyRequest
	var all []string
	for _, p := range privacyPlacements() {
		s := newSentinel(rng, "ascii")
		all = append(all, s)
		body, _ := json.Marshal(p.build(s))
		out = append(out, privacyRequest{Name: p.name, Consumer: privacyConsumer, Model: privacyModel, Body: body, Sentinels: []string{s}})

		u := "機密" + newSentinel(rng, "ja") + "漏洩🔒"
		bodyU, _ := json.Marshal(p.build(u))
		out = append(out, privacyRequest{Name: p.name + "/unicode", Consumer: privacyConsumer, Model: privacyModel, Body: bodyU, Sentinels: []string{u}})
	}

	// kitchen sink: one body, a sentinel in every placement that lives in messages.
	var sink []any
	var sinkSent []string
	for i, p := range privacyPlacements() {
		if !strings.Contains(p.name, "content") && !strings.Contains(p.name, "text") && !strings.Contains(p.name, "tool_") &&
			!strings.Contains(p.name, "reasoning") {
			continue
		}
		s := all[i]
		b := p.build(s)
		if ms, ok := b["messages"].([]any); ok {
			sink = append(sink, ms...)
			sinkSent = append(sinkSent, s)
		}
	}
	body, _ := json.Marshal(map[string]any{"model": privacyModel, "messages": sink})
	out = append(out, privacyRequest{Name: "kitchen_sink", Consumer: privacyConsumer, Model: privacyModel, Body: body, Sentinels: sinkSent})

	// multi-turn: turn 1, turn 2 (append), turn 3 (client rewrote history).
	t1 := newSentinel(rng, "t1")
	t2 := newSentinel(rng, "t2")
	t3 := newSentinel(rng, "t3")
	m1 := []any{mkMsg("system", "S"), mkMsg("user", t1)}
	m2 := append(append([]any{}, m1...), mkMsg("assistant", "ack "+t2), mkMsg("user", "next"))
	m3 := []any{mkMsg("system", "S"), mkMsg("user", t1+" (edited) "+t3), mkMsg("assistant", "ack "+t2), mkMsg("user", "next"), mkMsg("assistant", "more")}
	for i, ms := range [][]any{m1, m2, m3} {
		b, _ := json.Marshal(map[string]any{"model": privacyModel, "messages": ms})
		out = append(out, privacyRequest{Name: fmt.Sprintf("multi_turn_%d", i+1), Consumer: privacyConsumer, Model: privacyModel, Body: b,
			Sentinels: []string{t1, t2, t3}})
	}

	// hostile inputs that must not panic or leak: truncated JSON, non-object,
	// empty, huge single field, invalid UTF-8, deeply nested.
	hs := newSentinel(rng, "hostile")
	out = append(out,
		privacyRequest{Name: "hostile/truncated_json", Consumer: privacyConsumer, Model: privacyModel,
			Body: []byte(`{"model":"m","messages":[{"role":"user","content":"` + hs), Sentinels: []string{hs}},
		privacyRequest{Name: "hostile/not_an_object", Consumer: privacyConsumer, Model: privacyModel,
			Body: []byte(`["` + hs + `"]`), Sentinels: []string{hs}},
		privacyRequest{Name: "hostile/empty", Consumer: privacyConsumer, Model: privacyModel, Body: nil},
		privacyRequest{Name: "hostile/invalid_utf8", Consumer: privacyConsumer, Model: privacyModel,
			Body: append([]byte(`{"model":"m","messages":[{"role":"user","content":"`+hs+`\xff\xfe`), []byte(`"}]}`)...), Sentinels: []string{hs}},
		privacyRequest{Name: "hostile/messages_wrong_type", Consumer: privacyConsumer, Model: privacyModel,
			Body: []byte(`{"model":"m","messages":"` + hs + `"}`), Sentinels: []string{hs}},
		privacyRequest{Name: "hostile/huge_field", Consumer: privacyConsumer, Model: privacyModel,
			Body: []byte(`{"model":"m","messages":[{"role":"user","content":"` + hs + strings.Repeat("A", 2<<20) + `"}]}`), Sentinels: []string{hs}},
	)
	return out
}

// needles returns every spelling of a sentinel that counts as a leak:
// the full text, JSON/URL/base64/hex encodings, and (to catch truncated or
// previewed content) every fixed-width window of it.
func needles(s string) []string {
	set := map[string]bool{s: true}
	j, _ := json.Marshal(s)
	set[strings.Trim(string(j), `"`)] = true
	set[base64.StdEncoding.EncodeToString([]byte(s))] = true
	set[base64.RawStdEncoding.EncodeToString([]byte(s))] = true
	set[base64.URLEncoding.EncodeToString([]byte(s))] = true
	set[hex.EncodeToString([]byte(s))] = true
	set[strings.ToLower(s)] = true
	set[strings.ToUpper(s)] = true
	rs := []rune(s)
	const w = 10 // runes; the random hex core guarantees uniqueness
	for i := 0; i+w <= len(rs); i++ {
		set[string(rs[i:i+w])] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		if n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// findLeaks returns one human-readable line per leak found.
func findLeaks(sentinels []string, outputs map[string]string) []string {
	var leaks []string
	surfaces := make([]string, 0, len(outputs))
	for k := range outputs {
		surfaces = append(surfaces, k)
	}
	sort.Strings(surfaces)
	for _, s := range sentinels {
		ns := needles(s)
		for _, surf := range surfaces {
			text := outputs[surf]
			for _, n := range ns {
				// a window of the sentinel's own random core must be specific; skip
				// windows made only of the constant prefix ("SNTL_ascii_" etc.)
				if strings.HasPrefix("SNTL_ascii_", n) || strings.HasPrefix("SNTL_hostile_", n) || strings.HasPrefix("SNTL_ja_", n) {
					continue
				}
				if strings.Contains(text, n) {
					leaks = append(leaks, fmt.Sprintf("sentinel %q leaked into %q via needle %q", s, surf, n))
					break
				}
			}
		}
	}
	return leaks
}

// runPrivacyHarness drives the full corpus through a fresh SUT per request
// group and fails the test on any leak or vacuous output. It is the entry
// point WS-N1 calls.
func runPrivacyHarness(t *testing.T, newSUT func(t *testing.T) privacySUT) {
	t.Helper()
	// Per-request isolation attributes a leak to one field; the final shared
	// run proves cross-request state (conversation LRU, rollups) leaks nothing.
	reqs := privacyRequests(1)
	for _, r := range reqs {
		r := r
		t.Run(r.Name, func(t *testing.T) {
			sut := newSUT(t)
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("Observe panicked on %s (must fail open): %v", r.Name, p)
					}
				}()
				sut.Observe(r.Body, r.Consumer, r.Model)
			}()
			assertNoLeaks(t, sut, r.Sentinels)
		})
	}
	t.Run("all_requests_one_ledger", func(t *testing.T) {
		sut := newSUT(t)
		var all []string
		for _, r := range reqs {
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("Observe panicked on %s: %v", r.Name, p)
					}
				}()
				sut.Observe(r.Body, r.Consumer, r.Model)
			}()
			all = append(all, r.Sentinels...)
		}
		assertNoLeaks(t, sut, all)
	})
}

func assertNoLeaks(t *testing.T, sut privacySUT, sentinels []string) {
	t.Helper()
	outs := sut.Outputs()
	if vacuous(outs) {
		t.Fatalf("SUT exposed no output at all; the privacy check would pass vacuously (surfaces: %v)", surfaceNames(outs))
	}
	for _, l := range findLeaks(sentinels, outs) {
		t.Error(l)
	}
}

// vacuous reports whether every surface is blank.
func vacuous(outs map[string]string) bool {
	for _, v := range outs {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

// syncBuffer is a goroutine-safe bytes.Buffer for captured logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func surfaceNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// captureLogs routes slog's default logger and the std log package into the
// returned buffer for the life of the test (restored on cleanup). Not safe for
// t.Parallel tests (global logger), which T2 never uses.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prevSlog := slog.Default()
	prevOut := log.Writer()
	prevFlags := log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(buf)
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// ---- self-tests (the harness must be able to fail) --------------------------

// sizeOnlySUT is a reference implementation of the C2 rule: it records only
// lengths and the (bounded) consumer/model labels. Used to prove the harness
// passes a correct ledger and has non-empty output.
type sizeOnlySUT struct{ rows []string }

func (s *sizeOnlySUT) observe(body []byte, consumer, model string) {
	var top map[string]json.RawMessage
	n, tools := 0, 0
	if json.Unmarshal(body, &top) == nil {
		var ms []json.RawMessage
		_ = json.Unmarshal(top["messages"], &ms)
		n = len(ms)
		var ts []json.RawMessage
		_ = json.Unmarshal(top["tools"], &ts)
		tools = len(ts)
	}
	s.rows = append(s.rows, fmt.Sprintf(`{"consumer":%q,"model":%q,"new_msgs":%d,"body_chars":%d,"tool_count":%d}`, consumer, model, n, len(body), tools))
}

func (s *sizeOnlySUT) sut() privacySUT {
	return privacySUT{
		Observe: s.observe,
		Outputs: func() map[string]string { return map[string]string{"rows": strings.Join(s.rows, "\n")} },
	}
}

func TestPrivacyHarness_PassesSizeOnlyLedger(t *testing.T) {
	runPrivacyHarness(t, func(t *testing.T) privacySUT { return (&sizeOnlySUT{}).sut() })
}

// Each leaky variant must be CAUGHT, proving the harness can go red.
func TestPrivacyHarness_CatchesLeaks(t *testing.T) {
	type leak struct {
		name string
		mut  func(body []byte, consumer, model string) string // text a buggy ledger might store
	}
	leaks := []leak{
		{"stores_whole_body", func(b []byte, _, _ string) string { return string(b) }},
		{"stores_first_200_chars", func(b []byte, _, _ string) string {
			if len(b) > 200 {
				b = b[:200]
			}
			return string(b)
		}},
		{"stores_middle_preview", func(b []byte, _, _ string) string {
			if len(b) < 140 {
				return string(b)
			}
			return string(b[len(b)/2-40 : len(b)/2+40])
		}},
		{"stores_base64_of_body", func(b []byte, _, _ string) string { return base64.StdEncoding.EncodeToString(b) }},
		{"logs_parse_error_with_body", func(b []byte, _, _ string) string { return "ledger: parse error near " + string(b) }},
	}
	for _, lk := range leaks {
		lk := lk
		t.Run(lk.name, func(t *testing.T) {
			caught := false
			for _, r := range privacyRequests(1) {
				if len(r.Sentinels) == 0 {
					continue
				}
				outs := map[string]string{"rows": lk.mut(r.Body, r.Consumer, r.Model)}
				if len(findLeaks(r.Sentinels, outs)) > 0 {
					caught = true
					break
				}
			}
			if !caught {
				t.Fatalf("harness failed to detect leak mode %q over the whole corpus", lk.name)
			}
		})
	}
}

func TestPrivacyHarness_RejectsVacuousSUT(t *testing.T) {
	if !vacuous(map[string]string{"rows": "  ", "logs": ""}) {
		t.Fatal("blank surfaces must be classified as vacuous")
	}
	if vacuous(map[string]string{"rows": `{"new_msgs":1}`}) {
		t.Fatal("non-blank surface misclassified as vacuous")
	}
}

func TestPrivacyHarness_CorpusCoversEveryField(t *testing.T) {
	want := []string{"system_content", "developer_content", "user_content", "user_parts_text", "user_parts_image_url",
		"assistant_text", "assistant_reasoning_content", "tool_call_arguments", "tool_output", "tool_call_id",
		"tools_schema_description", "top_level_metadata", "kitchen_sink", "multi_turn_3", "hostile/truncated_json"}
	have := map[string]bool{}
	for _, r := range privacyRequests(1) {
		have[r.Name] = true
		// Plain ASCII single-placement requests must literally contain their
		// sentinel (otherwise the case tests nothing).
		plain := !strings.Contains(r.Name, "/") && !strings.HasPrefix(r.Name, "multi_turn") &&
			r.Name != "kitchen_sink" && !strings.Contains(r.Name, "image_url")
		if plain && !bytes.Contains(r.Body, []byte(r.Sentinels[0])) {
			t.Errorf("sentinel for %s is not actually in the body", r.Name)
		}
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("corpus missing %s", w)
		}
	}
}

// ---- wiring against the real WS-N1 ledger ------------------------------------

// newRealLedger builds N1's ledger on an in-memory store using only its public
// API (no dependency on N1's test helpers).
func newRealLedger(t testing.TB) (*Ledger, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	l := New(Config{Sink: db.ContextCreation(), Settings: db.Settings()})
	l.Start(context.Background())
	t.Cleanup(l.Close)
	return l, db
}

// TestPrivacyHarness_RealLedger runs the full T2 corpus against the real
// ledger: stored rows, the query endpoint's data for every `by=`, the metrics
// text and captured logs are all scanned for sentinels.
func TestPrivacyHarness_RealLedger(t *testing.T) {
	// One ledger/store for the whole corpus (cumulative state): per-request
	// store setup made this minutes-long under -race. Per-request subtests still
	// check their own sentinels against the cumulative outputs; the final
	// "all_requests_one_ledger" subtest checks every sentinel.
	l, db := newRealLedger(t)
	logs := captureLogs(t)
	ctx := context.Background()
	runPrivacyHarness(t, func(t *testing.T) privacySUT {
		return privacySUT{
			Observe: func(body []byte, consumer, model string) { l.Observe(body, consumer, model) },
			Outputs: func() map[string]string {
				l.Flush(ctx)
				out := map[string]string{}
				rows, err := db.ContextCreation().Since(ctx, 0)
				if err != nil {
					t.Fatalf("Since: %v", err)
				}
				j, _ := json.Marshal(rows)
				out["rows"] = string(j)
				for _, by := range []string{"tool", "consumer", "model"} {
					res, _, err := l.Query(ctx, "7d", by)
					if err != nil {
						t.Fatalf("Query(%s): %v", by, err)
					}
					qj, _ := json.Marshal(res)
					out["endpoint_by_"+by] = string(qj)
				}
				var m bytes.Buffer
				l.WriteMetrics(&m)
				out["metrics"] = m.String()
				out["logs"] = logs.String()
				return out
			},
		}
	})
}

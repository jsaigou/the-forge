// SPDX-License-Identifier: Apache-2.0

package ctxledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type msg = map[string]any

func body(msgs []msg, tools int) []byte {
	b := map[string]any{"model": "m", "messages": msgs}
	if tools > 0 {
		var ts []any
		for i := 0; i < tools; i++ {
			ts = append(ts, msg{"type": "function", "function": msg{"name": fmt.Sprintf("t%d", i)}})
		}
		b["tools"] = ts
	}
	out, _ := json.Marshal(b)
	return out
}

func newTest(t *testing.T) (*Ledger, *store.DB) {
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

func rows(t *testing.T, l *Ledger, db *store.DB) []store.ContextCreationRow {
	t.Helper()
	l.Flush(context.Background())
	r, err := db.ContextCreation().Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func find(rs []store.ContextCreationRow, consumer, tool string) store.ContextCreationRow {
	var out store.ContextCreationRow
	for _, r := range rs {
		if r.Consumer == consumer && r.Tool == tool {
			out.Merge(r)
		}
	}
	return out
}

func call(id, name, args string) msg {
	return msg{"id": id, "type": "function", "function": msg{"name": name, "arguments": args}}
}

// T4: multi-turn conversation -> correct per-tool created_chars; edited history -> suspect.
func TestMultiTurnAttributionAndCacheBust(t *testing.T) {
	l, db := newTest(t)
	sys := msg{"role": "system", "content": "sys"}
	u1 := msg{"role": "user", "content": "hello"} // 5
	// request 1: system + user -> new = system(other 3) + user(5)
	l.Observe(body([]msg{sys, u1}, 3), "oc", "m")
	a1 := msg{"role": "assistant", "content": "ok", "reasoning_content": "thinking", // 2 + 8
		"tool_calls": []any{call("c1", "web_fetch_mcp_webfetch", `{"url":"x"}`)}} // args 11
	tr1 := msg{"role": "tool", "tool_call_id": "c1", "content": strings.Repeat("x", 9000)}
	u2 := msg{"role": "user", "content": "more"} // 4
	// request 2: new = a1, tr1, u2
	l.Observe(body([]msg{sys, u1, a1, tr1, u2}, 3), "oc", "m")
	// request 3: user-agent-style retry of the same body -> no new messages
	l.Observe(body([]msg{sys, u1, a1, tr1, u2}, 3), "oc", "m")

	rs := rows(t, l, db)
	req := find(rs, "oc", "")
	if req.Requests != 3 {
		t.Fatalf("requests=%d", req.Requests)
	}
	if req.UserChars != 5+4 || req.OtherChars != 3 || req.AssistantTextChars != 2 ||
		req.AssistantReasoningChars != 8 || req.ToolArgsChars != 11 {
		t.Fatalf("created mismatch: %+v", req)
	}
	if req.NewMsgs != 2+3+0 {
		t.Fatalf("new_msgs=%d", req.NewMsgs)
	}
	if req.CacheBustSuspects != 0 {
		t.Fatalf("false suspect")
	}
	if req.SchemaRequests != 3 || req.SchemaCharsSum == 0 {
		t.Fatalf("schema: %+v", req)
	}
	tool := find(rs, "oc", "web_fetch")
	if tool.ToolOutputChars != 9000 || tool.ToolResults != 1 || tool.Big8k != 1 || tool.Big24k != 0 || tool.MaxResult != 9000 {
		t.Fatalf("tool row: %+v", tool)
	}

	// request 4: client EDITS history (u1 changed) -> suspect, whole body new.
	u1e := msg{"role": "user", "content": "HELLO"}
	l.Observe(body([]msg{sys, u1e, a1, tr1, u2, {"role": "user", "content": "z"}}, 3), "oc", "m")
	rs = rows(t, l, db)
	req = find(rs, "oc", "")
	// u1e != u1 so the first-two-non-system fingerprint changed too: a new
	// conversation key => treated as first request, NOT a suspect. A mid-history
	// edit (below) is what trips the detector.
	if req.CacheBustSuspects != 0 {
		t.Fatalf("edit of the fingerprint messages is a new conversation, got suspects=%d", req.CacheBustSuspects)
	}

	// request 5: edit deeper (tool output compressed by the client) -> suspect.
	trEdited := msg{"role": "tool", "tool_call_id": "c1", "content": "[truncated]"}
	l.Observe(body([]msg{sys, u1e, a1, trEdited, u2, {"role": "user", "content": "z"}, {"role": "user", "content": "w"}}, 3), "oc", "m")
	rs = rows(t, l, db)
	req = find(rs, "oc", "")
	if req.CacheBustSuspects != 1 {
		t.Fatalf("suspects=%d want 1", req.CacheBustSuspects)
	}
	// metrics + endpoint shape
	var buf bytes.Buffer
	l.WriteMetrics(&buf)
	if !strings.Contains(buf.String(), `context_cache_bust_suspect_total{consumer="oc"} 1`) ||
		!strings.Contains(buf.String(), `context_tool_output_chars_total{tool="web_fetch"}`) {
		t.Fatalf("metrics:\n%s", buf.String())
	}
	res, ok, err := l.Query(context.Background(), "24h", "tool")
	if !ok || err != nil || res.CacheBustSuspects != 1 || len(res.Rows) == 0 {
		t.Fatalf("query: %+v %v %v", res, ok, err)
	}
}

func TestTruncatedHistoryIsSuspect(t *testing.T) {
	l, db := newTest(t)
	m := func(r, c string) msg { return msg{"role": r, "content": c} }
	l.Observe(body([]msg{m("user", "a"), m("assistant", "b"), m("user", "c"), m("assistant", "d"), m("user", "e")}, 0), "c", "m")
	// client compacts: drops the middle but keeps the first two messages
	l.Observe(body([]msg{m("user", "a"), m("assistant", "b"), m("user", "e")}, 0), "c", "m")
	if got := find(rows(t, l, db), "c", "").CacheBustSuspects; got != 1 {
		t.Fatalf("suspects=%d", got)
	}
}

// A conversation seen first with a single non-system message must continue
// (not restart) once it has two, via the short-key migration.
func TestShortToFullFingerprintMigration(t *testing.T) {
	l, db := newTest(t)
	m := func(r, c string) msg { return msg{"role": r, "content": c} }
	l.Observe(body([]msg{m("user", "1234567890")}, 0), "c", "m")
	l.Observe(body([]msg{m("user", "1234567890"), m("assistant", "ab"), m("user", "xyz")}, 0), "c", "m")
	r := find(rows(t, l, db), "c", "")
	if r.UserChars != 10+3 || r.AssistantTextChars != 2 || r.CacheBustSuspects != 0 || r.NewMsgs != 1+2 {
		t.Fatalf("%+v", r)
	}
}

func TestConsumersAreIsolated(t *testing.T) {
	l, db := newTest(t)
	m := func(r, c string) msg { return msg{"role": r, "content": c} }
	b := body([]msg{m("user", "same"), m("assistant", "same2"), m("user", "q")}, 0)
	l.Observe(b, "a", "m")
	l.Observe(b, "b", "m")
	rs := rows(t, l, db)
	if find(rs, "a", "").NewMsgs != 3 || find(rs, "b", "").NewMsgs != 3 {
		t.Fatal("consumers shared a conversation")
	}
}

func TestLRUBoundAndTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newConvLRU(3, time.Hour, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		c.put(convEntry{key: [32]byte{byte(i)}, count: i})
	}
	if c.len() != 3 || c.get([32]byte{0}) != nil || c.get([32]byte{4}) == nil {
		t.Fatalf("lru bound broken, len=%d", c.len())
	}
	now = now.Add(2 * time.Hour)
	if c.get([32]byte{4}) != nil {
		t.Fatal("ttl not enforced")
	}
}

func TestCardinalityBounds(t *testing.T) {
	l, db := newTest(t)
	for i := 0; i < maxConsumers+20; i++ {
		l.Observe(body([]msg{{"role": "user", "content": fmt.Sprint("u", i)}}, 0), strings.Repeat("c", 200)+fmt.Sprint(i), "m")
	}
	seen := map[string]bool{}
	for _, r := range rows(t, l, db) {
		seen[r.Consumer] = true
		if len(r.Consumer) > maxLabelLen {
			t.Fatalf("label not truncated: %d", len(r.Consumer))
		}
	}
	if len(seen) > maxConsumers+1 {
		t.Fatalf("consumers=%d", len(seen))
	}
}

func TestDisabledSettingSkips(t *testing.T) {
	l, db := newTest(t)
	if err := db.Settings().Set(context.Background(), SettingEnabled, []byte("false")); err != nil {
		t.Fatal(err)
	}
	l.refreshEnabled(context.Background())
	if l.Observe(body([]msg{{"role": "user", "content": "x"}}, 0), "c", "m") != nil {
		t.Fatal("observed while disabled")
	}
}

func TestParseTimings(t *testing.T) {
	ns := `{"choices":[],"timings":{"cache_n":900,"prompt_n":100,"predicted_n":5}}`
	if c, p, ok := ParseTimings([]byte(ns), false); !ok || c != 900 || p != 100 {
		t.Fatal("non-stream")
	}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"timings\":{\"cache_n\":7,\"prompt_n\":3}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":10},\"choices\":[]}\n\ndata: [DONE]\n\n"
	if c, p, ok := ParseTimings([]byte(sse), true); !ok || c != 7 || p != 3 {
		t.Fatal("stream")
	}
	if _, _, ok := ParseTimings([]byte(`{"usage":{}}`), false); ok {
		t.Fatal("absent must be !ok")
	}
}

// ---- T5 fail-open ----

type badSink struct{ panicky bool }

func (b badSink) Upsert(context.Context, []store.ContextCreationRow) error {
	if b.panicky {
		panic("boom")
	}
	return errors.New("db down")
}
func (badSink) Since(context.Context, int64) ([]store.ContextCreationRow, error) {
	return nil, errors.New("db down")
}
func (badSink) Prune(context.Context, int64) (int64, error) { return 0, errors.New("db down") }

func TestFailOpen(t *testing.T) {
	var nilL *Ledger
	if nilL.Observe([]byte(`{}`), "c", "m") != nil {
		t.Fatal("nil ledger")
	}
	nilL.Flush(context.Background())
	nilL.WriteMetrics(&bytes.Buffer{})
	var nilT *Ticket
	nilT.Reuse(1, 1)

	for _, sink := range []Sink{badSink{}, badSink{panicky: true}} {
		l := New(Config{Sink: sink})
		l.Start(context.Background())
		garbage := [][]byte{nil, []byte("not json"), []byte(`{"messages":"x"}`), []byte(`{"messages":[1,null,"s",{"role":5}]}`), []byte(`{"messages":[{"role":"tool","content":{"a":1}}]}`)}
		for _, g := range garbage {
			l.Observe(g, "c", "m") // must not panic or block
		}
		l.Observe(body([]msg{{"role": "user", "content": "ok"}}, 0), "c", "m")
		l.Flush(context.Background())
		if _, _, err := l.Query(context.Background(), "24h", "tool"); err == nil {
			t.Fatal("expected store error surfaced to the endpoint only")
		}
		l.Close()
	}

	// full queue never blocks the caller
	l := New(Config{}) // not started: queue fills
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueCap*3; i++ {
			l.Observe(body([]msg{{"role": "user", "content": "x"}}, 0), "c", "m")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked on a full queue")
	}
	if l.Stats().Dropped == 0 {
		t.Fatal("expected drops")
	}
}

// Privacy: sentinels in every content field must never reach rows, metrics,
// endpoint output or logs. Only sizes and normalized tool names may appear.
func TestPrivacySentinels(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(nil) })

	l, db := newTest(t)
	const S = "SENTINEL_SECRET_9f3a"
	msgs := []msg{
		{"role": "system", "content": S + "sys"},
		{"role": "user", "content": []any{msg{"type": "text", "text": S + "part"}}},
		{"role": "assistant", "content": S + "a", "reasoning_content": S + "r",
			"tool_calls": []any{call("id1", "do_thing", `{"k":"`+S+`"}`)}},
		{"role": "tool", "tool_call_id": "id1", "name": "do_thing", "content": S + "out"},
		{"role": "user", "content": S + "u2"},
	}
	b := body(msgs, 2)
	b = bytes.Replace(b, []byte(`"model":"m"`), []byte(`"model":"m","metadata":{"x":"`+S+`"}`), 1)
	l.Observe(b, "consumer-a", "m")
	l.Observe(b, "consumer-a", "m")
	for _, r := range rows(t, l, db) {
		j, _ := json.Marshal(r)
		if strings.Contains(string(j), S) {
			t.Fatalf("row leaked: %s", j)
		}
	}
	for _, by := range []string{"tool", "consumer", "model"} {
		res, _, _ := l.Query(context.Background(), "7d", by)
		j, _ := json.Marshal(res)
		if strings.Contains(string(j), S) {
			t.Fatalf("query leaked (%s): %s", by, j)
		}
	}
	var m bytes.Buffer
	l.WriteMetrics(&m)
	if strings.Contains(m.String(), S) || strings.Contains(logs.String(), S) {
		t.Fatal("metrics/log leaked")
	}
}

// T3 (overhead): the hot-path call is an atomic load plus a non-blocking send.
func BenchmarkObserveHotPath(bm *testing.B) {
	l := New(Config{})
	b := body([]msg{{"role": "user", "content": strings.Repeat("x", 100000)}}, 20)
	bm.ResetTimer()
	for i := 0; i < bm.N; i++ {
		l.Observe(b, "c", "m")
	}
}

func TestObserveOverheadUnder1ms(t *testing.T) {
	l := New(Config{})
	b := body([]msg{{"role": "user", "content": strings.Repeat("x", 500000)}}, 20)
	start := time.Now()
	const n = 1000
	for i := 0; i < n; i++ {
		l.Observe(b, "c", "m")
	}
	if per := time.Since(start) / n; per > time.Millisecond {
		t.Fatalf("Observe took %v per call on a 500KB body", per)
	}
}

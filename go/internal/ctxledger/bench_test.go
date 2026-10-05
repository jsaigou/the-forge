// SPDX-License-Identifier: Apache-2.0

package ctxledger

// bench_test.go — T3 of the context-creation plan (WS-F; contract C2/C4,
// invariant I3): the ledger adds <= 1 ms p99 on a 200 KB body and never
// buffers a streaming response.
//
// Reusable HARNESS for WS-N1 plus self-tests that prove the harness can fail.
// Decoupled from N1: everything takes plain function values.
//
//	// N1's own test file, same package:
//	func TestLedgerOverheadBudget(t *testing.T) {
//	    l := newTestLedger(t)
//	    assertObserveOverhead(t, func(b []byte, c, m string) { l.Observe(b, c, m) }, overheadOpts{})          // 200 KB, p99 <= 1 ms
//	}
//	func BenchmarkLedgerObserve200KB(b *testing.B) { benchObserve(b, newTestLedger(b).Observe) }
//	func TestLedgerTapStreams(t *testing.T) {
//	    assertTapStreams(t, func(rc io.ReadCloser) io.ReadCloser { return newResponseTap(rc, ...) })
//	}
//
// For the real a0 path N1 should additionally keep a router-level streaming
// test with the tap installed (router_test.go's
// TestChatCompletions_StreamingNoBuffering pattern); assertTapStreams is the
// package-local, implementation-agnostic version of the same property.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// observeFn is the ledger entry point as the router hook calls it.
type observeFn func(body []byte, consumer, model string)

// overheadOpts configures assertObserveOverhead. Zero value = the contract
// numbers (200 KB, 1 ms p99).
type overheadOpts struct {
	BodyBytes int           // default 200 KiB
	P99Budget time.Duration // default 1 ms (env CTXLEDGER_OVERHEAD_BUDGET overrides, e.g. "3ms" on a loaded CI box)
	Iters     int           // default 400 per variant
}

func (o *overheadOpts) defaults() {
	if o.BodyBytes == 0 {
		o.BodyBytes = 200 << 10
	}
	if o.P99Budget == 0 {
		o.P99Budget = time.Millisecond
	}
	if v := os.Getenv("CTXLEDGER_OVERHEAD_BUDGET"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			o.P99Budget = d
		}
	}
	if o.Iters == 0 {
		o.Iters = 400
	}
}

// overheadStats are per-call latencies for one variant.
type overheadStats struct {
	Variant       string
	P50, P99, Max time.Duration
}

func (s overheadStats) String() string {
	return fmt.Sprintf("%s: p50=%v p99=%v max=%v", s.Variant, s.P50, s.P99, s.Max)
}

// raceBuild reports whether the test binary was built with -race (timing
// budgets are meaningless under the race detector's 5-20x slowdown).
func raceBuild() bool {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range bi.Settings {
		if s.Key == "-race" && s.Value == "true" {
			return true
		}
	}
	return false
}

// overheadBody builds a realistic ~size-byte multi-turn chat body (system +
// user + assistant tool_calls + large tool outputs + tools[] schema),
// deterministic for a given (size, nonce). nonce varies the first user
// message so each distinct nonce is a distinct conversation fingerprint.
func overheadBody(size, nonce int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"model":"gemma4-26b-a4b","stream":true,"temperature":0.2,"tools":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"function","function":{"name":"tool_%d","description":"%s","parameters":{"type":"object","properties":{"path":{"type":"string","description":"a path"},"n":{"type":"integer"}},"required":["path"]}}}`,
			i, strings.Repeat("Does something useful. ", 8))
	}
	b.WriteString(`],"messages":[`)
	fmt.Fprintf(&b, `{"role":"system","content":"You are a coding agent. %s"},`, strings.Repeat("Follow the rules. ", 40))
	fmt.Fprintf(&b, `{"role":"user","content":"task %d: refactor the module 日本語 ✓"}`, nonce)
	turn := 0
	for b.Len() < size {
		fmt.Fprintf(&b, `,{"role":"assistant","content":null,"reasoning_content":"thinking about step %d","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"read","arguments":"{\"path\":\"/src/file%d.go\"}"}}]}`, turn, turn, turn)
		chunk := strings.Repeat("package x // line of source code with <html> & \\\"quotes\\\"\\n", 30)
		fmt.Fprintf(&b, `,{"role":"tool","tool_call_id":"call_%d","content":"%s"}`, turn, chunk)
		turn++
	}
	b.WriteString(`,{"role":"user","content":"continue"}]}`)
	return b.Bytes()
}

func quantiles(d []time.Duration) (p50, p99, max time.Duration) {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration {
		i := int(q * float64(len(s)-1))
		return s[i]
	}
	return at(0.50), at(0.99), s[len(s)-1]
}

// measureObserve times observe over three workloads that exercise the ledger's
// distinct paths: a new conversation every call (full parse + fingerprint +
// LRU insert), the same request repeated (fingerprint hit, empty tail), and a
// conversation growing by appended turns (hit + non-empty tail).
func measureObserve(observe observeFn, o overheadOpts) []overheadStats {
	o.defaults()
	time1 := func(body []byte) time.Duration {
		t0 := time.Now()
		observe(body, "opencode-bench", "gemma4-26b-a4b")
		return time.Since(t0)
	}
	var out []overheadStats
	run := func(name string, bodyFor func(i int) []byte) {
		// warm-up (JIT-free, but warms allocator/caches)
		for i := 0; i < 20; i++ {
			time1(bodyFor(-1 - i))
		}
		ds := make([]time.Duration, 0, o.Iters)
		for i := 0; i < o.Iters; i++ {
			body := bodyFor(i) // construction is outside the timed region
			ds = append(ds, time1(body))
		}
		p50, p99, max := quantiles(ds)
		out = append(out, overheadStats{Variant: name, P50: p50, P99: p99, Max: max})
	}
	run("new_conversation_each_call", func(i int) []byte { return overheadBody(o.BodyBytes, 1000+i) })
	same := overheadBody(o.BodyBytes, 7)
	run("same_request_repeated", func(int) []byte { return same })
	// growing conversation: pre-build a base and a few slightly larger versions
	// sharing the same prefix by construction (same nonce, size grows).
	grow := make([][]byte, 8)
	for k := range grow {
		grow[k] = overheadBody(o.BodyBytes-(len(grow)-k)*3000, 9)
	}
	run("growing_conversation", func(i int) []byte { return grow[((i%len(grow))+len(grow))%len(grow)] })
	return out
}

// assertObserveOverhead fails the test if any workload's p99 exceeds the
// budget. Skipped (with a log) under -race, where timings are meaningless.
func assertObserveOverhead(t testing.TB, observe observeFn, o overheadOpts) {
	t.Helper()
	o.defaults()
	stats := measureObserve(observe, o)
	for _, s := range stats {
		t.Logf("%s (budget p99 <= %v, body %d B)", s, o.P99Budget, o.BodyBytes)
	}
	if raceBuild() {
		t.Log("race detector enabled: p99 budget not enforced")
		return
	}
	for _, s := range stats {
		if s.P99 > o.P99Budget {
			t.Errorf("ledger overhead too high: %s exceeds p99 budget %v", s, o.P99Budget)
		}
	}
}

// benchObserve is the shared benchmark body (go test -bench) for N1.
func benchObserve(b *testing.B, observe observeFn) {
	b.Helper()
	body := overheadBody(200<<10, 1)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		observe(body, "opencode-bench", "gemma4-26b-a4b")
	}
}

// ---- streaming: no buffering -------------------------------------------------

// tapWrap is how N1 installs its response tap around an upstream body (the
// shape of router.newUsageTap's wrapping: ReadCloser in, ReadCloser out).
type tapWrap func(io.ReadCloser) io.ReadCloser

// checkTapStreams verifies two properties of a response-body tap and returns a
// description of the first violation ("" = ok):
//
//  1. latency: SSE chunks emitted with gaps reach the consumer promptly (each
//     chunk is readable before the NEXT one is even produced), so nothing is
//     held back until the stream ends;
//  2. memory: pushing ~64 MiB through the tap keeps live heap growth bounded
//     (a tap that retains the stream, or accumulates it for later parsing,
//     grows with the stream length).
func checkTapStreams(wrap tapWrap) string {
	if why := checkTapLatency(wrap); why != "" {
		return why
	}
	return checkTapMemory(wrap)
}

func checkTapLatency(wrap tapWrap) string {
	const (
		chunks = 6
		gap    = 60 * time.Millisecond
		slack  = 40 * time.Millisecond // delivery must lag the write by less than this
	)
	pr, pw := io.Pipe()
	tapped := wrap(pr)
	var sent [chunks]atomic.Int64
	var lens [chunks]int
	for i := range lens {
		lens[i] = len(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i))
	}
	go func() {
		for i := 0; i < chunks; i++ {
			line := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
			sent[i].Store(time.Now().UnixNano())
			if _, err := io.WriteString(pw, line); err != nil {
				return
			}
			time.Sleep(gap)
		}
		_ = pw.Close()
	}()
	buf := make([]byte, 4096)
	recv, done := 0, 0 // bytes received; chunks fully received before the current read
	for {
		n, err := tapped.Read(buf)
		now := time.Now().UnixNano()
		if n > 0 {
			// The oldest chunk not yet delivered before this read must not have
			// been sitting inside the tap for longer than `slack`.
			if done < chunks {
				if s := sent[done].Load(); s != 0 && s <= now {
					if lag := time.Duration(now - s); lag > slack {
						_ = tapped.Close()
						return fmt.Sprintf("chunk %d reached the consumer %v after it was written (> %v): tap is buffering", done, lag, slack)
					}
				}
			}
			recv += n
			for done < chunks {
				need := 0
				for k := 0; k <= done; k++ {
					need += lens[k]
				}
				if recv < need {
					break
				}
				done++
			}
		}
		if err != nil {
			break
		}
	}
	_ = tapped.Close()
	if done < chunks {
		return fmt.Sprintf("only %d of %d chunks delivered", done, chunks)
	}
	return ""
}

func checkTapMemory(wrap tapWrap) string {
	const (
		total   = 64 << 20
		piece   = 16 << 10
		maxGrow = 24 << 20
	)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	src := io.NopCloser(&sseSource{remaining: total, piece: piece})
	tapped := wrap(src)
	_, _ = io.Copy(io.Discard, tapped)

	// Retention check: with the tap still referenced, force a collection and measure what is LIVE. A tap
	// that accumulates/retains the stream still holds ~total bytes here; a streaming tap holds only its
	// small tail. (The previous version sampled PEAK heap every 5 ms, which also counted uncollected
	// garbage in flight and so depended on GC timing — it failed on an idle machine.) Two GCs: the first
	// can leave freshly released spans for the second to reclaim.
	runtime.GC()
	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)
	runtime.KeepAlive(tapped)
	_ = tapped.Close()

	growth := int64(live.HeapAlloc) - int64(base.HeapAlloc)
	if growth > maxGrow {
		return fmt.Sprintf("%d MiB of heap still live after streaming %d MiB (and a GC): tap retains/accumulates the stream",
			growth>>20, total>>20)
	}
	return ""
}

// sseSource generates `remaining` bytes of SSE-looking data in `piece`-sized reads.
type sseSource struct {
	remaining int
	piece     int
	seq       int
}

func (s *sseSource) Read(p []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	n := s.piece
	if n > len(p) {
		n = len(p)
	}
	if n > s.remaining {
		n = s.remaining
	}
	line := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", s.seq)
	s.seq++
	for i := 0; i < n; i++ {
		p[i] = line[i%len(line)]
	}
	s.remaining -= n
	return n, nil
}

// assertTapStreams is the entry point WS-N1 calls with its real tap
// constructor.
func assertTapStreams(t testing.TB, wrap tapWrap) {
	t.Helper()
	if why := checkTapStreams(wrap); why != "" {
		t.Error(why)
	}
}

// ---- self-tests (the harness must be able to fail) ---------------------------

func identityWrap(rc io.ReadCloser) io.ReadCloser { return rc }

// tailScanWrap models the correct pattern (usageTap): pass bytes straight
// through while keeping only a bounded rolling tail.
type tailScanWrap struct {
	io.ReadCloser
	tail []byte
}

func (w *tailScanWrap) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.tail = append(w.tail, p[:n]...)
	if len(w.tail) > 64<<10 {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-(64<<10):]...)
	}
	return n, err
}

// bufferingWrap models the bug: slurp the whole body, then serve it.
type bufferingWrap struct {
	io.ReadCloser
	r io.Reader
}

func (w *bufferingWrap) Read(p []byte) (int, error) {
	if w.r == nil {
		all, _ := io.ReadAll(w.ReadCloser)
		w.r = bytes.NewReader(all)
	}
	return w.r.Read(p)
}

// retainingWrap passes data through promptly but keeps every byte (the
// "accumulate for later parsing" bug).
type retainingWrap struct {
	io.ReadCloser
	all [][]byte
}

func (w *retainingWrap) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.all = append(w.all, append([]byte(nil), p[:n]...))
	return n, err
}

func TestTapHarness_AcceptsPassthroughAndBoundedTail(t *testing.T) {
	assertTapStreams(t, identityWrap)
	assertTapStreams(t, func(rc io.ReadCloser) io.ReadCloser { return &tailScanWrap{ReadCloser: rc} })
}

func TestTapHarness_RejectsBufferingTap(t *testing.T) {
	if why := checkTapLatency(func(rc io.ReadCloser) io.ReadCloser { return &bufferingWrap{ReadCloser: rc} }); why == "" {
		t.Fatal("harness failed to detect a tap that buffers the whole stream")
	}
}

func TestTapHarness_RejectsRetainingTap(t *testing.T) {
	if why := checkTapMemory(func(rc io.ReadCloser) io.ReadCloser { return &retainingWrap{ReadCloser: rc} }); why == "" {
		t.Fatal("harness failed to detect a tap that retains the whole stream")
	}
}

func TestOverheadHarness_AcceptsNoopRejectsSlow(t *testing.T) {
	noop := func([]byte, string, string) {}
	stats := measureObserve(noop, overheadOpts{Iters: 100})
	if len(stats) != 3 {
		t.Fatalf("want 3 workloads, got %d", len(stats))
	}
	for _, s := range stats {
		if s.P99 > time.Millisecond {
			t.Errorf("noop observe reported %v; harness overhead itself exceeds the budget", s)
		}
	}
	slow := func([]byte, string, string) { time.Sleep(3 * time.Millisecond) }
	slowStats := measureObserve(slow, overheadOpts{Iters: 30})
	for _, s := range slowStats {
		if s.P99 <= time.Millisecond {
			t.Errorf("slow (3ms) observe measured p99=%v; harness cannot detect overhead", s.P99)
		}
	}
}

func TestOverheadBody_IsRealisticAndSized(t *testing.T) {
	b := overheadBody(200<<10, 1)
	if len(b) < 200<<10 || len(b) > 260<<10 {
		t.Fatalf("body size %d not ~200 KiB", len(b))
	}
	if !bytes.Contains(b, []byte(`"reasoning_content"`)) || !bytes.Contains(b, []byte(`"tool_calls"`)) || !bytes.Contains(b, []byte(`"tools":[`)) {
		t.Fatal("body lacks reasoning/tool_calls/tools")
	}
	if !json.Valid(b) {
		t.Fatal("overheadBody is not valid JSON")
	}
	if bytes.Equal(overheadBody(200<<10, 1), overheadBody(200<<10, 2)) {
		t.Fatal("nonce does not vary the body")
	}
}

// ---- wiring against the real WS-N1 ledger ------------------------------------

// TestOverheadHarness_RealLedger holds the real ledger's Observe to the C2
// budget (200 KB body, p99 <= 1 ms) with its worker running and persisting.
func TestOverheadHarness_RealLedger(t *testing.T) {
	l, _ := newRealLedger(t)
	assertObserveOverhead(t, func(b []byte, c, m string) { l.Observe(b, c, m) }, overheadOpts{})
}

// BenchmarkRealLedgerObserve200KB: go test -bench RealLedger ./internal/ctxledger
func BenchmarkRealLedgerObserve200KB(b *testing.B) {
	l, _ := newRealLedger(b)
	benchObserve(b, func(body []byte, c, m string) { l.Observe(body, c, m) })
}

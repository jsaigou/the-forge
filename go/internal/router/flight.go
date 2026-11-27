// SPDX-License-Identifier: Apache-2.0

package router

// flight.go — one record per a0-proxied request: when it arrived, who sent it, which model served it, how it ended.
//
// Two consumers read it. The in-flight gauge (internal/inflight) counts the request in at arrival and out when the handler
// returns; and one usage_events row per request (migration 0105) keeps the interval, so real concurrency per model and per
// consumer can be measured afterwards. The end of a request is the moment the handler returns: for a streamed response that
// is after the last byte has been relayed or the stream has closed, and it holds for a client that went away and for a
// handler that panicked mid-stream (the deferred finish still runs).
//
// What is recorded: a timestamp, a duration, an outcome, a model name, a consumer display label and the token counts the
// upstream itself reported. What is not: the key, the Authorization header, the request or response body.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type flightKey struct{}

func flightFrom(ctx context.Context) *flight {
	f, _ := ctx.Value(flightKey{}).(*flight)
	return f
}

type flight struct {
	s     *Server
	start time.Time

	defModel string // model for a request whose handler never learns one; counted once the caller is authenticated

	mu       sync.Mutex
	consumer string // as counted in the gauge
	model    string // as counted in the gauge
	entered  bool
	waitEnd  time.Time // last upstream dispatch
	external *store.UsageEvent
	prompt   int64
	compl    int64
	tokens   bool
}

// setConsumer is called once the caller is authenticated, so it is also the point where a default-model request is counted
// (an unauthenticated request must neither move the gauge nor leave a row).
func (f *flight) setConsumer(label string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.entered && f.defModel != "" {
		f.consumer = labelOrSmith(label)
		f.model = f.defModel
		f.consumer = f.s.deps.InFlight.Enter(f.model, f.consumer)
		f.entered = true
		return
	}
	if f.entered {
		// Re-key the counted request: the gauge was entered under a provisional label.
		f.s.deps.InFlight.Leave(f.model, f.consumer)
		f.consumer = f.s.deps.InFlight.Enter(f.model, labelOrSmith(label))
		return
	}
	f.consumer = labelOrSmith(label)
}

// labelOrSmith: consumerLabel returns "" only for smith's own traffic.
func labelOrSmith(l string) string {
	if l == "" {
		return "smith"
	}
	return l
}

// setModel counts the request under model (first call) or moves it there (later calls: requested name, then served name).
func (f *flight) setModel(model string) {
	if f == nil || model == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.entered {
		f.model = model
		f.consumer = f.s.deps.InFlight.Enter(model, f.consumerOrUnknown())
		f.entered = true
		return
	}
	if f.model == model {
		return
	}
	f.consumer = f.s.deps.InFlight.Move(f.model, model, f.consumer)
	f.model = model
}

func (f *flight) consumerOrUnknown() string {
	if f.consumer == "" {
		return "unknown"
	}
	return f.consumer
}

// served re-keys the request to the model that actually answered: a remote offering's wire name, else the backend's name.
func (f *flight) served(resolved ResolvedBackend, b *Backend) {
	if f == nil {
		return
	}
	name := b.Name
	if b.Kind == "remote" && resolved.WireModel != "" {
		name = resolved.WireModel
	}
	f.setModel(name)
}

// tapLocalUsage wraps a committed response so the token counts the upstream reported reach the row. Streaming bodies keep
// only usageTap's bounded tail; nothing is buffered ahead of the client.
func (f *flight) tapLocalUsage(resp *http.Response) {
	if f == nil || resp.StatusCode >= 400 || resp.Header.Get("Content-Encoding") != "" {
		return
	}
	ct := resp.Header.Get("Content-Type")
	streaming := strings.Contains(ct, "event-stream")
	if !streaming && !strings.Contains(ct, "json") {
		return
	}
	resp.Body = newUsageTap(resp.Body, streaming, func(buf []byte) {
		if p, c, ok := usageFromBody(buf, streaming); ok {
			f.noteTokens(p, c)
		}
	})
}

func (f *flight) dispatched() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.waitEnd = time.Now()
	f.mu.Unlock()
}

// setExternal parks the billing row of a committed remote response so finish writes it once, with the timing attached.
func (f *flight) setExternal(ev *store.UsageEvent) {
	f.mu.Lock()
	f.external = ev
	f.mu.Unlock()
}

// noteTokens records the usage the upstream reported for a local response (remote responses carry theirs in external).
func (f *flight) noteTokens(prompt, completion int64) {
	f.mu.Lock()
	f.prompt, f.compl, f.tokens = prompt, completion, true
	f.mu.Unlock()
}

// finish counts the request out and writes its row. status is what the handler last wrote to the client (0: nothing),
// clientErr is the original request context's error (client gone), panicked marks a handler that did not return normally.
func (f *flight) finish(status int, clientErr error, panicked bool) {
	end := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entered {
		f.s.deps.InFlight.Leave(f.model, f.consumer)
	}

	outcome := store.OutcomeOK
	switch {
	case errors.Is(clientErr, context.Canceled):
		outcome = store.OutcomeAborted
	case panicked || status >= 400 || status == 0:
		outcome = store.OutcomeError
	}

	// A request that never named a model (rejected as unauthenticated or malformed before routing) leaves no row: it was not
	// served, and rows must not be creatable by an unauthenticated caller.
	if f.s.deps.Usage == nil || !f.entered {
		return
	}
	timing := &store.RequestTiming{
		Start:    f.start,
		Duration: end.Sub(f.start),
		Wait:     -1,
		Outcome:  outcome,
		Consumer: f.consumerOrUnknown(),
	}
	if !f.waitEnd.IsZero() {
		timing.Wait = f.waitEnd.Sub(f.start)
	}

	var ev store.UsageEvent
	if f.external != nil {
		ev = *f.external
	} else {
		ev = store.UsageEvent{TS: end, Kind: "a0_request", Model: f.model}
		if f.tokens {
			ev.PromptTokens, ev.CompletionTokens = f.prompt, f.compl
		} else {
			ev.Unmetered = true
		}
	}
	ev.Timing = timing

	// Best-effort and off the request path's critical section: never fail a request over a usage-recording error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.s.deps.Usage.Record(ctx, ev); err != nil {
		log.Printf("router: usage record (%s): %v", ev.Kind, err)
	}
}

// track wraps a proxied handler in a flight. defaultModel names requests whose handler never learns a model (embeddings).
func (s *Server) track(defaultModel string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Usage == nil && s.deps.InFlight == nil {
			h(w, r)
			return
		}
		f := &flight{s: s, start: time.Now(), defModel: defaultModel}
		clientCtx := r.Context()
		sw := &statusWriter{ResponseWriter: w}
		panicked := true
		defer func() {
			f.finish(sw.status, clientCtx.Err(), panicked)
		}()
		h(sw, r.WithContext(context.WithValue(clientCtx, flightKey{}, f)))
		panicked = false
	}
}

// statusWriter remembers the status code while staying a Flusher (ReverseProxy streams with FlushInterval -1) and exposing
// the wrapped writer to http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// usageFromBody reads the token counts a local upstream reported: the OpenAI usage object when present, else llama.cpp's
// timings (prompt_n counts only the part not served from cache, cache_n the rest). ok=false: nothing reported.
func usageFromBody(buf []byte, streaming bool) (prompt, completion int64, ok bool) {
	var p, c int64
	if streaming {
		p, c, _, ok = parseStreamingUsage(buf)
	} else {
		p, c, _, ok = parseNonStreamingUsage(buf)
	}
	if ok {
		return p, c, true
	}
	payload := buf
	if streaming {
		payload = lastDataFrameWith(buf, "timings")
	}
	var t struct {
		Timings struct {
			PromptN    int64 `json:"prompt_n"`
			CacheN     int64 `json:"cache_n"`
			PredictedN int64 `json:"predicted_n"`
		} `json:"timings"`
	}
	if payload == nil || json.Unmarshal(payload, &t) != nil {
		return 0, 0, false
	}
	if t.Timings.PromptN == 0 && t.Timings.PredictedN == 0 {
		return 0, 0, false
	}
	return t.Timings.PromptN + t.Timings.CacheN, t.Timings.PredictedN, true
}

// lastDataFrameWith returns the payload of the last SSE "data:" line of tail that contains needle.
func lastDataFrameWith(tail []byte, needle string) []byte {
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, needle) {
			continue
		}
		return []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return nil
}

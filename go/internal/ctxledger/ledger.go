// SPDX-License-Identifier: Apache-2.0

// Package ctxledger is the a0 context-creation ledger (CONTRACTS C2/C4,
// WS-N1): observe-only telemetry on how much context each request adds and
// who adds it, plus detection of clients that edit conversation history.
//
// Invariants (principle P0): the ledger never changes a byte of any request
// or response, never stores or logs message content (sizes, counts and
// normalized tool names only), never buffers a stream, and fails open — a
// nil *Ledger, a full queue, a panic or a store error all leave the request
// untouched. The hot-path cost is one atomic load + one non-blocking channel
// send; parsing, hashing and persistence happen on a single worker goroutine.
package ctxledger

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// SettingEnabled is the ungated Settings KV key toggling the ledger (default true).
const SettingEnabled = "context_ledger.enabled"

const (
	queueCap        = 64
	maxBodyBytes    = 16 << 20
	flushEvery      = 30 * time.Second
	settingEvery    = 15 * time.Second
	pruneEvery      = 6 * time.Hour
	retention       = 90 * 24 * time.Hour
	maxConsumers    = 64
	maxModels       = 64
	maxTools        = 200
	maxLabelLen     = 64
	metricsTopTools = 50
)

// Sink persists rollup rows (store.ContextCreationStore implements it).
type Sink interface {
	Upsert(ctx context.Context, rows []store.ContextCreationRow) error
	Since(ctx context.Context, sinceHour int64) ([]store.ContextCreationRow, error)
	Prune(ctx context.Context, beforeHour int64) (int64, error)
}

// Config wires a Ledger. Sink nil -> in-memory counters only (nothing persisted).
type Config struct {
	Sink     Sink
	Settings store.Settings
	Now      func() time.Time
}

type item struct {
	kind     int // 0 observe, 1 reuse, 2 barrier
	body     []byte
	consumer string
	model    string
	ts       time.Time
	ratio    float64
	done     chan struct{}
}

// Ledger is the observe-only creation ledger. All methods are nil-safe.
type Ledger struct {
	cfg     Config
	enabled atomic.Bool
	q       chan item
	lru     *convLRU

	// worker-goroutine state
	consumers, models, tools map[string]bool

	mu      sync.Mutex // guards everything below (read by Query/WriteMetrics)
	pending map[rowKey]*store.ContextCreationRow
	created map[string]map[string]int64 // consumer -> kind -> chars (since start)
	toolOut map[string]int64            // tool -> chars (since start)
	suspect map[string]int64            // consumer -> count (since start)
	stats   Stats

	startOnce sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
}

type rowKey struct {
	hour                  int64
	consumer, model, tool string
}

// Stats are self-health counters (never content).
type Stats struct {
	Observed      int64 `json:"observed"`
	Dropped       int64 `json:"dropped"`
	ParseFailures int64 `json:"parse_failures"`
	Panics        int64 `json:"panics"`
	FlushErrors   int64 `json:"flush_errors"`
}

// SuspectRow is one consumer's cache-bust suspects within the queried window.
type SuspectRow struct {
	Consumer string `json:"consumer"`
	Suspects int64  `json:"suspects"`
	Requests int64  `json:"requests"`
}

// New builds a Ledger (enabled by default). Call Start to run its worker.
func New(cfg Config) *Ledger {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	l := &Ledger{
		cfg:       cfg,
		q:         make(chan item, queueCap),
		lru:       newConvLRU(convLRUMax, convTTL, cfg.Now),
		consumers: map[string]bool{}, models: map[string]bool{}, tools: map[string]bool{},
		pending: map[rowKey]*store.ContextCreationRow{},
		created: map[string]map[string]int64{},
		toolOut: map[string]int64{},
		suspect: map[string]int64{},
		stop:    make(chan struct{}),
	}
	l.enabled.Store(true)
	return l
}

// Ticket correlates a request's observation with its response (for reuse).
type Ticket struct {
	l        *Ledger
	consumer string
	model    string
	ts       time.Time
}

// Observe queues one inbound chat body for measurement. body is read-only and
// retained only until the worker parses it. Never blocks, never panics, never
// touches the request. Returns nil when disabled/over-size/queue-full.
func (l *Ledger) Observe(body []byte, consumer, model string) (t *Ticket) {
	if l == nil || !l.enabled.Load() {
		return nil
	}
	defer func() {
		if recover() != nil {
			atomic.AddInt64(&l.stats.Panics, 1)
			t = nil
		}
	}()
	if len(body) > maxBodyBytes {
		atomic.AddInt64(&l.stats.Dropped, 1)
		return nil
	}
	now := l.cfg.Now()
	select {
	case l.q <- item{kind: 0, body: body, consumer: consumer, model: model, ts: now}:
		return &Ticket{l: l, consumer: consumer, model: model, ts: now}
	default:
		atomic.AddInt64(&l.stats.Dropped, 1)
		return nil
	}
}

// Reuse records cache_n/(cache_n+prompt_n) for the ticket's request. Nil-safe,
// non-blocking, fail-open.
func (t *Ticket) Reuse(cacheN, promptN int64) {
	if t == nil || t.l == nil {
		return
	}
	defer func() { _ = recover() }()
	if cacheN < 0 || promptN < 0 || cacheN+promptN == 0 {
		return
	}
	r := float64(cacheN) / float64(cacheN+promptN)
	select {
	case t.l.q <- item{kind: 1, consumer: t.consumer, model: t.model, ts: t.ts, ratio: r}:
	default:
		atomic.AddInt64(&t.l.stats.Dropped, 1)
	}
}

// Start launches the worker, settings refresher and flush/prune timers; they
// stop (after a final flush) when ctx is cancelled or Close is called.
func (l *Ledger) Start(ctx context.Context) {
	if l == nil {
		return
	}
	l.startOnce.Do(func() {
		l.refreshEnabled(ctx)
		l.wg.Add(1)
		go l.run(ctx)
	})
}

// Close stops the worker after a final flush.
func (l *Ledger) Close() {
	if l == nil {
		return
	}
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	l.wg.Wait()
}

// Stats returns a snapshot of the self-health counters.
func (l *Ledger) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{
		Observed:      atomic.LoadInt64(&l.stats.Observed),
		Dropped:       atomic.LoadInt64(&l.stats.Dropped),
		ParseFailures: atomic.LoadInt64(&l.stats.ParseFailures),
		Panics:        atomic.LoadInt64(&l.stats.Panics),
		FlushErrors:   atomic.LoadInt64(&l.stats.FlushErrors),
	}
}

func (l *Ledger) refreshEnabled(ctx context.Context) {
	if l.cfg.Settings == nil {
		return
	}
	defer func() { _ = recover() }()
	raw, err := l.cfg.Settings.Get(ctx, SettingEnabled)
	if err != nil {
		l.enabled.Store(true) // unset/error -> default true
		return
	}
	switch string(raw) {
	case "false", "0", `"false"`:
		l.enabled.Store(false)
	default:
		l.enabled.Store(true)
	}
}

func (l *Ledger) run(ctx context.Context) {
	defer l.wg.Done()
	flush := time.NewTicker(flushEvery)
	setting := time.NewTicker(settingEvery)
	prune := time.NewTicker(pruneEvery)
	defer flush.Stop()
	defer setting.Stop()
	defer prune.Stop()
	l.pruneOld(ctx)
	for {
		select {
		case it := <-l.q:
			l.handle(it)
			if it.kind == 2 {
				l.persist(context.Background())
				close(it.done)
			}
		case <-flush.C:
			l.persist(ctx)
		case <-setting.C:
			l.refreshEnabled(ctx)
		case <-prune.C:
			l.pruneOld(ctx)
		case <-ctx.Done():
			l.drain()
			l.persist(context.Background())
			return
		case <-l.stop:
			l.drain()
			l.persist(context.Background())
			return
		}
	}
}

func (l *Ledger) drain() {
	for {
		select {
		case it := <-l.q:
			l.handle(it)
			if it.kind == 2 {
				close(it.done)
			}
		default:
			return
		}
	}
}

// Flush blocks until everything queued so far has been measured and persisted
// (used before queries and in tests). Fail-open: returns when ctx is done.
func (l *Ledger) Flush(ctx context.Context) {
	if l == nil {
		return
	}
	done := make(chan struct{})
	select {
	case l.q <- item{kind: 2, done: done}:
	case <-ctx.Done():
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-l.stop:
	}
}

func (l *Ledger) handle(it item) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&l.stats.Panics, 1)
			log.Printf("ctxledger: recovered panic in worker (content-free)")
		}
	}()
	switch it.kind {
	case 0:
		l.process(it)
	case 1:
		l.recordReuse(it)
	}
}

// bound maps a label into a bounded sticky set; overflow -> "other".
func bound(set map[string]bool, s string, max int) string {
	if s == "" {
		s = "unknown"
	}
	if len(s) > maxLabelLen {
		s = s[:maxLabelLen]
	}
	if set[s] {
		return s
	}
	if len(set) >= max {
		return "other"
	}
	set[s] = true
	return s
}

func hourOf(t time.Time) int64 { return t.UTC().Truncate(time.Hour).Unix() }

func (l *Ledger) row(hour int64, consumer, model, tool string) *store.ContextCreationRow {
	k := rowKey{hour, consumer, model, tool}
	r := l.pending[k]
	if r == nil {
		r = &store.ContextCreationRow{Hour: hour, Consumer: consumer, Model: model, Tool: tool,
			ResultHist: store.Hist{}, SchemaHist: store.Hist{}, ReuseHist: store.Hist{}}
		l.pending[k] = r
	}
	return r
}

func (l *Ledger) process(it item) {
	pb, ok := parseBody(it.body)
	if !ok {
		atomic.AddInt64(&l.stats.ParseFailures, 1)
		return
	}
	consumer := bound(l.consumers, it.consumer, maxConsumers)
	model := bound(l.models, it.model, maxModels)
	msgs := pb.Messages
	hs := hashMessages(msgs)
	n := len(msgs)

	full, short, hasFull, hasShort := fingerprints(msgs, hs, consumer)
	var entry *convEntry
	var found [32]byte
	if hasFull {
		if entry = l.lru.get(full); entry != nil {
			found = full
		}
	}
	if entry == nil && hasShort {
		if entry = l.lru.get(short); entry != nil {
			found = short
		}
	}

	newStart, suspect := 0, false
	_, fullSum, _ := prefixSums(hs, 0)
	if entry != nil {
		if atSum, _, ok := prefixSums(hs, entry.count); ok && atSum == entry.prefix {
			newStart = entry.count
		} else {
			suspect = true // history edited/truncated by the client: whole body is new
		}
	}

	created, total := measure(msgs, newStart)
	schemaChars, toolCount := schemaStats(pb.Tools)

	// persist the conversation state (digests/counts only)
	if hasFull {
		l.lru.put(convEntry{key: full, count: n, totalChars: total, prefix: fullSum})
		if entry != nil && found != full {
			l.lru.del(found) // migrated short -> full key
		}
	} else if hasShort {
		l.lru.put(convEntry{key: short, count: n, totalChars: total, prefix: fullSum, short: true})
	}

	atomic.AddInt64(&l.stats.Observed, 1)
	hour := hourOf(it.ts)

	l.mu.Lock()
	defer l.mu.Unlock()
	rr := l.row(hour, consumer, model, "")
	rr.Requests++
	rr.NewMsgs += int64(n - newStart)
	rr.UserChars += created.User
	rr.AssistantTextChars += created.AssistantText
	rr.AssistantReasoningChars += created.AssistantReasoning
	rr.ToolArgsChars += created.ToolArgs
	rr.OtherChars += created.Other
	if toolCount > 0 || schemaChars > 0 {
		rr.SchemaRequests++
		rr.SchemaCharsSum += schemaChars
		rr.SchemaHist[SizeBucket(schemaChars)]++
	}
	if suspect {
		rr.CacheBustSuspects++
		l.suspect[consumer]++
	}
	cm := l.created[consumer]
	if cm == nil {
		cm = map[string]int64{}
		l.created[consumer] = cm
	}
	cm["user"] += created.User
	cm["assistant_text"] += created.AssistantText
	cm["assistant_reasoning"] += created.AssistantReasoning
	cm["tool_args"] += created.ToolArgs
	cm["other"] += created.Other

	for name, ts := range created.Tools {
		tool := bound(l.tools, name, maxTools)
		tr := l.row(hour, consumer, model, tool)
		tr.Requests++
		tr.ToolResults += ts.Results
		tr.ToolOutputChars += ts.Chars
		tr.Big8k += ts.Big8k
		tr.Big24k += ts.Big24k
		if ts.Max > tr.MaxResult {
			tr.MaxResult = ts.Max
		}
		tr.ResultHist.Merge(ts.Hist)
		cm["tool_output"] += ts.Chars
		l.toolOut[tool] += ts.Chars
	}
}

func (l *Ledger) recordReuse(it item) {
	consumer := bound(l.consumers, it.consumer, maxConsumers)
	model := bound(l.models, it.model, maxModels)
	l.mu.Lock()
	defer l.mu.Unlock()
	rr := l.row(hourOf(it.ts), consumer, model, "")
	rr.ReuseN++
	rr.ReuseHist[ReuseBucket(it.ratio)]++
}

// persist swaps out the pending rollups and upserts them; on a store error
// the rows are merged back (bounded by distinct keys) for the next attempt.
func (l *Ledger) persist(ctx context.Context) {
	if l.cfg.Sink == nil {
		return
	}
	l.mu.Lock()
	batch := make([]store.ContextCreationRow, 0, len(l.pending))
	for _, r := range l.pending {
		batch = append(batch, *r)
	}
	l.pending = map[rowKey]*store.ContextCreationRow{}
	l.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				atomic.AddInt64(&l.stats.Panics, 1)
				err = context.Canceled
			}
		}()
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		err = l.cfg.Sink.Upsert(cctx, batch)
	}()
	if err != nil {
		atomic.AddInt64(&l.stats.FlushErrors, 1)
		log.Printf("ctxledger: flush failed (%d rows kept for retry): %v", len(batch), err)
		l.mu.Lock()
		for _, b := range batch {
			dst := l.row(b.Hour, b.Consumer, b.Model, b.Tool)
			dst.Merge(b)
		}
		l.mu.Unlock()
	}
}

func (l *Ledger) pruneOld(ctx context.Context) {
	if l.cfg.Sink == nil {
		return
	}
	defer func() { _ = recover() }()
	if _, err := l.cfg.Sink.Prune(ctx, hourOf(l.cfg.Now().Add(-retention))); err != nil {
		log.Printf("ctxledger: prune failed: %v", err)
	}
}

// ---- query (C2 endpoint) ----

// Row is one aggregated line of GET /api/v1/context/creation.
type Row struct {
	Key            string           `json:"key"`
	Requests       int64            `json:"requests"`
	CreatedChars   int64            `json:"created_chars"`
	Share          float64          `json:"share"`
	P50ResultChars float64          `json:"p50_result_chars"`
	P90ResultChars float64          `json:"p90_result_chars"`
	MaxResultChars int64            `json:"max_result_chars"`
	Big8k          int64            `json:"big_results_8k"`
	Big24k         int64            `json:"big_results_24k"`
	Kinds          map[string]int64 `json:"kinds,omitempty"`
}

// Result is the endpoint payload.
type Result struct {
	Window            string   `json:"window"`
	By                string   `json:"by"`
	Rows              []Row    `json:"rows"`
	TotalRequests     int64    `json:"total_requests"`
	TotalCreatedChars int64    `json:"total_created_chars"`
	SchemaCharsP50    *float64 `json:"schema_chars_p50"`
	ReuseP50          *float64 `json:"reuse_p50"`
	ReuseSamples      int64    `json:"reuse_samples"`
	CacheBustSuspects int64    `json:"cache_bust_suspects"`
	// SuspectsByConsumer lists consumers with >=1 suspect in the window
	// (descending); same bounded consumer labels as the rollups.
	SuspectsByConsumer []SuspectRow `json:"suspects_by_consumer"`
	Approximate        string       `json:"approximate"`
	Health             Stats        `json:"ledger"`
}

// Windows maps the accepted ?window= values.
var Windows = map[string]time.Duration{
	"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
}

// Query flushes pending data and aggregates the window by "tool", "consumer"
// or "model". ok=false for an invalid window/by.
func (l *Ledger) Query(ctx context.Context, window, by string) (Result, bool, error) {
	d, okw := Windows[window]
	if !okw || (by != "tool" && by != "consumer" && by != "model") {
		return Result{}, false, nil
	}
	res := Result{Window: window, By: by, Rows: []Row{}, SuspectsByConsumer: []SuspectRow{}, Health: l.Stats(),
		Approximate: "percentiles are estimated from log-scale buckets (~±10%); sizes are characters"}
	if l == nil || l.cfg.Sink == nil {
		return res, true, nil
	}
	l.Flush(ctx)
	rows, err := l.cfg.Sink.Since(ctx, hourOf(l.cfg.Now().Add(-d)))
	if err != nil {
		return res, true, err
	}
	type agg struct {
		Row
		hist store.Hist
	}
	groups := map[string]*agg{}
	schemaHist, reuseHist := store.Hist{}, store.Hist{}
	susp := map[string]*SuspectRow{}
	reqs := map[string]int64{}
	for _, r := range rows {
		var key string
		switch by {
		case "tool":
			key = r.Tool
		case "consumer":
			key = r.Consumer
		default:
			key = r.Model
		}
		if by == "tool" && r.Tool == "" {
			// request-level row: contributes only to the global figures below
		} else {
			g := groups[key]
			if g == nil {
				g = &agg{Row: Row{Key: key, Kinds: map[string]int64{}}, hist: store.Hist{}}
				groups[key] = g
			}
			if r.Tool == "" {
				g.Requests += r.Requests
				g.Kinds["user"] += r.UserChars
				g.Kinds["assistant_text"] += r.AssistantTextChars
				g.Kinds["assistant_reasoning"] += r.AssistantReasoningChars
				g.Kinds["tool_args"] += r.ToolArgsChars
				g.Kinds["other"] += r.OtherChars
				g.CreatedChars += r.UserChars + r.AssistantTextChars + r.AssistantReasoningChars + r.ToolArgsChars + r.OtherChars
			} else {
				if by == "tool" {
					g.Requests += r.Requests
				}
				g.Kinds["tool_output"] += r.ToolOutputChars
				g.CreatedChars += r.ToolOutputChars
				g.Big8k += r.Big8k
				g.Big24k += r.Big24k
				if r.MaxResult > g.MaxResultChars {
					g.MaxResultChars = r.MaxResult
				}
				g.hist.Merge(r.ResultHist)
			}
		}
		if r.Tool == "" {
			res.TotalRequests += r.Requests
			res.CacheBustSuspects += r.CacheBustSuspects
			reqs[r.Consumer] += r.Requests
			if r.CacheBustSuspects > 0 {
				sr := susp[r.Consumer]
				if sr == nil {
					sr = &SuspectRow{Consumer: r.Consumer}
					susp[r.Consumer] = sr
				}
				sr.Suspects += r.CacheBustSuspects
			}
			schemaHist.Merge(r.SchemaHist)
			reuseHist.Merge(r.ReuseHist)
			res.ReuseSamples += r.ReuseN
		}
		res.TotalCreatedChars += r.UserChars + r.AssistantTextChars + r.AssistantReasoningChars +
			r.ToolArgsChars + r.OtherChars + r.ToolOutputChars
	}
	for _, g := range groups {
		if v, ok := Percentile(g.hist, 0.5, SizeBucketValue); ok {
			g.P50ResultChars = v
		}
		if v, ok := Percentile(g.hist, 0.9, SizeBucketValue); ok {
			g.P90ResultChars = v
		}
		if res.TotalCreatedChars > 0 {
			g.Share = float64(g.CreatedChars) / float64(res.TotalCreatedChars)
		}
		res.Rows = append(res.Rows, g.Row)
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		if res.Rows[i].CreatedChars != res.Rows[j].CreatedChars {
			return res.Rows[i].CreatedChars > res.Rows[j].CreatedChars
		}
		return res.Rows[i].Key < res.Rows[j].Key
	})
	for _, sr := range susp {
		sr.Requests = reqs[sr.Consumer]
		res.SuspectsByConsumer = append(res.SuspectsByConsumer, *sr)
	}
	sort.Slice(res.SuspectsByConsumer, func(i, j int) bool {
		a, b := res.SuspectsByConsumer[i], res.SuspectsByConsumer[j]
		if a.Suspects != b.Suspects {
			return a.Suspects > b.Suspects
		}
		return a.Consumer < b.Consumer
	})
	if v, ok := Percentile(schemaHist, 0.5, SizeBucketValue); ok {
		res.SchemaCharsP50 = &v
	}
	if v, ok := Percentile(reuseHist, 0.5, ReuseBucketValue); ok {
		res.ReuseP50 = &v
	}
	return res, true, nil
}

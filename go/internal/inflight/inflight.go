// SPDX-License-Identifier: Apache-2.0

// Package inflight counts the requests a0 is serving right now, per model and per
// consumer, and remembers the peak over a rolling window. It holds only counts and
// labels (a model name and a consumer label): never a key, a prompt or a response.
//
// The gauge answers "how many at once, lately" for the live status surface. The
// durable answer (percentiles over days, from stored intervals) lives in
// usage_events and GET /api/v1/usage/concurrency.
package inflight

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Window is how far back Peak looks.
	Window = 15 * time.Minute
	bucket = time.Minute
	// maxCells bounds label cardinality (a consumer label can be a remote address).
	// Past it, new consumers are counted under OtherConsumer.
	maxCells = 512
	// OtherConsumer is the label new consumers get once maxCells is reached.
	OtherConsumer = "other"
	// ModelTotal is the Consumer value of a per-model total row.
	ModelTotal = ""
)

type key struct{ model, consumer string }

type cell struct {
	cur     int
	touched time.Time     // last Enter or Leave on this cell (zero if never)
	buckets map[int64]int // minute (unix/60) -> highest in-flight count seen in it
}

// Gauge is safe for concurrent use. The zero value is not usable; call New.
type Gauge struct {
	mu    sync.Mutex
	cells map[key]*cell
	now   func() time.Time
}

// New returns an empty gauge.
func New() *Gauge { return &Gauge{cells: map[key]*cell{}, now: time.Now} }

// SetClock replaces the clock (tests only).
func (g *Gauge) SetClock(now func() time.Time) {
	g.mu.Lock()
	g.now = now
	g.mu.Unlock()
}

func (g *Gauge) minute() int64 { return g.now().Unix() / int64(bucket/time.Second) }

func (g *Gauge) cellFor(k key) *cell {
	c := g.cells[k]
	if c == nil {
		c = &cell{buckets: map[int64]int{}}
		g.cells[k] = c
	}
	return c
}

func (c *cell) note(m int64) {
	if c.cur > c.buckets[m] {
		c.buckets[m] = c.cur
	}
}

func (g *Gauge) enterLocked(model, consumer string) string {
	if _, ok := g.cells[key{model, consumer}]; !ok && len(g.cells) >= maxCells {
		consumer = OtherConsumer
	}
	m := g.minute()
	now := g.now()
	for _, k := range []key{{model, consumer}, {model, ModelTotal}} {
		c := g.cellFor(k)
		c.touched = now
		c.note(m) // the level before this request, so a count held across a minute boundary carries over
		c.cur++
		c.note(m)
	}
	return consumer
}

func (g *Gauge) leaveLocked(model, consumer string) {
	m := g.minute()
	for _, k := range []key{{model, consumer}, {model, ModelTotal}} {
		c := g.cells[k]
		if c == nil || c.cur == 0 {
			continue
		}
		c.touched = g.now()
		c.note(m) // the level before leaving belongs to this minute
		c.cur--
	}
}

// Enter counts one request in. It returns the consumer label actually used, which
// the caller must hand back to Leave and Move (it differs from the argument only
// when the cardinality bound was hit).
func (g *Gauge) Enter(model, consumer string) string {
	if g == nil {
		return consumer
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enterLocked(model, consumer)
}

// Leave counts one request out.
func (g *Gauge) Leave(model, consumer string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leaveLocked(model, consumer)
}

// Move re-labels an in-flight request from one model to another (a request is
// admitted under the model that was asked for and re-keyed once the served model is
// known). It returns the consumer label in use under the new model.
func (g *Gauge) Move(from, to, consumer string) string {
	if g == nil || from == to {
		return consumer
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.leaveLocked(from, consumer)
	return g.enterLocked(to, consumer)
}

// ModelActivity reports how many requests a0 is serving for model right now and when a request for it last entered or left (zero if none has
// since the gauge started). It reads the per-model total cell; a nil gauge reports nothing in flight and no activity.
func (g *Gauge) ModelActivity(model string) (inFlight int, lastActive time.Time) {
	if g == nil {
		return 0, time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.cells[key{model, ModelTotal}]; c != nil {
		return c.cur, c.touched
	}
	return 0, time.Time{}
}

// Row is one gauge reading. Consumer "" is the model total.
type Row struct {
	Model    string `json:"model"`
	Consumer string `json:"consumer,omitempty"`
	InFlight int    `json:"in_flight"`
	// Peak is the highest in-flight count seen over the last peak_window_s seconds.
	Peak int `json:"peak"`
}

// Snapshot is the gauge as exposed on the status surface.
type Snapshot struct {
	PeakWindowS int   `json:"peak_window_s"`
	Rows        []Row `json:"rows"`
}

// Snapshot returns every cell that is non-zero now or had a non-zero peak inside the
// window, sorted by model then consumer (model total first). Idle cells past the
// window are dropped here, which is what keeps the map small.
func (g *Gauge) Snapshot() Snapshot {
	out := Snapshot{PeakWindowS: int(Window / time.Second), Rows: []Row{}}
	if g == nil {
		return out
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	oldest := g.minute() - int64(Window/bucket) + 1
	for k, c := range g.cells {
		peak := c.cur
		for bm, p := range c.buckets {
			if bm < oldest {
				delete(c.buckets, bm)
				continue
			}
			if p > peak {
				peak = p
			}
		}
		if c.cur == 0 && peak == 0 {
			delete(g.cells, k)
			continue
		}
		out.Rows = append(out.Rows, Row{Model: k.model, Consumer: k.consumer, InFlight: c.cur, Peak: peak})
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].Model != out.Rows[j].Model {
			return out.Rows[i].Model < out.Rows[j].Model
		}
		return out.Rows[i].Consumer < out.Rows[j].Consumer
	})
	return out
}

func promEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// WriteMetrics renders the gauge in Prometheus text format (consumer="" is the
// model total).
func (g *Gauge) WriteMetrics(w io.Writer) {
	snap := g.Snapshot()
	_, _ = fmt.Fprintln(w, "# TYPE a0_requests_in_flight gauge")
	for _, r := range snap.Rows {
		_, _ = fmt.Fprintf(w, "a0_requests_in_flight{model=\"%s\",consumer=\"%s\"} %d\n", promEscape(r.Model), promEscape(r.Consumer), r.InFlight)
	}
	_, _ = fmt.Fprintf(w, "# HELP a0_requests_in_flight_peak highest in-flight count over the last %d seconds\n# TYPE a0_requests_in_flight_peak gauge\n", snap.PeakWindowS)
	for _, r := range snap.Rows {
		_, _ = fmt.Fprintf(w, "a0_requests_in_flight_peak{model=\"%s\",consumer=\"%s\"} %d\n", promEscape(r.Model), promEscape(r.Consumer), r.Peak)
	}
}

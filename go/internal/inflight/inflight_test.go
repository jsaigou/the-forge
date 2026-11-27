// SPDX-License-Identifier: Apache-2.0

package inflight

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func find(t *testing.T, s Snapshot, model, consumer string) Row {
	t.Helper()
	for _, r := range s.Rows {
		if r.Model == model && r.Consumer == consumer {
			return r
		}
	}
	t.Fatalf("no row %q/%q in %+v", model, consumer, s.Rows)
	return Row{}
}

func TestEnterLeaveCountsPerModelAndConsumer(t *testing.T) {
	g := New()
	a := g.Enter("m1", "opencode")
	b := g.Enter("m1", "librechat")
	c := g.Enter("m1", "opencode")
	s := g.Snapshot()
	if r := find(t, s, "m1", ModelTotal); r.InFlight != 3 || r.Peak != 3 {
		t.Errorf("model total = %+v, want 3/3", r)
	}
	if r := find(t, s, "m1", "opencode"); r.InFlight != 2 {
		t.Errorf("opencode = %+v, want 2", r)
	}
	g.Leave("m1", a)
	g.Leave("m1", b)
	g.Leave("m1", c)
	s = g.Snapshot()
	if r := find(t, s, "m1", ModelTotal); r.InFlight != 0 || r.Peak != 3 {
		t.Errorf("after leaving: %+v, want 0 in flight and the peak of 3 kept", r)
	}
}

func TestPeakRollsOffAfterTheWindow(t *testing.T) {
	g := New()
	now := time.Unix(1_700_000_000, 0)
	g.SetClock(func() time.Time { return now })
	for i := 0; i < 4; i++ {
		g.Enter("m", "c")
	}
	for i := 0; i < 4; i++ {
		g.Leave("m", "c")
	}
	if r := find(t, g.Snapshot(), "m", ModelTotal); r.Peak != 4 {
		t.Fatalf("peak = %d, want 4", r.Peak)
	}
	now = now.Add(Window + 2*time.Minute)
	if rows := g.Snapshot().Rows; len(rows) != 0 {
		t.Errorf("idle cells past the window must be dropped, got %+v", rows)
	}
}

func TestLongHeldRequestKeepsItsPeakAcrossMinutes(t *testing.T) {
	g := New()
	now := time.Unix(1_700_000_000, 0)
	g.SetClock(func() time.Time { return now })
	g.Enter("m", "c")
	g.Enter("m", "c")
	now = now.Add(10 * time.Minute)
	if r := find(t, g.Snapshot(), "m", ModelTotal); r.InFlight != 2 || r.Peak != 2 {
		t.Errorf("held across minutes: %+v, want 2/2", r)
	}
	g.Leave("m", "c")
	now = now.Add(time.Minute)
	if r := find(t, g.Snapshot(), "m", ModelTotal); r.InFlight != 1 || r.Peak != 2 {
		t.Errorf("after one leaves: %+v, want 1 in flight, peak 2 still inside the window", r)
	}
}

func TestMoveRekeysTheModel(t *testing.T) {
	g := New()
	c := g.Enter("asked", "opencode")
	c = g.Move("asked", "served", c)
	s := g.Snapshot()
	if r := find(t, s, "asked", ModelTotal); r.InFlight != 0 {
		t.Errorf("old model still counts: %+v", r)
	}
	if r := find(t, s, "served", "opencode"); r.InFlight != 1 {
		t.Errorf("new model = %+v, want 1", r)
	}
	g.Leave("served", c)
	if r := find(t, g.Snapshot(), "served", ModelTotal); r.InFlight != 0 {
		t.Errorf("leave after move: %+v", r)
	}
}

func TestConsumerCardinalityIsBounded(t *testing.T) {
	g := New()
	var used []string
	for i := 0; i < maxCells+50; i++ {
		used = append(used, g.Enter("m", fmt.Sprintf("10.0.%d.%d", i/250, i%250)))
	}
	if n := len(g.Snapshot().Rows); n > maxCells+2 {
		t.Errorf("%d rows, want the bound (%d) respected", n, maxCells)
	}
	other := 0
	for _, u := range used {
		if u == OtherConsumer {
			other++
		}
	}
	if other == 0 {
		t.Error("overflow consumers must be folded into 'other'")
	}
	for i, u := range used {
		g.Leave("m", u)
		_ = i
	}
	if r := find(t, g.Snapshot(), "m", ModelTotal); r.InFlight != 0 {
		t.Errorf("total after all leave: %+v", r)
	}
}

func TestConcurrentUseReturnsToZero(t *testing.T) {
	g := New()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := g.Enter("m", fmt.Sprintf("c%d", i%5))
			if i%7 == 0 {
				c = g.Move("m", "m2", c)
				g.Leave("m2", c)
				return
			}
			g.Leave("m", c)
		}(i)
	}
	wg.Wait()
	for _, r := range g.Snapshot().Rows {
		if r.InFlight != 0 {
			t.Errorf("not zero: %+v", r)
		}
	}
}

func TestNilGaugeIsInert(t *testing.T) {
	var g *Gauge
	c := g.Enter("m", "c")
	g.Leave("m", c)
	if c != "c" || len(g.Snapshot().Rows) != 0 {
		t.Errorf("nil gauge must be a no-op")
	}
}

func TestWriteMetricsEscapesLabels(t *testing.T) {
	g := New()
	g.Enter("m", `evil"label`+"\n")
	var b bytes.Buffer
	g.WriteMetrics(&b)
	out := b.String()
	if !strings.Contains(out, `a0_requests_in_flight{model="m",consumer="evil\"label\n"} 1`) {
		t.Errorf("metrics = %q", out)
	}
	if !strings.Contains(out, "# TYPE a0_requests_in_flight_peak gauge") {
		t.Errorf("peak family missing: %q", out)
	}
}

func TestModelActivityReportsInFlightAndLastTouch(t *testing.T) {
	g := New()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	g.SetClock(func() time.Time { return now })
	if n, last := g.ModelActivity("m"); n != 0 || !last.IsZero() {
		t.Fatalf("empty gauge = %d %v", n, last)
	}
	g.Enter("m", "c1")
	g.Enter("m", "c2")
	now = now.Add(30 * time.Second)
	g.Leave("m", "c1")
	if n, last := g.ModelActivity("m"); n != 1 || !last.Equal(now) {
		t.Fatalf("after leave = %d %v, want 1 at %v", n, last, now)
	}
	if n, last := g.ModelActivity("other"); n != 0 || !last.IsZero() {
		t.Fatalf("other model = %d %v", n, last)
	}
	var nilGauge *Gauge
	if n, last := nilGauge.ModelActivity("m"); n != 0 || !last.IsZero() {
		t.Fatalf("nil gauge = %d %v", n, last)
	}
}

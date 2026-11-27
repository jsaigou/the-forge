// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/inflight"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

func iv(start, end int64) store.Interval { return store.Interval{StartMS: start, EndMS: end, Outcome: store.OutcomeOK} }

func TestConcurrencyStatsSweepLine(t *testing.T) {
	cases := []struct {
		name       string
		ivs        []store.Interval
		max        int64
		p50        int64
		overlapped int
		busyMS     int64
	}{
		{"empty", nil, 0, 0, 0, 0},
		{"single", []store.Interval{iv(0, 1000)}, 1, 1, 0, 1000},
		{"disjoint", []store.Interval{iv(0, 100), iv(200, 300)}, 1, 1, 0, 200},
		// One request ends the instant the next starts: back to back, not concurrent.
		{"touching", []store.Interval{iv(0, 100), iv(100, 200)}, 1, 1, 0, 200},
		{"partial overlap", []store.Interval{iv(0, 100), iv(50, 150)}, 2, 1, 2, 150},
		{"identical", []store.Interval{iv(10, 110), iv(10, 110), iv(10, 110)}, 3, 3, 3, 100},
		{"nested", []store.Interval{iv(0, 1000), iv(100, 200)}, 2, 1, 2, 1000},
		// A request shorter than 1 ms counts as 1 ms: two at the same instant overlap, one at another's end does not.
		{"identical zero-length", []store.Interval{iv(5, 5), iv(5, 5)}, 2, 2, 2, 1},
		{"zero-length inside", []store.Interval{iv(0, 100), iv(50, 50)}, 2, 1, 2, 100},
		{"zero-length at an end", []store.Interval{iv(0, 100), iv(100, 100)}, 1, 1, 0, 101},
		{"zero-length at a start", []store.Interval{iv(100, 200), iv(100, 100)}, 2, 1, 2, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, busy, overlapped := concurrencyStats(c.ivs)
			if in.Max != c.max || in.P50 != c.p50 || overlapped != c.overlapped || busy.Milliseconds() != c.busyMS {
				t.Errorf("max=%d p50=%d overlapped=%d busy=%dms, want %d/%d/%d/%dms",
					in.Max, in.P50, overlapped, busy.Milliseconds(), c.max, c.p50, c.overlapped, c.busyMS)
			}
		})
	}
}

// TestConcurrencyStatsTimeWeighting: one request spans 0-1000 ms and three more run together inside 100-200 ms. The level is
// 4 for 100 of the 1000 busy ms, so p90 is still 1 and p99 is 4.
func TestConcurrencyStatsTimeWeighting(t *testing.T) {
	in, busy, overlapped := concurrencyStats([]store.Interval{iv(0, 1000), iv(100, 200), iv(100, 200), iv(100, 200)})
	if in.P50 != 1 || in.P90 != 1 || in.P99 != 4 || in.Max != 4 {
		t.Errorf("in-flight = %+v, want p50=1 p90=1 p99=4 max=4", in)
	}
	if busy != time.Second || overlapped != 4 {
		t.Errorf("busy=%v overlapped=%d, want 1s and 4", busy, overlapped)
	}
}

func TestConcurrencyOverlapIgnoresInputOrder(t *testing.T) {
	// A long early request overlaps a late one through a chain of short ones.
	ivs := []store.Interval{iv(500, 600), iv(0, 1000), iv(2000, 2100), iv(0, 10)}
	_, _, overlapped := concurrencyStats(ivs)
	if overlapped != 3 { // 500-600, 0-1000 and 0-10 overlap; 2000-2100 is alone
		t.Errorf("overlapped = %d, want 3", overlapped)
	}
}

func TestBuildConcurrencyTokensAndOutcomes(t *testing.T) {
	p := func(v int64) *int64 { return &v }
	set := store.IntervalSet{ExcludedUntimed: 7, Intervals: []store.Interval{
		{StartMS: 0, EndMS: 10, PromptTokens: p(100), CompletionTokens: p(10), Outcome: store.OutcomeOK},
		{StartMS: 0, EndMS: 10, PromptTokens: p(200), CompletionTokens: p(20), Outcome: store.OutcomeOK},
		{StartMS: 0, EndMS: 10, PromptTokens: p(300), CompletionTokens: p(30), Outcome: store.OutcomeError},
		{StartMS: 50, EndMS: 60, Outcome: store.OutcomeAborted}, // no usage reported
	}}
	r := buildConcurrency(set)
	if r.Requests != 4 || r.ExcludedUntimed != 7 || r.TokenSamples != 3 {
		t.Errorf("requests=%d excluded=%d samples=%d, want 4/7/3", r.Requests, r.ExcludedUntimed, r.TokenSamples)
	}
	if r.Outcomes["ok"] != 2 || r.Outcomes["error"] != 1 || r.Outcomes["aborted"] != 1 {
		t.Errorf("outcomes = %v", r.Outcomes)
	}
	if r.PromptTokens.P50 != 200 || r.PromptTokens.Max != 300 || r.CompletionTokens.P50 != 20 {
		t.Errorf("token quantiles = %+v / %+v", r.PromptTokens, r.CompletionTokens)
	}
	if r.OverlappedRequests != 3 || r.OverlapFraction != 0.75 {
		t.Errorf("overlap = %d (%v), want 3 (0.75)", r.OverlappedRequests, r.OverlapFraction)
	}
}

func concurrencyServer(t *testing.T, role authz.Role, db *store.DB, g *inflight.Gauge) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	d := Deps{
		Snapshots: collector.NewStatic(nil), Engine: &engine.Stub{}, Sched: &sched.Stub{},
		Auth:   &stubAuth{identity: authz.Identity{Name: "k", Role: role}},
		Events: events, Publish: events, Config: func() *config.Config { return cfg },
		Hostname: "h", InFlight: g,
	}
	if db != nil {
		d.Usage = db.Usage()
	}
	s := New(d)
	t.Cleanup(func() { s.Close() })
	return s
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUsageConcurrencyAuth(t *testing.T) {
	db := openDB(t)
	viewer := concurrencyServer(t, authz.RoleViewer, db, nil)
	if w := do(t, viewer, authedRequest("GET", "/api/v1/usage/concurrency", nil)); w.Code != http.StatusForbidden {
		t.Errorf("viewer status = %d, want 403", w.Code)
	}
	if w := do(t, viewer, authedRequest("GET", "/api/v1/a0/in-flight/metrics", nil)); w.Code != http.StatusForbidden {
		t.Errorf("viewer metrics status = %d, want 403", w.Code)
	}
	anon := concurrencyServer(t, authz.RoleAdmin, db, nil)
	r, _ := http.NewRequest("GET", "/api/v1/usage/concurrency", nil)
	if w := do(t, anon, r); w.Code != http.StatusUnauthorized {
		t.Errorf("no credentials status = %d, want 401", w.Code)
	}
	for _, role := range []authz.Role{authz.RoleOperator, authz.RoleAdmin} {
		s := concurrencyServer(t, role, db, nil)
		if w := do(t, s, authedRequest("GET", "/api/v1/usage/concurrency", nil)); w.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200: %s", role, w.Code, w.Body.String())
		}
	}
}

func TestUsageConcurrencyValidation(t *testing.T) {
	s := concurrencyServer(t, authz.RoleOperator, openDB(t), nil)
	cases := []struct{ query, field, code string }{
		{"since=yesterday", "since", "invalid_timestamp"},
		{"until=nope", "until", "invalid_timestamp"},
		{"since=2000&until=1000", "until", "invalid_time_range"},
		{"since=1000&until=1000", "until", "invalid_time_range"},
		{fmt.Sprintf("since=%d&until=%d", 1_000_000, 1_000_000+32*86400), "since", "time_range_too_large"},
	}
	for _, c := range cases {
		w := do(t, s, authedRequest("GET", "/api/v1/usage/concurrency?"+c.query, nil))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", c.query, w.Code, w.Body.String())
			continue
		}
		var body struct {
			Error      string            `json:"error"`
			FieldCodes map[string]string `json:"field_codes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != "validation_failed" || body.FieldCodes[c.field] != c.code {
			t.Errorf("%s: body = %s, want field_codes[%s]=%s", c.query, w.Body.String(), c.field, c.code)
		}
	}
}

func TestUsageConcurrencyEmptyWindow(t *testing.T) {
	s := concurrencyServer(t, authz.RoleOperator, openDB(t), nil)
	w := do(t, s, authedRequest("GET", "/api/v1/usage/concurrency?model=nothing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var r concurrencyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Requests != 0 || r.ExcludedUntimed != 0 || r.InFlight.Max != 0 || r.OverlapFraction != 0 || r.Model != "nothing" {
		t.Errorf("empty window = %+v, want all zero", r)
	}
	if !strings.Contains(w.Body.String(), `"excluded_untimed":0`) || !strings.Contains(w.Body.String(), `"outcomes":{`) {
		t.Errorf("body must carry explicit zeros: %s", w.Body.String())
	}
}

func TestUsageConcurrencyEndToEndExcludesOldRows(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	u := db.Usage()
	now := time.Now()
	start := now.Add(-time.Hour)
	rec := func(model, consumer string, off, dur time.Duration, out string, p, c int64) {
		t.Helper()
		if err := u.Record(ctx, store.UsageEvent{TS: start, Kind: "a0_request", Model: model, PromptTokens: p, CompletionTokens: c,
			Timing: &store.RequestTiming{Start: start.Add(off), Duration: dur, Wait: -1, Outcome: out, Consumer: consumer}}); err != nil {
			t.Fatal(err)
		}
	}
	rec("qwen", "opencode", 0, 4*time.Second, "ok", 1000, 200)
	rec("qwen", "librechat", time.Second, 4*time.Second, "ok", 3000, 400)
	rec("qwen", "opencode", 10*time.Second, time.Second, "aborted", 500, 5)
	rec("other", "opencode", 0, 4*time.Second, "ok", 1, 1)
	// A request recorded before the timing columns existed.
	if err := u.Record(ctx, store.UsageEvent{TS: start, Kind: "external_request", Model: "qwen", PromptTokens: 9, CompletionTokens: 9}); err != nil {
		t.Fatal(err)
	}

	s := concurrencyServer(t, authz.RoleOperator, db, nil)
	var r concurrencyResponse
	w := do(t, s, authedRequest("GET", "/api/v1/usage/concurrency?model=qwen", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Requests != 3 || r.ExcludedUntimed != 1 {
		t.Errorf("requests=%d excluded=%d, want 3 and 1 (the pre-timing row is left out and counted)", r.Requests, r.ExcludedUntimed)
	}
	if r.InFlight.Max != 2 || r.OverlappedRequests != 2 {
		t.Errorf("max=%d overlapped=%d, want 2 and 2 (the aborted one ran alone)", r.InFlight.Max, r.OverlappedRequests)
	}
	if r.Outcomes["aborted"] != 1 || r.PromptTokens.Max != 3000 {
		t.Errorf("outcomes=%v prompt=%+v", r.Outcomes, r.PromptTokens)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/usage/concurrency?model=qwen&consumer=opencode", nil))
	r = concurrencyResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.Requests != 2 || r.OverlappedRequests != 0 || r.Consumer != "opencode" {
		t.Errorf("consumer filter: requests=%d overlapped=%d consumer=%q, want 2/0/opencode", r.Requests, r.OverlappedRequests, r.Consumer)
	}

	// The per-request rows stay out of the dashboard's event timeline.
	w = do(t, s, authedRequest("GET", "/api/v1/usage/events?window=24h", nil))
	if strings.Contains(w.Body.String(), "a0_request") {
		t.Errorf("a0_request rows leaked into /usage/events: %s", w.Body.String())
	}
}

func TestStatusAndMetricsExposeTheGauge(t *testing.T) {
	g := inflight.New()
	c := g.Enter("qwen", "opencode")
	defer g.Leave("qwen", c)
	s := concurrencyServer(t, authz.RoleOperator, openDB(t), g)

	w := do(t, s, authedRequest("GET", "/api/v1/status", nil))
	var st struct {
		A0InFlight *inflight.Snapshot `json:"a0_in_flight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.A0InFlight == nil {
		t.Fatalf("status has no a0_in_flight: err=%v body=%s", err, w.Body.String())
	}
	found := false
	for _, r := range st.A0InFlight.Rows {
		if r.Model == "qwen" && r.Consumer == "opencode" && r.InFlight == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("rows = %+v, want qwen/opencode at 1", st.A0InFlight.Rows)
	}

	w = do(t, s, authedRequest("GET", "/api/v1/a0/in-flight/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `a0_requests_in_flight{model="qwen",consumer="opencode"} 1`) {
		t.Errorf("metrics %d: %s", w.Code, w.Body.String())
	}

	none := concurrencyServer(t, authz.RoleOperator, openDB(t), nil)
	if w := do(t, none, authedRequest("GET", "/api/v1/a0/in-flight/metrics", nil)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired metrics status = %d, want 503", w.Code)
	}
}

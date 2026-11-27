// SPDX-License-Identifier: Apache-2.0

package httpapi

// usage_concurrency.go — GET /api/v1/usage/concurrency: how many a0 requests were really in flight at once, measured from the
// stored request intervals (usage_events.started_ms/duration_ms, migration 0105), for sizing --max-num-seqs and capacity.
//
//	?model=<name>        only requests served by this model (the model column, i.e. the served name)
//	?consumer=<label>    only requests from this consumer label
//	?since=, ?until=     unix seconds or RFC 3339; a request is in the window when it STARTED in it. Defaults: the last 24 h.
//
// Operator role. Prompts, responses and keys are never read: the table holds none.

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

const maxConcurrencySpan = 31 * 24 * time.Hour

// quantiles is the distribution summary used for both in-flight levels and token counts.
type quantiles struct {
	P50 int64 `json:"p50"`
	P90 int64 `json:"p90"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

// concurrencyResponse is the body of GET /api/v1/usage/concurrency.
type concurrencyResponse struct {
	Model    string `json:"model,omitempty"`
	Consumer string `json:"consumer,omitempty"`
	Since    int64  `json:"since"` // unix seconds
	Until    int64  `json:"until"` // unix seconds
	// Requests is the number of timed requests that started in the window and match the filters.
	Requests int `json:"requests"`
	// ExcludedUntimed counts per-request rows in the same window (and model) that predate migration 0105 and so have no
	// start or duration. They are not in any figure below. Local requests have no pre-0105 per-request rows at all, so this
	// only ever counts remote (external_request) rows.
	ExcludedUntimed int `json:"excluded_untimed"`
	// Outcomes counts Requests by how they ended.
	Outcomes map[string]int `json:"outcomes"`
	// InFlight is the time-weighted distribution of the in-flight count over the time at least one request was in flight
	// (so "p90: 4" means that, while busy, the gauge was at 4 or below for 90% of the time). Max is the highest count seen.
	InFlight quantiles `json:"in_flight"`
	// BusySeconds is the time at least one request was in flight, the denominator of InFlight.
	BusySeconds float64 `json:"busy_seconds"`
	// OverlappedRequests is how many requests were in flight together with at least one other; OverlapFraction is that over
	// Requests (0 when there are none).
	OverlappedRequests int     `json:"overlapped_requests"`
	OverlapFraction    float64 `json:"overlap_fraction"`
	// PromptTokens and CompletionTokens are nearest-rank percentiles over the requests whose upstream reported usage
	// (TokenSamples of them).
	PromptTokens     quantiles `json:"prompt_tokens"`
	CompletionTokens quantiles `json:"completion_tokens"`
	TokenSamples     int       `json:"token_samples"`
}

// concurrencyStats runs the sweep line over the intervals. Each interval is the half-open [start, end) in milliseconds, with
// a request shorter than 1 ms counted as 1 ms: so intervals that merely touch (one ends the instant the next starts) do not
// overlap, identical intervals (including identical zero-length ones) do, and a zero-length request inside another overlaps it.
func concurrencyStats(ivs []store.Interval) (in quantiles, busy time.Duration, overlapped int) {
	n := len(ivs)
	if n == 0 {
		return quantiles{}, 0, 0
	}
	type iv struct{ s, e int64 }
	xs := make([]iv, n)
	type edge struct {
		t     int64
		delta int
	}
	edges := make([]edge, 0, 2*n)
	for i, v := range ivs {
		s, e := v.StartMS, v.EndMS
		if e <= s {
			e = s + 1
		}
		xs[i] = iv{s, e}
		edges = append(edges, edge{s, +1}, edge{e, -1})
	}

	// Levels: ends sort before starts at the same instant, so touching intervals never stack.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].t != edges[j].t {
			return edges[i].t < edges[j].t
		}
		return edges[i].delta < edges[j].delta
	})
	timeAt := map[int]int64{} // level -> ms spent at it
	level, maxLevel := 0, 0
	for i, ed := range edges {
		if i > 0 && level > 0 {
			timeAt[level] += ed.t - edges[i-1].t
		}
		level += ed.delta
		if level > maxLevel {
			maxLevel = level
		}
	}
	var busyMS int64
	levels := make([]int, 0, len(timeAt))
	for l, ms := range timeAt {
		busyMS += ms
		levels = append(levels, l)
	}
	sort.Ints(levels)
	pick := func(p float64) int64 {
		if busyMS == 0 {
			return 0
		}
		need := p * float64(busyMS)
		var cum float64
		for _, l := range levels {
			cum += float64(timeAt[l])
			if cum >= need {
				return int64(l)
			}
		}
		return int64(maxLevel)
	}
	in = quantiles{P50: pick(0.50), P90: pick(0.90), P99: pick(0.99), Max: int64(maxLevel)}

	// Overlap: sorted by start, an interval overlaps an earlier one when some earlier end lies beyond its start, and a later
	// one when the next start lies before its end (the next start is the earliest of all later ones).
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].s != xs[j].s {
			return xs[i].s < xs[j].s
		}
		return xs[i].e < xs[j].e
	})
	var maxEnd int64 = math.MinInt64
	for i, x := range xs {
		hit := maxEnd > x.s || (i+1 < n && xs[i+1].s < x.e)
		if hit {
			overlapped++
		}
		if x.e > maxEnd {
			maxEnd = x.e
		}
	}
	return in, time.Duration(busyMS) * time.Millisecond, overlapped
}

// tokenQuantiles is nearest-rank over the non-nil values.
func tokenQuantiles(vals []int64) quantiles {
	if len(vals) == 0 {
		return quantiles{}
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	rank := func(p float64) int64 {
		i := int(math.Ceil(p*float64(len(vals)))) - 1
		if i < 0 {
			i = 0
		}
		return vals[i]
	}
	return quantiles{P50: rank(0.50), P90: rank(0.90), P99: rank(0.99), Max: vals[len(vals)-1]}
}

func buildConcurrency(set store.IntervalSet) concurrencyResponse {
	resp := concurrencyResponse{
		Requests:        len(set.Intervals),
		ExcludedUntimed: set.ExcludedUntimed,
		Outcomes:        map[string]int{store.OutcomeOK: 0, store.OutcomeError: 0, store.OutcomeAborted: 0},
	}
	var prompt, compl []int64
	for _, v := range set.Intervals {
		resp.Outcomes[v.Outcome]++
		if v.PromptTokens != nil && v.CompletionTokens != nil {
			prompt = append(prompt, *v.PromptTokens)
			compl = append(compl, *v.CompletionTokens)
		}
	}
	var busy time.Duration
	resp.InFlight, busy, resp.OverlappedRequests = concurrencyStats(set.Intervals)
	resp.BusySeconds = busy.Seconds()
	if resp.Requests > 0 {
		resp.OverlapFraction = float64(resp.OverlappedRequests) / float64(resp.Requests)
	}
	resp.TokenSamples = len(prompt)
	resp.PromptTokens = tokenQuantiles(prompt)
	resp.CompletionTokens = tokenQuantiles(compl)
	return resp
}

// handleUsageConcurrency — GET /api/v1/usage/concurrency.
func (s *Server) handleUsageConcurrency(w http.ResponseWriter, r *http.Request) {
	if s.deps.Usage == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "usage store"}, "usage store not wired")
		return
	}
	q := r.URL.Query()
	now := time.Now()
	until, since := now, now.Add(-24*time.Hour)
	fields, codes := map[string]string{}, map[string]string{}
	if raw := q.Get("since"); raw != "" {
		if t, ok := parseSince(raw); ok {
			since = t
		} else {
			fields["since"], codes["since"] = "must be unix seconds or RFC3339", "invalid_timestamp"
		}
	}
	if raw := q.Get("until"); raw != "" {
		if t, ok := parseSince(raw); ok {
			until = t
		} else {
			fields["until"], codes["until"] = "must be unix seconds or RFC3339", "invalid_timestamp"
		}
	}
	if len(fields) == 0 {
		switch {
		case !until.After(since):
			fields["until"], codes["until"] = "must be after since", "invalid_time_range"
		case until.Sub(since) > maxConcurrencySpan:
			fields["since"], codes["since"] = "the window may span at most 31 days", "time_range_too_large"
		}
	}
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	model, consumer := q.Get("model"), q.Get("consumer")
	set, err := s.deps.Usage.Intervals(ctx, store.IntervalQuery{Model: model, Consumer: consumer, Since: since, Until: until})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp := buildConcurrency(set)
	resp.Model, resp.Consumer = model, consumer
	resp.Since, resp.Until = since.Unix(), until.Unix()
	writeJSON(w, http.StatusOK, resp)
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

// usage_handlers.go — aggregated usage/cost payload + raw event timeline
// (Contract 1 §2 #7–8). Split out of handlers.go by Sprint 0
// (docs/v5-sprint0-contract-freeze.md §0.1); pure move, no behavior change.
// Owner track after split: BE-2.

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"
)

// handleUsage returns the aggregated usage/cost payload (Contract 1 §2 #7).
// Mirrors V4's usage.get_usage(): per-model token + lifecycle counters,
// per-external-provider costs, per-proxy compressor savings, totals.
//
// Sprint 0 §0.2 billing & currency: local "cost" is the per-1M electricity
// estimate (registry.PowerEstPer1m, USD-denominated) — computed from
// power_kW × time-to-generate-1M-tokens × rate_per_kWh, not a curated guess
// (BE-COST, docs/v5-review-fixes.md F5) — and external cost is the
// provider's native billed amount; both are FX-converted to the display
// currency (billing.display_currency via the daemon-side fx.Source). External
// rows carry cost_native + native_currency; the response carries
// display_currency + fx_as_of (null when no conversion was applied) +
// fx_stale. When FX is nil (stub/tests) everything is USD 1:1.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}
	dur, ok := parseUsageWindow(window)
	if !ok {
		window = "7d"
		dur = 7 * 24 * time.Hour
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	display := s.displayCurrency(ctx)

	resp := usageResponse{
		Window: window,
		// Filled after aggregation: null/false until a conversion is applied.
		DisplayCurrency: display,
		FxAsOf:          nil,
		FxStale:         false,
		Models:          []usageModelRow{},
		External:        []usageExternalRow{},
		Compressor:      []compressorSavingsRow{},
		Totals:          usageTotals{},
	}

	if s.deps.Usage == nil {
		// No store wired (Phase 4 stub) — return empty.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	since := time.Now().Add(-dur)
	events, err := s.deps.Usage.Events(ctx, since, 1_000_000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "usage query failed")
		return
	}

	// Aggregate by model (kind=inference for token samples; lifecycle
	// events for loads_ok/load_failures/inference_hangs/kfd_evictions/unloads)
	// and by external provider (kind=external_request).
	byModel := map[string]*usageModelRow{}
	byExt := map[string]*usageExternalRow{}

	// Cache provider → bill_currency across the loop (one DB hit per provider).
	billCcy := map[string]string{}
	billCcyFor := func(provider string) string {
		if c, ok := billCcy[provider]; ok {
			return c
		}
		c := s.billCurrency(ctx, provider)
		billCcy[provider] = c
		return c
	}

	for _, ev := range events {
		switch ev.Kind {
		case "inference":
			m := byModel[ev.Model]
			if m == nil {
				m = &usageModelRow{Model: ev.Model}
				byModel[ev.Model] = m
			}
			m.PromptTokens += ev.PromptTokens
			m.PredictedTokens += ev.CompletionTokens
		case "load_ok":
			m := byModel[ev.Model]
			if m == nil {
				m = &usageModelRow{Model: ev.Model}
				byModel[ev.Model] = m
			}
			m.LoadsOK++
		case "load_failed", "load_failure":
			m := byModel[ev.Model]
			if m == nil {
				m = &usageModelRow{Model: ev.Model}
				byModel[ev.Model] = m
			}
			m.LoadFailures++
		case "inference_hang":
			m := byModel[ev.Model]
			if m == nil {
				m = &usageModelRow{Model: ev.Model}
				byModel[ev.Model] = m
			}
			m.InferenceHangs++
		// "kfd_eviction"/"kfd_evict" intentionally not read here: no writer
		// ever emits either kind. See registry.Reliability.KFDEvictions'
		// doc comment — real detection needs dmesg access (out of scope).
		case "unload":
			m := byModel[ev.Model]
			if m == nil {
				m = &usageModelRow{Model: ev.Model}
				byModel[ev.Model] = m
			}
			m.Unloads++
		case "external_request":
			key := ev.ProviderName + "/" + ev.Model
			e := byExt[key]
			if e == nil {
				e = &usageExternalRow{Provider: ev.ProviderName, Model: ev.Model}
				byExt[key] = e
			}
			e.Requests++
			if ev.Unmetered {
				e.RequestsUnmetered++
				continue // no real tokens/cost to add — see RequestsUnmetered's doc comment
			}
			e.PromptTokens += ev.PromptTokens
			e.CompletionTokens += ev.CompletionTokens
			// cost/savings sprint Phase 4 (2026-07-30): the router now
			// writes real per-request CostNative/CostCurrency (the offering's
			// own currency). Legacy rows (pre-Phase-4, or a row where no
			// offering priced the request) have CostNative == nil — cost_usd
			// is their only figure, and per Sprint 0 §0.2 was always assumed
			// USD-denominated.
			if ev.CostNative != nil {
				e.CostNative += *ev.CostNative
				if e.NativeCurrency == "" && ev.CostCurrency != "" {
					e.NativeCurrency = ev.CostCurrency
				}
			} else {
				e.CostNative += ev.CostUSD
				if e.NativeCurrency == "" {
					e.NativeCurrency = "USD"
				}
			}
		}
	}

	// Track whether any non-trivial FX conversion was applied (drives the
	// response-level fx_as_of / fx_stale) and whether a needed rate was
	// missing (forces fx_stale=true even if some rates are cached).
	var (
		anyConversion bool
		anyMissing    bool
	)

	for name, m := range byModel {
		// Local cost: tokens × registry.PowerEstPer1m(configID) — the
		// per-1M electricity estimate (Sprint 0 §0.2). The token sample's
		// "model" field is the mode name; B2 resolves it to the config ID
		// via the merged config seam (cfg.Modes[name].ConfigID).
		// registry.PowerEstPer1m computes its figure from cfg.Cost.RatePerKWh,
		// which is denominated in cfg.Cost.RateCurrency (NOT necessarily
		// USD — an operator-set rate_currency of e.g. "JPY" was previously
		// mis-converted here by a hardcoded "USD" from-currency; fixed
		// 2026-07-30, cost/savings sprint). FX-convert powerRateCurrency→
		// display when they differ.
		var powerCost float64
		powerRateCurrency := "USD"
		if s.deps.Registry != nil && s.deps.Config != nil {
			if cfg := s.deps.Config(); cfg != nil {
				if cfg.Cost.RateCurrency != "" {
					powerRateCurrency = cfg.Cost.RateCurrency
				}
				if mode, ok := cfg.Modes[name]; ok && mode.ConfigID != 0 {
					if rate, ok := s.deps.Registry.PowerEstPer1m(mode.ConfigID); ok {
						totalTok := float64(m.PromptTokens + m.PredictedTokens)
						powerCost = totalTok / 1e6 * rate
					}
				}
			}
		}
		converted, missing := s.convert(ctx, powerCost, powerRateCurrency, display)
		m.PowerCostDisplay = round6(converted)
		anyConversion = anyConversion || (display != powerRateCurrency)
		anyMissing = anyMissing || missing
		resp.Models = append(resp.Models, *m)
		resp.Totals.LocalCostDisplay += m.PowerCostDisplay
	}

	for _, e := range byExt {
		if e.NativeCurrency == "" {
			e.NativeCurrency = billCcyFor(e.Provider)
		}
		converted, missing := s.convert(ctx, e.CostNative, e.NativeCurrency, display)
		e.CostDisplay = round6(converted)
		anyConversion = anyConversion || (e.NativeCurrency != display)
		anyMissing = anyMissing || missing
		resp.External = append(resp.External, *e)
		resp.Totals.ExternalCostDisplay += e.CostDisplay
	}

	// Compressor savings from store.Routing.
	if s.deps.Routing != nil {
		savings, err := s.deps.Routing.Savings(ctx, since)
		if err == nil {
			for proxy, total := range savings {
				resp.Compressor = append(resp.Compressor, compressorSavingsRow{
					Proxy:    proxy,
					TokensIn: total.TokensIn,
					Saved:    total.Saved,
				})
				resp.Totals.CompressorSavedTokens += total.Saved
			}
		}
	}

	// Sort: models by total tokens desc, external by cost desc, compressor
	// by saved desc (V4 get_usage).
	sort.Slice(resp.Models, func(i, j int) bool {
		return resp.Models[i].PromptTokens+resp.Models[i].PredictedTokens >
			resp.Models[j].PromptTokens+resp.Models[j].PredictedTokens
	})
	sort.Slice(resp.External, func(i, j int) bool {
		return resp.External[i].CostDisplay > resp.External[j].CostDisplay
	})
	sort.Slice(resp.Compressor, func(i, j int) bool {
		return resp.Compressor[i].Saved > resp.Compressor[j].Saved
	})

	resp.Totals.TotalCostDisplay = resp.Totals.LocalCostDisplay + resp.Totals.ExternalCostDisplay
	resp.FxAsOf, resp.FxStale = s.fxProvenance(ctx, anyConversion, anyMissing)
	writeJSON(w, http.StatusOK, resp)
}

// displayCurrency resolves billing.display_currency via the FX source
// ("USD" when FX is nil or the setting is unset).
func (s *Server) displayCurrency(ctx context.Context) string {
	if s.deps.FX == nil {
		return "USD"
	}
	return s.deps.FX.DisplayCurrency(ctx)
}

// billCurrency resolves a provider's bill_currency via the FX source ("USD"
// when FX is nil or the provider is unknown).
func (s *Server) billCurrency(ctx context.Context, provider string) string {
	if s.deps.FX == nil {
		return "USD"
	}
	return s.deps.FX.BillCurrency(ctx, provider)
}

// convert FX-converts amount from `from` to `to`. Returns the converted
// amount (1:1 when from==to or FX is nil), and missing=true when a non-trivial
// conversion was needed but no rate was cached (the caller falls back to 1:1
// and the response is flagged fx_stale).
func (s *Server) convert(ctx context.Context, amount float64, from, to string) (float64, bool) {
	if from == to {
		return amount, false
	}
	if s.deps.FX == nil {
		// Stub path: no FX wired → treat as 1:1 (display assumed USD). The
		// caller's anyConversion flag still tracks that a conversion *would*
		// be needed, but with FX nil display is USD and from is USD/CNY… —
		// in practice this branch is unreachable when display==from.
		return amount, true
	}
	rate, ok := s.deps.FX.Rate(ctx, from, to)
	if !ok {
		return amount, true // fall back to 1:1, flag stale
	}
	return amount * rate, false
}

// fxProvenance resolves the response-level fx_as_of / fx_stale. null/false
// when no conversion was applied; otherwise the FX source's provenance, with
// fx_stale forced true if any needed rate was missing.
func (s *Server) fxProvenance(ctx context.Context, anyConversion, anyMissing bool) (*float64, bool) {
	if !anyConversion {
		return nil, false
	}
	if s.deps.FX == nil {
		// Conversion needed but no FX wired (stub): flag stale, no as_of.
		return nil, true
	}
	asOf, stale, hasRates := s.deps.FX.Provenance(ctx)
	stale = stale || anyMissing || !hasRates
	if !hasRates {
		return nil, stale
	}
	v := float64(asOf.UnixNano()) / 1e9
	return &v, stale
}

// round6 rounds to 6 decimal places (micro-currency precision) so floating
// FX multiplication doesn't surface 17-significant-digit noise in the JSON.
func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// handleUsageEvents returns the raw event timeline (Contract 1 §2 #8).
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	dur, ok := parseUsageWindow(window)
	if !ok {
		window = "24h"
		dur = 24 * time.Hour
	}

	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parsePositiveInt(v); err == nil && n > 0 {
			limit = n
			if limit > 5000 {
				limit = 5000
			}
		}
	}

	out := []usageEventResponse{}
	if s.deps.Usage == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	since := time.Now().Add(-dur)
	events, err := s.deps.Usage.Events(ctx, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "usage events query failed")
		return
	}
	for _, ev := range events {
		out = append(out, usageEventResponse{
			TS:     unixSeconds(ev.TS),
			Kind:   ev.Kind,
			Model:  ev.Model,
			Slot:   ev.Slot,
			Detail: ev.Detail,
		})
	}
	// Most-recent-first (V4 get_events).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].TS > out[j].TS
	})
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, out)
}

// usageHeatmapResponse is a dense, zero-filled per-calendar-day token-volume
// series (Sprint L Console/Dashboard visual sprint's activity heatmap). Dense
// so the client never has to gap-fill: every day in [window ago, today] gets
// a row even when no traffic occurred.
type usageHeatmapResponse struct {
	Window string            `json:"window"`
	TZ     string            `json:"tz"`
	Days   []usageHeatmapDay `json:"days"`
}

type usageHeatmapDay struct {
	Date     string `json:"date"` // YYYY-MM-DD, calendar day in TZ
	Tokens   int64  `json:"tokens"`
	Requests int    `json:"requests"`

	// Local/external split (ALL/Local/External toggle) — Local is
	// kind="inference" (a Forge slot), External is kind="external_request"
	// (a remote provider). Additive to Tokens/Requests above, which stay the
	// sum of both so existing "all" consumers don't need to change.
	TokensLocal      int64 `json:"tokens_local"`
	TokensExternal   int64 `json:"tokens_external"`
	RequestsLocal    int   `json:"requests_local"`
	RequestsExternal int   `json:"requests_external"`
}

// handleUsageHeatmap returns daily token-volume totals for a GitHub-
// contribution-style grid. Buckets align to LOCAL CALENDAR MIDNIGHT in the
// requested tz, deliberately not the from-anchored modulo bucketing
// /metrics/history and /cost/energy-history use — that scheme floats bucket
// boundaries with request time, which is fine for a rolling trend line but
// would smear every cell across two calendar days here (and at a positive UTC
// offset, leave "today" empty until well into the morning).
func (s *Server) handleUsageHeatmap(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	windowRaw := q.Get("window")
	if windowRaw == "" {
		windowRaw = "84d"
	}
	dur, ok := parseUsageWindow(windowRaw)
	if !ok {
		windowRaw = "84d"
		dur = 84 * 24 * time.Hour
	}
	days := int(math.Ceil(dur.Hours() / 24))
	if days < 1 {
		days = 1
	}

	tzName := q.Get("tz")
	loc, err := time.LoadLocation(tzName)
	if tzName == "" || err != nil {
		loc = time.UTC
		tzName = "UTC"
	}

	resp := usageHeatmapResponse{Window: windowRaw, TZ: tzName, Days: []usageHeatmapDay{}}
	if s.deps.Usage == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	now := time.Now().In(loc)
	endDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	startDay := endDay.AddDate(0, 0, -(days - 1))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := s.deps.Usage.TokenActivity(ctx, startDay)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "usage heatmap query failed")
		return
	}

	type bucket struct {
		tokens   int64
		requests int

		tokensLocal      int64
		tokensExternal   int64
		requestsLocal    int
		requestsExternal int
	}
	buckets := make(map[string]*bucket, days)
	order := make([]string, 0, days)
	for d := startDay; !d.After(endDay); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		buckets[key] = &bucket{}
		order = append(order, key)
	}
	for _, row := range rows {
		key := row.TS.In(loc).Format("2006-01-02")
		b, ok := buckets[key]
		if !ok {
			continue // outside the dense range (clock skew between the two "now" reads) — drop rather than grow the grid
		}
		b.tokens += row.Tokens
		b.requests++
		switch row.Kind {
		case "inference":
			b.tokensLocal += row.Tokens
			b.requestsLocal++
		case "external_request":
			b.tokensExternal += row.Tokens
			b.requestsExternal++
		}
	}
	for _, key := range order {
		b := buckets[key]
		resp.Days = append(resp.Days, usageHeatmapDay{
			Date:             key,
			Tokens:           b.tokens,
			Requests:         b.requests,
			TokensLocal:      b.tokensLocal,
			TokensExternal:   b.tokensExternal,
			RequestsLocal:    b.requestsLocal,
			RequestsExternal: b.requestsExternal,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

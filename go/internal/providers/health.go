// SPDX-License-Identifier: Apache-2.0

package providers

// health.go — provider health probes (Sprint 0 §0.3).
//
// The fallback chain per §0.3:
//  1. status_page — when router_providers.status_url is configured, fetch
//     a known status-page JSON shape (Statuspage.io /api/v2/summary.json
//     convention) and derive state from the incident list.
//  2. live_probe — fall back to GET <target_url>/models with the API key.
//     A 2xx response ⇒ reachable; anything else ⇒ down.
//  3. none — when the provider has neither status_url nor a usable
//     target_url+api_key, state="unknown" with source="none".
//
// "idle" is intentionally absent from the vocabulary (user: "I don't care
// about idle"). The frozen states are reachable | degraded | down | unknown.
//
// Live-verified 2026-07-22: DeepSeek has no public status page (status URL
// 404s), so the live probe path is what actually fires on ForgeHost; AI& also
// has no status page. Both providers' /v1/models endpoints return 200, so
// both surface as reachable via live_probe.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Health vocabulary (frozen by §0.3, mirrored from providerHealthJSON).
const (
	HealthStateReachable = "reachable"
	HealthStateDegraded  = "degraded"
	HealthStateDown      = "down"
	HealthStateUnknown   = "unknown"

	HealthSourceStatusPage = "status_page"
	HealthSourceLiveProbe  = "live_probe"
	HealthSourceNone       = "none"
)

// healthClients bundles the per-strategy fetchers so the service can
// dispatch in one call. Construction lives in newHealthClients.
type healthClients struct {
	statusPage *statusPageChecker
	liveProbe  *liveProbeChecker
}

func newHealthClients(c httpClient, timeout time.Duration) *healthClients {
	return &healthClients{
		statusPage: &statusPageChecker{client: c, timeout: timeout},
		liveProbe:  &liveProbeChecker{client: c, timeout: timeout},
	}
}

// fetch runs the §0.3 fallback chain for one provider. Never returns an
// error — a fetch failure degrades the state, never the request.
func (h *healthClients) fetch(ctx context.Context, row store.ProviderRow) Health {
	if row.StatusURL != "" {
		if health, ok := h.statusPage.check(ctx, row.StatusURL); ok {
			return health
		}
		// Status page unreachable/unparseable → fall through to live probe.
	}
	if row.TargetURL != "" {
		if health, ok := h.liveProbe.check(ctx, row.TargetURL, row.APIKey); ok {
			return health
		}
	}
	return Health{State: HealthStateUnknown, AsOf: time.Time{}, Source: HealthSourceNone}
}

// ── Status-page strategy ─────────────────────────────────────────────────────

// statusPageChecker parses the Statuspage.io /api/v2/summary.json shape
// (the de-facto standard — Atlassian Statuspage, hosted status pages).
// Live-verified against a real ForgeHost-relevant provider on 2026-07-22: the
// shape is {"components":[...],"incidents":[...],"status":{...}} where a
// non-empty incidents array ⇒ degraded/down depending on impact, and
// status.indicator ∈ {none,minor,major,critical} drives the state.
type statusPageChecker struct {
	client httpClient
	timeout time.Duration
}

// summaryJSON is the subset of the Statuspage /api/v2/summary.json shape
// this checker reads. Unknown fields are ignored (the real payload is much
// larger; we only need status + incidents).
type summaryJSON struct {
	Status struct {
		Indicator string `json:"indicator"`
	} `json:"status"`
	Incidents []struct {
		Name     string `json:"name"`
		Impact   string `json:"impact"`
		Status   string `json:"status"` // investigating|identified|monitoring|resolved
	} `json:"incidents"`
}

func (c *statusPageChecker) check(ctx context.Context, statusURL string) (Health, bool) {
	body, code, err := fetchJSON(ctx, c.client, statusURL, "", c.timeout)
	if err != nil || code != 200 {
		// Network failure or non-200 → caller falls through to live probe.
		return Health{}, false
	}
	var sum summaryJSON
	if err := json.Unmarshal(body, &sum); err != nil {
		return Health{}, false
	}
	now := time.Now()
	// Unresolved incidents (status != resolved) drive degraded/down.
	state := HealthStateReachable
	detail := ""
	for _, inc := range sum.Incidents {
		if inc.Status == "resolved" {
			continue
		}
		switch inc.Impact {
		case "critical":
			state = HealthStateDown
		case "major", "minor":
			if state != HealthStateDown {
				state = HealthStateDegraded
			}
		}
		if detail == "" {
			detail = inc.Name
		}
	}
	// status.indicator is the overall page status — trust it as the
	// authoritative state when incidents don't already say worse.
	if state == HealthStateReachable {
		switch sum.Status.Indicator {
		case "critical":
			state = HealthStateDown
		case "major", "minor":
			state = HealthStateDegraded
		}
	}
	return Health{
		State:  state,
		AsOf:   now,
		Source: HealthSourceStatusPage,
		Detail: detail,
	}, true
}

// ── Live-probe strategy ──────────────────────────────────────────────────────

// liveProbeChecker GETs <target_url>/models with the provider's API key.
// 2xx ⇒ reachable; 4xx auth failure ⇒ down (auth issue is a config problem,
// not "down" upstream, but the user can't tell the difference and the
// operator should fix it); 5xx ⇒ down; network error ⇒ down (with no
// as_of, since the probe never reached the server).
//
// The /models endpoint is the OpenAI-compatible standard; both DeepSeek
// and AI& implement it (live-verified 2026-07-22). The probe is the
// fallback when no status_url is configured — which is the actual state on
// ForgeHost today.
type liveProbeChecker struct {
	client httpClient
	timeout time.Duration
}

func (c *liveProbeChecker) check(ctx context.Context, targetURL, apiKey string) (Health, bool) {
	// Build the probe URL: targetURL is the OpenAI-style base (e.g.
	// "https://api.deepseek.com/v1"). Append "/models" if not present.
	url := strings.TrimRight(targetURL, "/") + "/models"
	body, code, err := fetchJSON(ctx, c.client, url, apiKey, c.timeout)
	if err != nil {
		// Network failure → down with no as_of (never reached the server).
		// The caller (refresh) will still cache this state so the next
		// request doesn't immediately retry.
		return Health{State: HealthStateDown, AsOf: time.Time{}, Source: HealthSourceLiveProbe}, true
	}
	_ = body // 2xx is sufficient; we don't parse the model list here
	if code >= 200 && code < 300 {
		return Health{State: HealthStateReachable, AsOf: time.Now(), Source: HealthSourceLiveProbe}, true
	}
	return Health{State: HealthStateDown, AsOf: time.Now(), Source: HealthSourceLiveProbe}, true
}

// ── Cache encode/decode ───────────────────────────────────────────────────────

// healthJSON mirrors providerHealthJSON in httpapi/shapes.go 1:1 (frozen
// shape). Kept here so the cache blob is forward-compatible with the wire
// shape — the handler can round-trip the blob directly without re-encoding.
type healthJSON struct {
	State  string   `json:"state"`
	AsOf   *float64 `json:"as_of"`
	Source string   `json:"source"`
	Detail *string  `json:"detail"`
}

// creditsJSON mirrors providerCreditsJSON in httpapi/shapes.go 1:1.
type creditsJSON struct {
	BalanceNative *float64 `json:"balance_native"`
	Currency      *string  `json:"currency"`
	AsOf          *float64 `json:"as_of"`
	Supported     bool     `json:"supported"`
}

func encodeHealth(h Health) string {
	b, _ := json.Marshal(healthJSON{
		State:  h.State,
		AsOf:   unixSecondsPtr(h.AsOf),
		Source: h.Source,
		Detail: ptrStringIfNonEmpty(h.Detail),
	})
	return string(b)
}

func decodeHealth(blob string) Health {
	if blob == "" {
		return Health{State: HealthStateUnknown, Source: HealthSourceNone}
	}
	var h healthJSON
	if err := json.Unmarshal([]byte(blob), &h); err != nil {
		return Health{State: HealthStateUnknown, Source: HealthSourceNone}
	}
	return Health{
		State:  orDefault(h.State, HealthStateUnknown),
		AsOf:   timeOfPtr(h.AsOf),
		Source: orDefault(h.Source, HealthSourceNone),
		Detail: derefStr(h.Detail),
	}
}

func encodeCredits(c Credits) string {
	b, _ := json.Marshal(creditsJSON{
		BalanceNative: c.BalanceNative,
		Currency:      ptrStringIfNonEmpty(c.Currency),
		AsOf:          unixSecondsPtr(c.AsOf),
		Supported:     c.Supported,
	})
	return string(b)
}

func decodeCredits(blob string) Credits {
	if blob == "" {
		return Credits{Supported: false}
	}
	var c creditsJSON
	if err := json.Unmarshal([]byte(blob), &c); err != nil {
		return Credits{Supported: false}
	}
	return Credits{
		BalanceNative: c.BalanceNative,
		Currency:      derefStr(c.Currency),
		AsOf:         timeOfPtr(c.AsOf),
		Supported:    c.Supported,
	}
}

// ── Small helpers (kept unexported + in this file so the package's
// helpers are co-located with the encode/decode users) ────────────────────────

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrStringIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func unixSecondsPtr(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	v := float64(t.UnixNano()) / 1e9
	return &v
}

func timeOfPtr(p *float64) time.Time {
	if p == nil || *p == 0 {
		return time.Time{}
	}
	sec := int64(*p)
	nsec := int64((*p - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}

// errHealthParse is returned by the live-probe when the body isn't JSON
// — currently unused (we treat any 2xx as reachable), kept for the
// future when the probe may parse the model list.
var errHealthParse = errors.New("providers: parse health response")

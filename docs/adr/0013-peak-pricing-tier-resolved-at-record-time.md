# Peak/off-peak pricing tier is resolved once, at usage-record time

Status: accepted.

DeepSeek switched to time-of-day (peak/off-peak) API pricing on 2026-09-10 — peak hours cost
2x off-peak, currently 01:00–04:00 and 06:00–10:00 UTC, Monday–Friday. `store.Offering` held
exactly one flat price triple, so before this change roughly half of all DeepSeek spend was
mispriced no matter what number was entered.

## Decision

Each offering carries an optional peak-tier price triple (`price_in_per_1m_peak`/
`price_out_per_1m_peak`/`price_cached_in_per_1m_peak`, per-field nil = falls back to the base
rate — the same convention `PriceCachedInPer1M` already used); the schedule itself
(`internal/pricing.Windows`) lives on the **provider**, not the offering, since a peak window is
a fact about the provider's billing policy shared by every model it serves, not a per-model
property. `router/routing.go`'s `offeringChain` copies both triples plus the parsed schedule
onto `Backend`/`ResolvedBackend` at its single existing copy point.

**The tier itself is picked exactly once, in `computeCostNative` (`router/usage.go`), at the
same instant `recordExternalUsage` stamps the event's timestamp** — not earlier, at
route-selection/request-start time. Concretely: `now := time.Now()` is captured once in
`recordExternalUsage` and used for both `ev.TS` and the tier lookup; the tier is recorded on the
event (`usage_events.price_tier`).

## Why not resolve the tier at request start

Nothing upstream of cost computation uses price for a routing decision — `select.go` sorts
candidate offerings by `priority` only, so resolving the tier early buys nothing operationally.
Resolving it early would actively create a correctness problem: a request whose response
completes after a tier boundary would then have a stored cost that *disagrees with the tier
derivable from the event's own stored timestamp* — and `compressor_summary_handlers.go`'s
historical savings estimators (`estimateRemoteCacheDiscountSaved`/
`estimateRemoteCompressionSaved`) re-price past events from exactly that timestamp. Pinning the
tier at record time means the ledger is self-consistent by construction: one function
(`computeCostNative`), one time input, shared by both the live-billing path and the historical
estimator (which prefers the event's own recorded `price_tier`, falling back to evaluating the
schedule against `ev.TS` only for rows written before this migration).

**Consequence, disclosed rather than hidden:** a request that starts in one tier and whose
response completes after a boundary is billed entirely at the tier in force at completion, not
a split or the starting tier. A 2x rate delta on a long streaming completion is exactly the
boundary-crossing case where this matters most — a reproducible, auditable rule (visible via the
recorded `price_tier`) beats guessing DeepSeek's own internal accounting for the same case.

## Other decisions folded in here

- **Half-open `[start, end)` window boundaries** — a request landing exactly on the window's end
  time is off-peak, pinned by `internal/pricing`'s own tests.
- **No "has peak pricing" boolean.** A flag can disagree with the data it describes; the
  per-field-nil convention is one source of truth.
- **Peak price validated for sign only, never `peak >= base`.** Whether a provider's peak tier
  costs more or less than off-peak is the provider's own policy, not this app's to enforce.
- **`peak_active_now` is computed server-side only**, both on the repeatedly-polled
  `GET /api/v1/providers` list (via `internal/providers.Service`'s existing injectable clock) and
  on the one-off create/update echo (`httpapi.peakActiveNow`, plain `time.Now()`) — the window
  math is never ported to TypeScript.

## Correction, 2026-09-13: timezone support

The original decision above said "UTC only, no timezone field — a timezone field the code
doesn't honor is worse than no field." That was right for DeepSeek's own published schedule (it
really is UTC), but wrong as a general policy: most providers publish peak hours in one local
zone ("9am–5pm Pacific"), and requiring the operator to hand-convert to UTC is exactly the trap
the original reasoning was trying to avoid — a fixed UTC offset entered once goes silently wrong
by an hour across the next DST transition, twice a year, with nothing to catch it.

**Fixed properly, not worked around:** `Windows` gained an optional `TZ` field (IANA zone name,
`""` = UTC, backward-compatible with DeepSeek's already-stored no-`tz` schedule). `Active`
converts the evaluation instant into that zone via `time.Time.In` — Go's real IANA tzdata, not a
stored offset — before any day-of-week/time-of-day comparison, so the same schedule is correct
in January and July without ever being re-entered, and a schedule quoted in a zone far from UTC
correctly shifts which calendar day it evaluates against (e.g. "Monday 00:00–04:00 Asia/Tokyo" is
active during Sunday afternoon UTC, because it's already Monday in Tokyo). `time.LoadLocation` is
memoized (`internal/pricing`'s package-level `locCache`) since `Active` runs on the remote-request
hot path and the stdlib doesn't cache zoneinfo parsing itself. No new migration — `peak_windows`
was already a JSON `TEXT` column; `tz` is just a new key in that same blob.

Frontend: `ProviderKeys`' `PeakWindowsEditor` gained a labeled Timezone field (a curated
common-zones dropdown + a "Custom…" free-text IANA name escape hatch) separate from the windows
JSON textarea, composing/splitting the two into the one stored JSON string — the timezone is a
single flat value worth a real input, unlike the windows list itself (still JSON; still no
bespoke hour-range picker, per the original reasoning, which stands).

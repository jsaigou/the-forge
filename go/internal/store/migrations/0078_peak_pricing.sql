-- DeepSeek switched to time-of-day pricing on 2026-09-10 (peak hours cost
-- 2x off-peak). offerings previously stored exactly one flat price triple,
-- so roughly half of all DeepSeek spend was mispriced regardless of what
-- number was entered. All new columns are nullable additive: the existing
-- price_in_per_1m/price_out_per_1m/price_cached_in_per_1m columns keep
-- meaning "the price when no peak window is active" (off-peak), and a NULL
-- peak column falls back per-field to its base value — every existing
-- offering/provider/event is unchanged and correct by construction, no
-- backfill needed. See internal/pricing for the window-evaluation logic and
-- go/internal/router/usage.go's computeCostNative for how these are priced.

ALTER TABLE offerings ADD COLUMN price_in_per_1m_peak REAL;
ALTER TABLE offerings ADD COLUMN price_out_per_1m_peak REAL;
ALTER TABLE offerings ADD COLUMN price_cached_in_per_1m_peak REAL;

-- peak_windows is a JSON-encoded internal/pricing.Windows — a provider-wide
-- schedule (DeepSeek's peak hours apply to every one of its models, not one
-- specific model), so it lives on the provider, not the offering. NULL/""
-- means no peak/off-peak concept for this provider (internal/pricing.Parse
-- treats "" as the zero value); every request is priced at the base rate.
ALTER TABLE router_providers ADD COLUMN peak_windows TEXT;

-- price_tier records which tier ("peak"/"off_peak"/"flat") was in force
-- when a usage event's cost was computed, so historical reporting
-- (compressor_summary_handlers.go's savings estimators) can price past
-- events accurately instead of re-deriving from today's prices, and so an
-- operator can audit a recorded cost against the provider's invoice. NULL
-- for every event recorded before this migration or for an event whose
-- pricing wasn't resolvable at all (mirrors CostNative's existing nil
-- convention).
ALTER TABLE usage_events ADD COLUMN price_tier TEXT;

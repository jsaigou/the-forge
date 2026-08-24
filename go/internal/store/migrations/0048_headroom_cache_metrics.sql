-- 0048: Add cache-bust columns to headroom_samples and delta_f to
-- headroom_label_samples, enabling the scraper to persist provider cache
-- token metrics and per-compressor timing that were available since
-- headroom-ai 0.30.0 but never scraped.
--
-- cache_read_tokens and uncached_tokens already exist on headroom_samples
-- (from 0021, rebuilt in 0042) but were hardcoded to 0 in the INSERT —
-- the RecordHeadroomSample query is updated alongside this migration to
-- populate them from headroom_cache_read_tokens_total{provider} and
-- headroom_uncached_input_tokens_total{provider}.

ALTER TABLE headroom_samples ADD COLUMN cache_busts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE headroom_samples ADD COLUMN cache_bust_tokens_lost INTEGER NOT NULL DEFAULT 0;

-- delta_f holds float-valued labelled metric deltas (e.g.
-- headroom_transform_timing_ms_sum). Nullable: existing int-valued rows
-- stay NULL; new float-valued rows set it, new int-valued rows leave it NULL.
ALTER TABLE headroom_label_samples ADD COLUMN delta_f REAL;

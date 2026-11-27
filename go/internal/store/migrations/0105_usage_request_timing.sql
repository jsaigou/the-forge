-- SPDX-License-Identifier: Apache-2.0
-- Request timing on usage_events, so real concurrency per model (and per consumer) can be measured from stored intervals.
-- Until now an event had only a completion timestamp (ts, 1 s resolution): no start, no duration, no caller.
--
--   started_ms   request arrival at a0, unix milliseconds (the interval is [started_ms, started_ms + duration_ms])
--   duration_ms  arrival to the last byte relayed / the stream closing, also when the client aborted or the request failed
--   outcome      'ok' | 'error' | 'aborted'
--   consumer     the caller's display label (key DisplayName, or key name + user agent, or a remote address). Never the key
--                itself or any secret, and never prompt or response content.
--   wait_ms      arrival to the moment the upstream attempt was dispatched (gating, on-demand load); NULL if never dispatched
--
-- Additive and nullable: no table rebuild, no backfill. Rows written before this migration keep NULL here and are excluded
-- from concurrency figures (they have no interval); every existing column keeps its meaning. a0 writes these on rows of
-- kind 'a0_request' (one per proxied request); the other kinds never set them.
ALTER TABLE usage_events ADD COLUMN started_ms  INTEGER;
ALTER TABLE usage_events ADD COLUMN duration_ms INTEGER;
ALTER TABLE usage_events ADD COLUMN outcome     TEXT;
ALTER TABLE usage_events ADD COLUMN consumer    TEXT;
ALTER TABLE usage_events ADD COLUMN wait_ms     INTEGER;
CREATE INDEX idx_usage_events_started_ms ON usage_events(started_ms) WHERE started_ms IS NOT NULL;

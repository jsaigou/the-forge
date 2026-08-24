-- Sprint 7 (v1.1 post-launch hardening, docs/v5-headroom-replacement.md): full
-- rename of "Headroom" (the retired third-party proxy) to "compressor" (what
-- actually runs now, foundry-compress) across the schema. Sprint 4 already
-- named its own new additions "compressor" (compressor_samples, RSS/restart
-- health) without renaming this older surface, so the schema carried both
-- names inconsistently before this migration. Naming map + decisions:
-- docs/v5-headroom-replacement.md Sprint 7.
--
-- Table renames auto-update every REFERENCES clause that points at the old
-- name (SQLite ALTER TABLE RENAME TO, default behavior since 3.25 unless
-- PRAGMA legacy_alter_table is set — it isn't here), so compressor_samples'
-- own FK to headroom_proxies(id) is fixed up by the first rename below
-- without a separate statement.
ALTER TABLE headroom_proxies RENAME TO compressor_proxies;
ALTER TABLE headroom_samples RENAME TO compressor_savings_samples;
ALTER TABLE headroom_label_samples RENAME TO compressor_label_samples;
-- headroom_savings is NOT dead (Sprint 6 found main.go:517 writes it every
-- collector cycle, and Settings -> Headroom's "tokens saved 7d" chip reads
-- it) — kept as its own table, renamed rather than folded into
-- compressor_savings_samples, which tracks different columns.
ALTER TABLE headroom_savings RENAME TO compressor_savings_totals;

-- Indexes are not renamed automatically by ALTER TABLE RENAME TO.
DROP INDEX idx_headroom_proxies_service;
CREATE UNIQUE INDEX idx_compressor_proxies_service ON compressor_proxies(service);
DROP INDEX idx_headroom_proxies_provider;
CREATE UNIQUE INDEX idx_compressor_proxies_provider ON compressor_proxies(provider_id)
    WHERE provider_id IS NOT NULL AND orphaned_at IS NULL;
DROP INDEX idx_headroom_savings;
CREATE INDEX idx_compressor_savings_totals ON compressor_savings_totals(proxy_id, ts);
DROP INDEX idx_headroom_label_samples;
CREATE INDEX idx_compressor_label_samples
    ON compressor_label_samples(proxy_id, label_key, label_value, ts);
-- compressor_savings_samples (old headroom_samples) has never had an index
-- of its own since the 0042 surrogate-key rewrite dropped the pre-surrogate
-- idx_headroom_samples and never recreated it for the new proxy_id-keyed
-- table — a pre-existing gap, out of Sprint 7's scope, carried forward
-- unchanged rather than silently fixed alongside an unrelated rename.

-- Settings keys.
UPDATE settings SET key = 'compressor.local_enabled', updated_at = updated_at
    WHERE key = 'headroom.local_enabled';
UPDATE settings SET key = 'compressor.external_enabled', updated_at = updated_at
    WHERE key = 'headroom.external_enabled';
UPDATE settings SET key = 'compressor.passthrough_all', updated_at = updated_at
    WHERE key = 'headroom.passthrough_all';

-- auth.policy is a JSON blob (resource key -> assurance factor); rewrite the
-- two renamed resource keys in place. Both substrings are unique within the
-- blob (verified against the live shape: "page.headroom":"...",
-- "action.headroom.teardown":"...").
UPDATE settings
    SET value = REPLACE(
            REPLACE(value, '"page.headroom"', '"page.compression"'),
            '"action.headroom.teardown"', '"action.compressor.teardown"'
        )
    WHERE key = 'auth.policy';

-- smith_findings.check_id: headroom_health -> compressor_reachability.
-- Rewritten (not left orphaned) so existing history stays attached to the
-- renamed check — operator's explicit call, Sprint 7 planning.
UPDATE smith_findings SET check_id = 'compressor_reachability'
    WHERE check_id = 'headroom_health';

-- smith_actions.title/detail/dedupe_key/result on historical rows are left
-- untouched deliberately — they're a record of what was actually proposed
-- at the time, same principle as CLAUDE.md's historical prose staying as
-- written after a rename.

-- SPDX-License-Identifier: Apache-2.0
-- Schema v71 — smith.build_refresh.watchlist seam (S6 phase 2 of
-- docs/v5-ops-sprints-2026-08-21.md, feedback F1's second half — commit-
-- subject fetching + the user-editable watchlist).
--
-- binary_versions (checks_binaries.go) now optionally fetches upstream
-- commit SUBJECT lines (Deps.GitBehindLog, same HEAD..upstream_ref range
-- UpstreamAhead already counts) whenever there is measurable drift AND this
-- watchlist is non-empty, and flags any subject containing a watchlist
-- keyword (case-insensitive substring) — visibility only, surfaced in the
-- finding's summary/evidence regardless of whether the raw commit count has
-- crossed smith.thresholds.build_refresh_behind_n. It does not itself widen
-- proposeRebuildRunbook's threshold gate.
--
-- TWO-LAYER KNOWLEDGE ARCHITECTURE (same rule as 0061's forks seam): ships
-- EMPTY, no deployment-specific keywords in source. The operator maintains
-- this list per-install (Settings → smith → Reasoning →
-- WatchlistCard, web/src/settings/panels/Smith.tsx); smith itself may also
-- append to it, auto-populated from failed model-add compatibility
-- failures (the original design intent — free-form investigation-matching
-- was explicitly rejected in favor of this narrower, explicit list).

INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.build_refresh.watchlist', '[]', strftime('%s', 'now'));

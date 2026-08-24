-- SPDX-License-Identifier: Apache-2.0
-- Schema v44 (smith P7 retention sprint, 2026-08-14).
--
-- migration 0038 seeded smith.tailscale.watch_peers with ["pairnode","core"].
-- pairnode is not relevant to this deployment build, and a non-empty default
-- seed that no longer reflects reality produces noise on every deep sweep
-- (an offline peer the operator never opted into watching). Clear the seed
-- to [] ("nothing to watch" by default); an operator who wants peer watching
-- adds hostnames back via Settings. This is an UPDATE, not INSERT OR IGNORE,
-- because the whole point is to overwrite the now-wrong 0038 default — a
-- fresh install gets [] and an upgraded install drops pairnode. If an
-- operator has already curated their own list, they can re-add it after the
-- upgrade; the default changing to "nothing watched" is the safer direction
-- (a missed offline peer) than silently watching a peer that was never meant
-- to be on the list.
UPDATE settings SET value = '[]', updated_at = strftime('%s', 'now')
    WHERE key = 'smith.tailscale.watch_peers';

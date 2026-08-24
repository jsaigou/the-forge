-- SPDX-License-Identifier: Apache-2.0
-- Schema v47 (smith web research — custom providers, operator feedback
-- 2026-08-14: "add options for a custom search provider and a custom fetch
-- provider"). Two generic adapter slots — customsearch (a SearxNG-compatible
-- GET ?q= endpoint) and customfetch (a Jina-reader-style GET ?url= proxy) —
-- so an operator can point smith at any compatible endpoint without a
-- per-service adapter. Seeded disabled with empty base URLs, matching the
-- 0037 "seeded once, no hardcoded default in code" convention.
INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.web.customsearch', '{"base_url":"","enabled":false,"api_key":""}', strftime('%s', 'now')),
    ('smith.web.customfetch', '{"base_url":"","enabled":false,"api_key":""}', strftime('%s', 'now'));

-- Add the new adapters to the default provider_order so they're reachable in
-- the chain once enabled (searchOrder/fetchOrder only consider adapters
-- listed in the saved order). Only rewrites the exact seeded value; an
-- operator-customized order is left untouched (add via Settings → Smith).
UPDATE settings SET value = '["searxng","customsearch","firecrawl","customfetch","direct"]',
        updated_at = strftime('%s', 'now')
    WHERE key = 'smith.web.provider_order'
      AND value = '["searxng","firecrawl","direct"]';

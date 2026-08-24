-- BE-3 fix (docs/v5-review-fixes.md F4): AI&'s Analytics API requires an
-- X-Org-ID header alongside the API key (confirmed against real ai& docs,
-- docs.aiand.com/analytics/metrics/ "Auth" section) -- no existing column
-- carries this. "" = not configured (credits client degrades to
-- Supported:false for providers whose client needs it but it's unset).
ALTER TABLE router_providers ADD COLUMN org_id TEXT NOT NULL DEFAULT '';

-- SPDX-License-Identifier: Apache-2.0
-- Schema v3 (V5 polish sprint — docs/v5-sprint0-contract-freeze.md §0.10).
-- Renames the default headroom proxy "external" → "aiand" in existing V5
-- installs. Fresh installs seeded by migrate-v4 already use "aiand"; this
-- migration is for DBs that were seeded before the rename. Service names are
-- data (no API shape change); this just updates the data.
--
-- Touch points: headroom_proxies.service + .unit, router_providers.headroom_proxy,
-- headroom_proxies.provider (if a provider is linked to "external"), and the
-- headroom.passthrough_services JSON array in settings.

-- Rename the proxy row itself. The service is the PK; the unit follows the
-- convention headroom-<service>.
UPDATE headroom_proxies SET service = 'aiand', unit = 'headroom-aiand'
WHERE service = 'external';

-- Update any provider linked to the old proxy name.
UPDATE router_providers SET headroom_proxy = 'aiand'
WHERE headroom_proxy = 'external';

-- Update the headroom_proxies.provider link if it pointed at a provider
-- named "external" (unlikely but idempotent if so).
UPDATE headroom_proxies SET provider = 'aiand'
WHERE provider = 'external';

-- Rename "external" → "aiand" inside the headroom.passthrough_services JSON
-- array in settings. The value is a JSON array of strings like ["deepseek",
-- "external"]; we do a targeted replace. SQLite's JSON functions are not
-- available in all builds, so we use a string replace on the raw JSON value.
-- This is safe because "external" as a bare word only appears as a JSON
-- string value in this array (not as a key or substring of another value).
UPDATE settings SET value = replace(value, '"external"', '"aiand"')
WHERE key = 'headroom.passthrough_services';

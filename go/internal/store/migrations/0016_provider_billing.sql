-- SPDX-License-Identifier: Apache-2.0
-- Schema v16 (product/QA sprint, 2026-07-29 — provider billing links + toggle).
-- billing_enabled: per-provider on/off for the credits/balance API. When 0,
-- providers.creditsClients.fetch (credits.go) must short-circuit to
-- Credits{Supported:false} without probing, so the Console's provider chip
-- renders its credits/spend region blank rather than stale.
-- billing_console_url: the human billing dashboard link (e.g.
-- https://platform.deepseek.com/usage) shown on the provider chip — distinct
-- from credits_url, which is the machine balance API endpoint. Both are
-- user-editable; billing_console_url has no auto-discovery (nothing to probe
-- for a human-facing page).
ALTER TABLE router_providers ADD COLUMN billing_enabled     INTEGER NOT NULL DEFAULT 1;
ALTER TABLE router_providers ADD COLUMN billing_console_url TEXT    NOT NULL DEFAULT '';

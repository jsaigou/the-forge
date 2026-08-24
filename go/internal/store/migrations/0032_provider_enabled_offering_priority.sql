-- SPDX-License-Identifier: Apache-2.0
-- Schema v32 (multi-provider routing sprint, 2026-08-06).
--
-- Two additions for the "same model on several providers" edge case:
--
-- router_providers.enabled — disable a provider without deleting it. The
-- router skips a disabled provider's offerings entirely (selection, /v1/models
-- listing, and request routing), and the providers service stops refreshing
-- its health/credits cache. Everything else (credentials, offerings, linked
-- Headroom proxy) is preserved so re-enabling restores the provider exactly.
-- Default 1 keeps every existing provider routing exactly as before.
ALTER TABLE router_providers ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;

-- offerings.priority — operator preference among the offerings of one model.
-- When several providers offer the same catalog model, the router presents
-- and routes the offering with the LOWEST priority value (1 beats 100 —
-- same lower-first convention as slots.sort_order); ties break by provider
-- name then offering id so the outcome is always deterministic. Default 100
-- leaves headroom to promote specific offerings (e.g. 10) without rewriting
-- every row.
ALTER TABLE offerings ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;

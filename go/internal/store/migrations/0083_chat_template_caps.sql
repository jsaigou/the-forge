-- SPDX-License-Identifier: Apache-2.0
-- Schema v83 (per-request thinking control, Sprint T1, 2026-09-14). Persists
-- llama.cpp's /props chat_template_caps probe per config, captured at the
-- end of every successful load (engine.Manager, best-effort) rather than
-- read live-only the way Part 1's ttlCatalog.Probe already does for the
-- tools gate — a config's card must show its capabilities even when
-- nothing is currently loaded on it, and Part 2's reasoning_effort
-- translation layer needs a stored fact to consult per config, not just a
-- live peer probe.
--
-- chat_template_caps is the raw probe result, keyed by llama.cpp's own
-- field names (e.g. "supports_reasoning_effort", "supports_tool_calls")
-- rather than a fixed set of columns — the field set is genuinely
-- build-dependent (reasoning_effort is present in every kintsugi build but
-- absent from three others, confirmed live 2026-09-13) and a fixed schema
-- would silently drop whatever a future llama.cpp build adds.
-- chat_template_caps_probed_at is NULL until the first successful load.
-- chat_template_caps_override is an operator-curated correction for the
-- cases the live probe gets wrong (a build misreports a capability, or a
-- capability the probe can't observe at all) — merged over the probed
-- value per key by Config.EffectiveChatTemplateCaps, never replacing it
-- wholesale, so an unprobed config still keeps every other probed fact.
ALTER TABLE configs ADD COLUMN chat_template_caps TEXT NOT NULL DEFAULT '{}';
ALTER TABLE configs ADD COLUMN chat_template_caps_probed_at INTEGER;
ALTER TABLE configs ADD COLUMN chat_template_caps_override TEXT;

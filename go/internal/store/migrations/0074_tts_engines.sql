-- SPDX-License-Identifier: Apache-2.0
-- Schema v74 — tts.engines seam (Tier 1 Sprint 2, Voice & Speech settings,
-- 2026-08-27). forge-tts was the one v0.5 service still configured purely by
-- process env; this seam lets the daemon's ttsctl.Provisioner
-- (go/internal/ttsctl) write forge-tts's env file from a store-backed
-- config instead, with enable/resident/available/disabled control per
-- engine (kokoro fast-tier + the three Qwen sub-variants).
--
-- TWO-LAYER KNOWLEDGE ARCHITECTURE (same rule as 0071/0072/0073): ships with
-- enabled/mode set to the product-sensible defaults, but Unit/URL always
-- empty — systemd unit names and localhost ports are per-install
-- environment facts, never product knowledge. The operator fills them in
-- per install (Settings → Voice & Speech, web/src/settings/panels/Voice.tsx)
-- before residency actually takes effect; until then every engine behaves
-- as "available" (the existing CLI-fallback path) regardless of the
-- configured mode, since ttsctl.Provisioner has nothing to start.

INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('tts.engines', '{"kokoro":{"enabled":true,"mode":"resident","unit":"","url":""},"customvoice":{"enabled":true,"mode":"resident","unit":"","url":""},"voicedesign":{"enabled":true,"mode":"available","unit":"","url":""},"base":{"enabled":true,"mode":"resident","unit":"","url":""}}', strftime('%s', 'now'));

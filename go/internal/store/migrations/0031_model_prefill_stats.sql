-- SPDX-License-Identifier: Apache-2.0
-- Schema v31 (Headroom local-savings prefill sprint, 2026-08-06).
--
-- Headroom's local time-saved estimate is purely an avoided-re-prefill
-- figure (Headroom does not touch decode/generation at all), so it needs a
-- real, durable, per-model PREFILL tok/s — never a decode/generation
-- number, and never a fabricated constant (see the sprint plan and
-- docs/progress.md's 2026-08-06 entries for the incident this fixes: a flat
-- 50 tok/s "generation" fallback produced a 493-hour "saved" figure inside
-- a 168-hour window).
--
-- llama-server's own /metrics exposes cumulative counters
-- llamacpp:prompt_tokens_total and llamacpp:prompt_seconds_total (verified
-- directly against ForgeHost's real llama.cpp source,
-- tools/server/server-context.cpp). The collector already scrapes the
-- former every ~4s; this sprint adds the latter and accumulates
-- token/second deltas into a durable per-model rolling aggregate here.
--
-- Deliberately a rolling AGGREGATE, not a retention-pruned sample log:
-- typical prefill speed is a property of the model+config, not of a query
-- window, and must never "expire" just because raw samples aged out.
-- Keyed by (mode, fingerprint) — fingerprint reuses
-- internal/profile.Runner.Fingerprint's existing, proven staleness concept
-- (hashes model path/size/quant/n_ctx/backend/binary/extra_args), so a
-- config change starts a fresh accumulation instead of silently blending
-- two different performance regimes into one average.
-- first_seen/last_seen are UnixNano (not whole seconds): ByMode picks each
-- mode's current-regime row by "greatest last_seen", and whole-second
-- resolution could plausibly tie between two fingerprints observed close
-- together (right after a config change).
CREATE TABLE model_prefill_stats (
    mode           TEXT    NOT NULL,
    fingerprint    TEXT    NOT NULL,
    prompt_tokens  INTEGER NOT NULL DEFAULT 0,
    prompt_seconds REAL    NOT NULL DEFAULT 0,
    samples        INTEGER NOT NULL DEFAULT 0,
    first_seen     INTEGER NOT NULL,
    last_seen      INTEGER NOT NULL,
    PRIMARY KEY (mode, fingerprint)
);

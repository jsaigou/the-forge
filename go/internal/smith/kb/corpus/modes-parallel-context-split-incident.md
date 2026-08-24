ref: modes:parallel-context-split-incident
doc: modes
slug: parallel-context-split-incident
title: The `--parallel` context-splitting incident (2026-07-23)
category: config
source: docs/modes.md

## The `--parallel` context-splitting incident (2026-07-23)

`--parallel N` in llama.cpp does **not** allocate N× the configured context — it
**splits** the single configured `context` value across N slots. `/props`'
`default_generation_settings.n_ctx` (what the engine reads to verify a load) reports
the **per-slot** value: `configured_context / parallel`. Confirmed against llama.cpp
source on this deployment (`tools/server/server-context.cpp`: `n_ctx_slot =
llama_n_ctx_seq(ctx_tgt)`, and `get_props`'s `default_generation_settings.n_ctx =
meta->slot_n_ctx`).

The PROFILE track's profiler (`docs/v5-profiling-benchmarks.md`) discovered this by
accident: it hard-aborts when actual `n_ctx` < configured, and flagged `gemma4-e2b` and
`nemotron-nano` (both then `--parallel 2`) as "silently reduced" — 131072 configured,
65536 actual, exactly half. That diagnosis was **half right**: the model genuinely
never got the full context per conversation (real, user-visible degradation — every
request was capped at 65536 tokens, not 131072), but the *cause* wasn't a GTT/kernel
allocation failure at all. It was working exactly as `--parallel 2` is documented to
work.

Root-caused live (loaded `gemma4-e2b` on an idle slot, read `/props`: `total_slots: 2`,
`default_generation_settings.n_ctx: 65536` — exactly `131072/2`). Not a V4→v0.5 migration
bug — `--parallel 2` for these modes is identical in the old V4 `config.toml` (still on
disk) and v0.5's `forge.toml`; `migrate_v4.go` carries `extra_args` over verbatim, and
V4's Python `_verify_model_context` (`forge/engine.py`) has the same
un-parallel-aware comparison. This false "silent GTT reduction" diagnosis predates v0.5
entirely; it just never blocked anything until the profiler's new hard-abort made it
visible.

**Operator decision:** models should load at their full trained context in a single
slot — no request-splitting, since `--parallel > 1` also costs per-request throughput
(compute split across concurrent slots instead of dedicated to one). Fixed live on the
deployment host (`systemctl kill -s HUP forge-daemon`, no restart) by setting `--parallel 1` on
all 4 modes that had `--parallel > 1`: `gemma4-e2b` (was 2), `nemotron-nano` (was 2),
`qwen3coder-q6` (was 2), `swallow-8b` (was 4) — see the corresponding fixed-live
entry in the deployment's progress log for the live-verification detail (both reported modes
reloaded post-fix, confirmed `total_slots: 1` + full configured `n_ctx` in `/props`).
`qwen25coder` keeps `--parallel 4` deliberately (short 32K-context fast-worker mode,
genuinely meant for concurrent short requests, not one long conversation).

**Caveat for future `migrate-v4` re-runs:** V4's `config.toml` was *not* updated to
match — it still has the old `--parallel` values for these 4 modes. If `forge
migrate-v4` is ever re-run from V4's config, this fix would need to be re-applied.

**Separately:** `qwen36`'s long-standing context reduction (524288→262144, documented
since 2026-05, `--parallel 1` so unaffected by the bug above) remains unexplained — a
real, un-investigated GTT/kernel issue at that context size. Do not assume the
`--parallel` fix above resolves it.

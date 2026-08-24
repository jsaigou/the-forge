ref: modes:gemma4-31b-mtp-oom-resolved
doc: modes
slug: gemma4-31b-mtp-oom-resolved
title: `gemma4-31b-mtp` OOMs at full context — RESOLVED (2026-07-29)
category: memory
source: docs/modes.md

## `gemma4-31b-mtp` OOMs at full context — RESOLVED (2026-07-29)

**Root cause confirmed, fixed, deployed to the catalog DB, live-verified.** Original
diagnosis (below, kept for history) was correct: `--swa-full` was the driver, not model
size. Full resolution:

**What "changed"? Nothing — this was never actually working.** `--swa-full` was
blanket-applied to all Gemma4 modes on 2026-07-02 (`1dc65bf`) and "live-verified" —
but that verification only ever covered the smaller `gemma4-mtp-nothink` (26B MoE).
`gemma4-31b-mtp` (31B dense) was never pushed to its own full `262144`-token context
until PROFILE — a tool that didn't exist until the following week — did so for the
first time on 2026-07-24. v0.5's `mode_history` table (the only hard record of load
attempts, recording starts ~2026-07-22/23) shows the *first ever* load attempts at
full context for this mode are the two failures on 2026-07-24. This was a dormant,
always-broken config value, not a regression. The "kernel panic" that got attached to
this mode's reputation in a later (2026-07-28) sweep was a **separate, unrelated,
now-fixed bug**: `nemotron` silently launching on the vulkan binary instead of its
configured rocm backend (fixed via the `builds.backend` column + `repair-catalog-backend`,
`5b84863`) — `gemma4-31b-mtp`'s own backend was correctly `vulkan` the whole time and
was never affected by that bug; it just happened to be profiled in the same session.

**Upstream research (llama.cpp GitHub, checked 2026-07-29):** confirmed this is a real,
known, currently-unfixed memory/performance tradeoff, not a bug with a hidden patch.
- [discussions/24543](https://github.com/ggml-org/llama.cpp/discussions/24543) covers
  this *exact* model (Gemma 4 31B) hitting the same class of OOM with `--swa-full`; the
  maintainer's own words: `--swa-full` "keeps the whole history instead of the sliding
  window" — genuine full, non-windowed retention, not a bug. Same reporter: fits
  comfortably in 64GB without it.
- [issues/23978](https://github.com/ggml-org/llama.cpp/issues/23978) (`--swa-full`
  breaks KV cache quantization, ballooning to f16-equivalent size) looked like it might
  explain the *scale* of our OOM, but is a red herring for us — fixed upstream by
  [PR #23907](https://github.com/ggml-org/llama.cpp/pull/23907) (merged 2026-06-03),
  confirmed present in this deployment's build (`8bf3c1130`, 2026-07-25) via `git merge-base
  --is-ancestor`. Ruled out.
- [issues/21831](https://github.com/ggml-org/llama.cpp/issues/21831) (the general
  "forcing full prompt re-processing" bug `--swa-full` exists to work around) is still
  open as of 2026-07-28 — root-caused there as **exclusive to hybrid/recurrent
  architectures** (`qwen35moe`/Ornith, `gemma4moe`/the 26B-A4B MoE Gemma4 modes).
  `gemma4-31b-mtp` is dense (no SSM/recurrent layers, confirmed by our own crash log's
  `llama_kv_cache_iswa: using full-size SWA cache` line — the genuine-SWA code path, not
  the hybrid one) so `--swa-full` is a real, correctly-functioning fix for it, just an
  expensive one at large context.
- `--ctx-checkpoints`/`--swa-checkpoints` (confirmed via `--help` to be the *same flag*,
  just aliased) is still not a safe alternative:
  [issues/24265](https://github.com/ggml-org/llama.cpp/issues/24265) (hard-hangs on a
  Gemma SWA/hybrid model with checkpoints near the RAM limit) closed stale
  2026-07-23 with no fix, corroborating this repo's own existing caution
  (`docs/investigations.md` item 9). Do not enable it for this mode.

**The fix:** the mode's `262144` context was simply never sized to what `--swa-full`
actually costs — not a bug, an unvalidated config value inherited by copy from the other
three Gemma4 modes (all of which are smaller and/or lower-context and are fine).
Live-tested on the free `a4` slot (never evicting other slots):
- `--swa-full` **kept on** (preserves cache reuse — verified live: a follow-up turn in
  the same slot showed `prompt_tokens_details.cached_tokens: 33` for all 33 prior-turn
  tokens, with `selected slot by LCP similarity` in the log and no `forcing full prompt
  re-processing` — the flag is doing its job correctly at this size).
- `n_ctx` reduced `262144` → **`131072`** (a round number already proven safe elsewhere
  in the fleet, not a rigorously-found ceiling — a tighter binary search was
  deliberately not pursued once 131072 was validated as sufficient headroom).
- Backend: tested `vulkan` vs `rocm` (this deployment's ROCm builds are ROCm 7.15
  via TheRock, confirmed via `ldd` on both rocm build binaries — not the older ROCm 7.13
  comparison that used to favor vulkan by default). Real head-to-head generation
  benchmarks at the final config: vulkan avg 115.6 prefill / 24.4 decode tok/s; rocm 7.15
  avg 117.9 prefill / 24.3 decode tok/s — a statistical tie (run-to-run variance within
  each backend exceeded the gap between them). **Decision (explicit, from the operator):
  prefer ROCm 7.15 over Vulkan whenever performance is tied and there's no compatibility
  issue** — ROCm fails an OOM cleanly inside the process (a normal `cudaMalloc failed`
  error); Vulkan can silently OOM-kill the whole adjacent `forge-primary.service` via
  the kernel OOM-killer, which is exactly the failure mode that clouded this
  investigation for days. This is now a standing tiebreaker for future backend choices
  on this fleet, not a one-off for this mode.

**Final config (live in the catalog DB, `configs.id=2`):** `build_id=6` (`standard-rocm`,
ROCm 7.15), `n_ctx=131072`, `extra_args` unchanged otherwise (still includes
`--swa-full`, `--spec-type draft-mtp`, etc.). Not currently loaded on any slot (tested
on the free `a4` slot, then unloaded to restore it to empty).

**Still open, lower priority:**
1. The `131072` ceiling wasn't binary-searched — a tighter number (e.g. ~180-200K) may
   also fit; only worth chasing if the smaller context becomes a real constraint.
2. Re-verify `gemma4-mtp` (26B MoE, same `--swa-full`) isn't hiding the same risk — it
   *has* loaded successfully at its own full `262144` context twice since this was first
   flagged (`mode_history` ids 58, and the original 2026-07-23 load), so this is probably
   fine, just never explicitly re-confirmed as "checked, not just lucky."
3. **RESOLVED (2026-07-29, same day):** the operator reported triggering a PROFILE run on
   `laguna-s-21` that "never finished and did not display an error — it just disappeared
   from the dashboard," sitting loaded for 2h27min before being SIGKILL'd. Investigated
   and root-caused — **not a PROFILE bug and not a genuine hang.** `internal/engine`'s
   `killLingering()` (called at the end of every `Unload()`) was scoped to *all four*
   canonical slot ports instead of just the port of the slot actually being unloaded, so
   any unrelated single-slot unload elsewhere on the box would silently `SIGKILL`
   whatever was running on *every other* slot too — systemd's `Restart=` masked it as a
   brief availability blip rather than a visible failure, the same pattern as the
   original aux-service collateral-damage bug this function was already once fixed for
   (see the doc comment on `killLingering`). A routine, unrelated slot-unload call during
   the operator's PROFILE run collaterally killed it mid-request; PROFILE itself never
   had a chance to detect a hang because there wasn't one. Live-reproduced with a clean
   two-slot test on this host (load two unrelated modes on two slots, unload one, watch the
   other silently die and auto-restart), fixed (`killLingering` now takes explicit
   `ports ...int` — `Unload(slot)` passes only that slot's own port, `stopAll` passes
   every port), regression-tested (`TestKillLingeringDoesNotTargetOtherSlots`), full
   `go build/vet/test ./...` clean. See `go/internal/engine/lifecycle.go`.
4. **Two unrelated, pre-existing issues noticed while checking live slot state**, neither
   caused by this session: `a2` (`secondary` at the time, `swallow-8b`) has been down
   since a `SIGKILL` (OOM-killer signature) on 2026-07-23, never restarted; `slot_state`
   in the DB was stale for both `a2` and `a3` at the time (doesn't reflect their real down
   status) — a live instance of the exact desync class the 2026-07-21 status/state fix was
   supposed to have closed. (The `slot_state` staleness itself was fixed later the same
   session — a 5-minute background sync, see below — and the internal slot keys
   `primary`/`secondary` were renamed to `a1`/`a2` on 2026-07-29, see the slot-rename
   entry below.)

---

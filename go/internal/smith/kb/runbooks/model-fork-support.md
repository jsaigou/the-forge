ref: build:model-fork-support
doc: build
slug: model-fork-support
title: Does this model need a special llama.cpp fork? (decision procedure)
category: build
source: hand-authored corpus entry (2026-08-17 — qwen38-27b/puzzle investigation)

When evaluating whether a new model needs a fork of llama.cpp (vs a stock
build), follow this decision procedure. It's what the 2026-08-17 session used
for qwen38-27b (no fork needed) and nemotron-puzzle (fork still required).

## 1. Check the model's architecture against mainline llama.cpp

- Read the GGUF's `general.architecture` (the string, e.g. `qwen35`). You can
  find it near the start of the file header (search for `general.architecture`
  in the first KB of the GGUF).
- Determine whether that architecture is supported in current upstream. The
  local kintsugi tree tracks upstream — check whether the arch name appears
  in `src/llama-arch.h` and has a model implementation in `src/llama-model.cpp`
  at HEAD:
  ```bash
  cd <fork-root>
  git show HEAD:src/llama-arch.h | grep -i '<arch>'      # LLM_ARCH_* enum
  git show HEAD:src/llama-model.cpp | grep -n '<arch>'   # loader case
  ```
- Important: support is measured against CURRENT upstream, not the fork's
  base. An arch that was fork-only six months ago is often mainline now
  (e.g. `qwen35` became mainline 2026-02-10 via #19468; MTP became mainline
  2026-05-16 via #22673). Re-check, don't assume.

## 2. If mainline supports it, try a stock build

The simplest test: does a stock (non-fork) llama.cpp build load the model and
run a completion? If yes, the model does NOT need the fork — it only *benefits*
from fork features (see step 4). The 2026-08-16 qwen38-27b conclusion was
exactly this: arch `qwen35` was mainline, so the kintsugi fork wasn't
required to run it — the fork only added cross-turn KV cache reuse for
hybrid/recurrent models.

## 3. Model-format compatibility can still force a fork even when arch is mainline

The 2026-08-17 nemotron-puzzle finding: the arch (`nemotron_h_moe`) was
mainline, but the model's GGUF stores `nemotron_h_moe.expert_used_count` as a
**per-layer array**, and current upstream reads it as a scalar (`get_key`),
rejecting the file with "wrong type arr but expected type u32". The puzzle
fork tolerates both forms (`get_key_or_arr` + `max` over the array). So:

- A mainline arch does NOT guarantee a mainline model file loads.
- When a load fails on a metadata type error, search the fork for a
  `get_key_or_arr` / array-tolerant read of that key — that's often the
  real reason the fork exists.
- The fix is either (a) keep the fork, or (b) re-quant/re-export the GGUF so
  the key is a scalar.

## 4. Fork features vs fork requirements

A fork can be *valuable* without being *required*:

- Required: model won't load / crashes without it (metadata incompatibility,
  missing arch, the Poolside laguna fork's `GGML_HIP_ROCWMMA_FATTN=ON` need).
- Optional (performance): the kintsugi fork's cross-turn KV cache reuse for
  hybrid/recurrent models (avoids full-conversation reprocessing every turn —
  big win on long agentic sessions, no difference on single-shot). qwen38-27b
  is arch `qwen35` (hybrid SSM+attention): it runs on stock llama.cpp but runs
  *better* on kintsugi for multi-turn workloads.

## 5. Check whether the fork's unique patches have been upstreamed

If the fork exists for a specific feature/fix, search whether upstream merged
an equivalent:

```bash
cd <fork-root>
git log --oneline origin/master -S '<unique-string-from-the-patch>'
git show origin/master:<file-the-patch-touches> | grep -i '<concept>'
```

The 2026-08-17 finding: the puzzle fork's MTP KV-cache fix (`af49ef5cd`) was
fully upstreamed (`mtp_on_hybrid_nemotron` in current `llama-model.cpp`) — but
the fork was still needed for the unrelated `expert_used_count` array issue.
Evaluate each reason independently; a fork can be half-retired.

## 6. When a build is actually needed — the procedure

Use `runbook:build-refresh` — the full rebase → dual-build (vulkan+rocm) →
reliability+perf test → promote → repoint → SELinux-relabel deploy procedure,
with every command, gotcha (RPATH `$ORIGIN`, clean-env ldd, `bin_t` relabel),
and verification step.

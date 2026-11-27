# Retire the Kintsugi patch set: run plain upstream llama.cpp

Status: accepted (2026-11-01), implemented the same day: the patches are retired and all nine ROCm configs that
ran on build 16 now run on build 19 (plain upstream), each tested against its own baseline (see "Migration
results"). Builds 9/10/15/16/17 remain as rollback rows until retired. Supersedes ADR-0018's decision to keep
carrying two patches; ADR-0018's per-build flag spelling and registrar rules still stand.

## Context

ADR-0018 left the `jsaigou/llama.cpp` fork carrying two commits on top of upstream:

1. `091c95b9c` "Kintsugi": cross-turn KV cache reuse for hybrid/recurrent models. It defaults `n_rs_seq` to 4,
   defaults `n_ctx_checkpoints` to 4 when 0, saves a generation-end checkpoint per turn and prefers it on restore,
   synchronizes after a partial-state restore, and keeps checkpoints across a forced reset.
2. `ce24471ca`: the same generation-end checkpoint, extended to the speculative-decoding accept path
   (including an EOG pre-scan), for hybrid models with MTP.

On 2026-11-01 upstream was 215 commits past the 2026-10-22 rebase base (`7170cad4d`) and had merged support our
builds lacked (LFM3 among it). Before rebasing again we asked whether the patches were still needed at all.

## Review of the code

Reading upstream `a91eab67` against the two patches:

* No upstream commit adds a generation-end checkpoint; `create_checkpoint` still has a single call site, during
  prompt processing.
* Upstream now defaults `n_ctx_checkpoints` to 32, and every one of our configs sets 16, so the patch's
  "default to 4 when 0" never fires.
* Upstream changed the neighbouring code: `ab3768cd` makes recurrent memory refuse rollbacks past the last
  ubatch's snapshots, and `7df8b876` changed the spec-checkpoint condition from `n_rollback` to `n_draft`.
  Both cherry-picks still applied with no conflicts, which proves nothing about behaviour.

Reading could not settle it, so we measured.

## Measurement

Probe: a ~35K-token document, then three follow-up turns that resend the whole history (reasoning included where
the client would), through a0. The number is `timings.prompt_n`, the prompt tokens the server actually
re-evaluated on each follow-up; a miss would be about 35,000. Probe script: `docs/history/kintsugi/reuse_ab.py`. Two runs per cell unless noted.

| Scenario | Old build 16 + Kintsugi | Rebase + Kintsugi (build 17) | Plain upstream (build 19) |
|---|---|---|---|
| Qwen3.6 MTP, reasoning preserved | 16-30 (3 runs; 1 miss in 9 turns) | 16-30 | 17-30 |
| Ornith, no MTP, reasoning dropped | 40-172 | 40-119 | 27-105 |
| Flash-Next (qwen4exp, MTP, flagship) | 17-30 | not run | 16-30 |
| 187K-token prompt (Qwen3.6 MTP) | not run | not run | 17, 20 |

Decode and prefill speeds were the same across builds (Qwen3.6 MTP 56-69 tok/s, Ornith about 38, Flash-Next
25-35; Flash-Next 35K prefill about 88 s on both). The single miss on build 16 was a one-off that did not
recur in 6 further follow-up turns on that build; one miss in 9 versus none in 12 is not a difference.

## Decision

1. **Stop carrying patches.** Production llama.cpp is plain upstream, built from `ggml-org/llama.cpp` master into
   a new directory per build (catalog build 19 `llama.cpp-upstream-plain/build-rocm-plain` is the first).
2. **Move configs one at a time** per ADR-0018 section 2: `forge load` on the new build, compare against the
   config's own baseline (cache reuse, decode, `n_ctx`, journal), revert on any regression. Build 16 stays as the
   rollback until the last config has moved.
3. **Archive the fork.** `kintsugi` is obsolete. The two commits are preserved as patch files in
   `docs/history/kintsugi/`, the branch tip is tagged, and its README says it is obsolete. Archiving the GitHub
   repository itself is the operator's call (see Consequences).
4. **Experiments live on branches off plain upstream**, not in a standing fork. Clover is on
   `feature/clover-draft-self-r3` (upstream plus ggml-org/llama.cpp#31306 plus two small commits).

## Migration results (2026-11-01)

Per config: `forge load` on build 16 and run a smoke battery (`n_ctx`, a coherence prompt, a reasoning puzzle, a
tool call, decode speed, a 3-turn ~10K-token cache-reuse probe; tokenization for the DNA models), repoint the
config to build 19, reload, repeat, compare; revert on any regression (failed load, `n_ctx` change, a check that
passed before failing, decode below 85% of baseline, reuse regression, different tokens). All nine passed.

| Config | Decode, build 16 -> 19 | Follow-up tokens re-evaluated (reuse) |
|---|---|---|
| qwen36-35b-a3b-64k | 78.8 -> 80.3 tok/s | 16, 18 -> 16, 18 |
| qwen36-35b-a3b | 86.0 -> 85.5 | 16, 17 -> 16, 17 |
| qwen36a | 53.2 -> 54.7 | 16, 18 -> 16, 18 |
| ornith-35b | 44.6 -> 45.6 | 23, 25 -> 24, 26 |
| groot-19b-a3b-261023-gguf-q4-k-m | 65.8 -> 67.6 | 23, 25 -> 23, 25 |
| sage-a15 | 52.3 -> 52.8 | 18, 20 -> 18, 20 |
| qwen38-flash-next | 32.9 -> 31.7 | 16, 18 -> 16, 18 |
| carbon-x | 14.5 -> 14.5 (direct slot chat) | n/a: tokenization identical (4/12/10 tokens), same answers |
| carbon-8b (base model) | n/a | n/a: tokenization identical, `n_ctx` unchanged |

The harness first reverted `carbon-x` on a false alarm: it is a hidden config, a0 does not route hidden configs,
and the chat check got a 404 on build 16 as well. It was re-tested directly against the slot and moved. The five
Vulkan configs on build 12 were not touched. Smith's tracked-binary entry for the kintsugi tree was replaced by the
plain tree (previous value saved at `/opt/forge/backups/smith.binaries.tracked.pre-adr0019.json`).

## Consequences

* A rebase is now "pull upstream and rebuild". New model support (LFM3 and later) arrives without re-integration.
* **Validated per config on the ROCm side, not on Vulkan.** The `hybriddna` tokenizer is upstream (in
  `src/llama-vocab.cpp`), and Carbon/Sage passed. The Vulkan configs (build 12) have not been tested on plain
  upstream and no plain Vulkan build exists yet.
* If upstream regresses cross-turn reuse, re-run the probe, pin the build, and only then consider a patch.
* The smith runbooks no longer look for the `Kintsugi: saved generation checkpoint` log line, which does not exist
  on plain builds; they check `timings.prompt_n` instead.
* **Archiving the repository freezes the experiment branches** that live in it (Clover). Push those branches
  elsewhere, or finish the Clover evaluation, before archiving. Binaries already on ForgeHost are unaffected.

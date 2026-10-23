# Keep the llama.cpp fork thin: rebase onto upstream, drop what upstream absorbed, pick flags per build

Status: accepted (2026-10-23).

## Context

ForgeHost runs llama.cpp from `jsaigou/llama.cpp`, a fork of `ggml-org/llama.cpp`. Its `kintsugi` branch carried
two kinds of patch: (1) our own cross-turn KV-cache reuse for hybrid/recurrent models ("Kintsugi") and the
MTP/EOG checkpoint fix (ggml-org/llama.cpp#28049); (2) eleven commits assembling qwen4exp MTP speculative
decoding out of in-flight community PRs. On 2026-10-22 the branch was 1,086 commits behind upstream
(base 2026-09-01). Upstream had meanwhile merged qwen4exp MTP itself (#29761) and made a breaking CLI
change: `--mmap`/`--no-mmap`/`--mlock`/`--direct-io` were removed in favor of `--load-mode
{auto,none,mmap,mlock,mmap+mlock}` (#28334). The catalog launches llama-server with per-config
`extra_args`, ten of which carried `--no-mmap`, and the HF auto-registrar stamped `--no-mmap` on every new
model.

Measured on ForgeHost (Flash-Next + MTP, ROCm, 262144 ctx, vision attached): the rebased build kept
cross-turn cache reuse identical to the old build (a 184,130-token prompt and its follow-ups, same answers,
`prompt_n` 23-31 on follow-ups) while decoding ~2x faster (30-37 vs 16-18 t/s) and prefilling a long
prompt ~2.1x faster (222 vs 106 t/s). Evidence and per-config results: `progress.md` (2026-10-23).

## Decision

1. **Rebase by rebuilding, not by replaying.** The fork branch is rebuilt from current upstream master by
   cherry-picking only the patches upstream does not have. Patches that upstream merged are dropped, not
   ported. The pre-rebase `kintsugi` is preserved under a dated tag; the long-form history stays in git.
2. **A rebased build joins the catalog as an additive `builds` row** (migration 0091 for the 2026-10 builds)
   and configs are moved onto it one at a time, each tested through the real `forge load` path against its
   own baseline (cache reuse, decode, `n_ctx`, journal) and reverted on any regression. The old build row
   stays as the rollback.
3. **Launch flags are a property of the build, not of the config template.** Catalog `extra_args` are
   stored in the spelling the config's own build understands (`--no-mmap` -> `--load-mode none` where the
   binary accepts it). The HF auto-registrar asks the chosen binary (`<bin> --help`, cached by
   path+size+mtime) which spelling it takes instead of hard-coding one, and falls back to the modern flag
   when it cannot tell.
4. **The registrar's build choice is data-driven**: among live builds (non-empty `binary_path`, not
   `(retired)`) for the required backend, the one already serving the most configs, newest on a tie. A
   merely-newer row (a smith candidate, a test build) is never adopted by default.

## Consequences

* Tracking upstream becomes cheap again: the carried patch set is two commits, so each rebase is a handful
  of conflicts in `tools/server/server-context.cpp` rather than a re-integration of several PR branches.
* We depend on upstream's qwen4exp MTP staying compatible with the detached `mtp-*.gguf` heads we already
  ship; if it regresses we pin the build, not re-patch.
* A config on a legacy fork build (poolside rocm715, which rejects `--load-mode`) keeps working because its
  flags are chosen per build; nothing relies on a global rename.
* Operators editing `extra_args` by hand must use the spelling their build accepts. The flag picker lists
  `--load-mode` and marks `--no-mmap` deprecated.
* Remaining risk: a rebased build is validated on the configs and prompts we ran, not exhaustively.
  Vulkan configs on `standard-vulkan` (build 12) were not moved and have not been tested on the rebased
  Vulkan build.

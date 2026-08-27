# Model Configuration Concepts

The Forge has no fixed model roster to document here - the catalog (models, variants,
artifacts, configs) is entirely store-backed, and every deployment's roster is whatever you've
added via **Models → Add Model**, smith, or a manual catalog entry (see
[docs/adding-a-model.md](adding-a-model.md)). This page covers the flags and constraints that
apply across configs, not a list of specific models.

## Backend: Vulkan vs ROCm

Vulkan is the default backend and generally wins on token-generation throughput. It has a hard
ceiling, though: it cannot load models much above **~63 GB** (`radv: Not enough memory for
command submission`). ROCm with `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` reaches the full unified-memory
GTT pool (~120 GB on a Strix Halo-class host) - use `backend = "rocm"` for anything larger.
Backend is set per-config; the dashboard exposes a toggle. See
[docs/pitfalls.md](pitfalls.md) for the underlying memory-accounting detail.

When performance is a tie between the two backends, prefer ROCm: it fails an out-of-memory
condition cleanly inside the process, where Vulkan can silently OOM-kill an adjacent service via
the kernel's own OOM killer.

## `--parallel` - always set it explicitly

`--parallel N` in llama.cpp does **not** allocate N× the configured context - it **splits** the
single configured context value across N slots. `/props`'s
`default_generation_settings.n_ctx` (what the engine reads to verify a load) reports the
**per-slot** value: `configured_context / parallel`.

For any config meant to serve one long conversation at its full configured context, set
`--parallel 1`. A higher value only makes sense for a short-context, high-concurrency
fast-worker config where several small, independent requests are the actual intended use.

## `--ctx-checkpoints`

This flag (llama.cpp default is currently 32) lets the engine checkpoint and restore context
state instead of reprocessing a whole conversation from scratch on every turn. It's most
valuable for architectures that need cross-turn cache reuse (see the hybrid/recurrent note
below). Before enabling it on a given llama.cpp build, check that build's upstream issue tracker
for open hang bugs in the checkpoint-restore code path - this has historically been an area with
unresolved upstream bugs, so treat a positive value as build-specific, not a blanket-safe default.

## Hybrid/recurrent architectures reprocess the whole prompt every turn

Any model whose GGUF architecture mixes SSM/recurrent layers with attention (visible in GGUF
metadata as `*.ssm.*` fields) logs `forcing full prompt re-processing due to lack of cache data`
on every turn, and genuinely reprocesses the entire accumulated conversation from scratch - not
a bug, an upstream llama.cpp limitation for this architecture family as of this writing. Per-turn
latency grows with conversation length until it can exceed a calling agent's client-side timeout.
GPU temp/power/GTT usage stay normal throughout - the GPU is doing genuine, if wasteful, work,
not hung. `--swa-full` does **not** fix this for hybrid/recurrent models (it only disables
sliding-window-attention cache eviction, a different mechanism); `--ctx-checkpoints` is the
flag that would fix it, subject to the upstream-hang caveat above.

## Speculative decoding (MTP / draft models)

Multi-token prediction (MTP) and draft-model speculative decoding
(`--spec-type draft-mtp` / `draft-dflash`, `--spec-draft-model`, `--spec-draft-n-max`) can give a
meaningful throughput boost on a GGUF built with the matching draft head or a paired draft model.
Requires GGUF-level support from the specific quant/build you're running - not every quant of a
given model ships the draft tensors, and using one that doesn't will not error, it just won't
speed anything up.

## mmproj (vision) is vision-only

A config with an `mmproj` file gets image/video-frame input, not audio - video is sampled as
frames and any audio track is never read. A vision-capable config cannot be used for
speech-to-text.

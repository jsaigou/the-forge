ref: pitfalls:vllm-hipmalloc-ceiling
doc: pitfalls
slug: vllm-hipmalloc-ceiling
title: vLLM on Strix Halo APU - PyTorch `hipMalloc` Ceiling
category: memory
source: docs/pitfalls.md

## vLLM on Strix Halo APU - PyTorch `hipMalloc` Ceiling

PyTorch's standard allocator uses `hipMalloc`, which is bounded by `hipGetDeviceProperties().totalGlobalMem` (~66 GB on Strix Halo) even though the GTT pool is ~120 GB. `hipMallocManaged` can exceed this limit, but PyTorch does not use it - this is an upstream gap with no runtime workaround.

**Effect on vLLM:** vLLM's `MemorySnapshot` reads `total_memory = 66 GB` from HIP. At `--gpu-memory-utilization 0.98`, requested budget = 64.7 GB. Weights + MoE profiling overhead consume most of this, leaving a small residual for KV cache:

| Model | Weights | MoE overhead | KV available | KV needed (32k ctx) | Result |
|---|---|---|---|---|---|
| Carbon 8B | ~16 GB | none | ~48 GB | 0.5 GB | ✓ fits |
| Hy-MT2 30B-A3B | ~56 GB | ~7 GB | ~1.8 GB | 3.0 GB | ✗ blocked |

MoE profiling overhead scales with `--max-num-batched-tokens` and arises from expert routing buffer allocation during the vLLM profiling forward pass.

**Unblocked by:** PyTorch/ROCm managed-memory allocator for APU targets (no upstream ETA), or llama.cpp adding `hy_v3` architecture support (Hy-MT2 moves to `backend = "vulkan"`).

**Memory display note:** The dashboard's GTT status bar plots `inference_rss_mb` (not the raw `gtt_used_mb` hardware counter) against `gtt_total_mb`. As of the 2026-07-21 fix, `inference_rss_mb` is `gtt_used_mb + unified_mb`, where `gtt_used_mb` is the whole-GPU `rocm-smi` reading (correctly includes ComfyUI and any other classic-GTT-backed process, but does not see unified-memory/HMM allocations - see above) and `unified_mb` is `VmRSS` summed *only* for `llama-server` processes in slots whose mode uses `backend = "rocm"` (`engine._get_unified_memory_rss_mb()`) - the only consumers genuinely invisible to `gtt_used_mb`. Previously this was `max(gtt_used, vmrss_used)` with `vmrss_used` summing *all* `llama-server` processes regardless of backend - additive would have been correct, but `max()` meant that whenever a ROCm+unified slot outweighed `gtt_used_mb` (the common case), every other GTT-backed consumer running alongside it - ComfyUI included - was silently dropped from the total; see `docs/scheduler.md` "ComfyUI as a Reservable Resource" for the full writeup. The weight/KV breakdown (`weights_mb`, `kv_mb`) is still derived from forge env files and does not account for non-forge GPU consumers either way - KV estimate will still be inflated if ComfyUI is co-running (a cosmetic breakdown-attribution quirk, not a budget/OOM-prevention correctness issue).

ref: pitfalls:gtt-ceiling
doc: pitfalls
slug: gtt-ceiling
title: ROCm & GTT Memory (unified-memory APUs)
category: memory
source: docs/pitfalls.md

## ROCm & GTT Memory (unified-memory APUs)

A unified-memory APU host (e.g. AMD Strix Halo) has no discrete GPU or PCIe bus - CPU and GPU
share the same DRAM pool. Model weights live in **GTT** (GPU-addressable system RAM), not a
separate VRAM pool.

- **Unified Memory flag:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` must be in **every** per-slot
  sysconfig env file, not just whichever slot a large config usually lands on - the scheduler
  places any config on whichever slot is free, never pinned to one physical slot. Without it,
  ROCm is capped at ~63 GB. This is easy to lose silently: a rewritten sysconfig file or a fresh
  per-slot env file that doesn't preserve hand-set variables will drop it, and the ceiling won't
  be reached again until `n_ctx` or memory use looks wrong for a large ROCm-backed config.
- **All layers to GPU:** Always pass `--gpu-layers 99` (or equivalent). CPU-resident layers run
  at roughly half memory bandwidth. This is the single biggest performance lever.
- **GART vs GTT:** GART is a fixed BIOS aperture - keep it small (e.g. 512 MB). GTT is the
  dynamic GPU memory pool models allocate from; the platform's total GTT capacity is the
  effective model-size ceiling.
- **Contiguous allocation:** The kernel may fail to find a large contiguous GTT block. Always
  verify `n_ctx` via `/props` after startup - it silently downscales on allocation failure rather
  than erroring.
- **GPU utilization is not a proxy for efficiency:** GPU% measures shader occupancy. Token
  generation is memory-bandwidth-limited - 60-80% utilization is expected and normal on large
  models, not a sign of a problem.
- **rocWMMA FlashAttention:** on a standard (non-forked) llama.cpp build, avoid
  `-DGGML_HIP_ROCWMMA_FATTN=ON` on RDNA3.5-class hardware (gfx1151) - the rocWMMA kernel has
  historically been slower there than the standard HIP path, and upstream has since removed it
  entirely in favor of a unified kernel. Some third-party forks with hand-rolled WMMA
  flash-attention kernels are the exception and *require* rocWMMA to run at all on this hardware,
  because their kernel has no device code for the standard path - check a given fork's own build
  docs rather than assuming either default.
- **GTT counter blind spot with Unified Memory:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` backs
  allocations with HMM-mapped host RAM, not classic GTT BOs, so a raw GTT-usage reading (e.g.
  `rocm-smi --showmeminfo gtt`) does not see them - it can report near-baseline usage while real
  process RSS is tens of GB. Vulkan and non-unified-memory ROCm configs still report correctly
  via GTT; this blind spot is specific to the unified-memory path. The collector adds this path's
  RSS on top of the raw GTT reading rather than comparing the two, so any other GPU consumer
  sharing the host (e.g. an image-generation service) isn't silently dropped from the total
  whenever a unified-memory config outweighs it.

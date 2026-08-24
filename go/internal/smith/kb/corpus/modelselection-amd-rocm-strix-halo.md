ref: modelselection:amd-rocm-strix-halo
doc: modelselection
slug: amd-rocm-strix-halo
title: AMD (ROCm) — Strix Halo APU
category: gpu
source: modelselection.md

### AMD (ROCm) — Strix Halo APU

The Forge runs on a **Ryzen AI Max+ 395 (Strix Halo)** — an APU, not a discrete GPU system. This changes the memory model fundamentally.

**Architecture:**
- CPU and GPU share the same LPDDR5x-8000 DRAM on a 256-bit bus — **no PCIe bus between CPU and GPU**.
- GPU memory bandwidth: **~256 GB/s**. CPU memory bandwidth: ~128 GB/s (the memory controller is GPU-optimized; the CPU sees half).
- Models live in **GTT** (GPU Translation Table) — system RAM pages mapped into the GPU's address space. The GPU reads them at full 256 GB/s.
- `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` enables this GTT allocation path. Without it, ROCm is limited to the tiny on-die GPU cache (~2 GB physical VRAM showing in rocm-smi).
- `rocm-smi --showmeminfo vram` shows only dedicated on-die SRAM (~2 GB). The actual model pool is `--showmeminfo gtt` (~120 GB on this deployment).

**Strengths:**
- Up to 128 GB of unified GPU-accessible memory — no separate VRAM budget to manage.
- No PCIe bottleneck. Model data moves at DRAM speed.
- Linux-native via ROCm; GTT allocation is transparent once `GGML_CUDA_ENABLE_UNIFIED_MEMORY` is set.

**Pitfalls (from production experience on this host class):**

- **All layers must go to GPU.** Always set `--gpu-layers 99`. CPU-offloaded layers run at ~128 GB/s instead of 256 GB/s — a 2× bandwidth penalty per layer. This is the single largest performance lever.

- **GART must be minimized.** GART is a fixed BIOS aperture reserved for the GPU. Set it to 512 MB in BIOS; the rest of GPU-addressable memory comes from dynamic GTT allocation. Large GART wastes addressable space without benefit.

- **Contiguous GTT allocation failures.** The kernel may fail to find a large contiguous block for the KV cache and silently reduce `n_ctx`. Always verify via `/props` after startup. Kernel mitigations: `amdgpu.mcbp=0`, `amdgpu.vm_fragment_size=9`.

- **GTT memory leaks.** Qwen and Gemma models leak GTT memory across requests unless `--ctx-checkpoints 0` is set. Cumulative leaks cause OOM crashes. Not needed for pure-attention models.

- **Slow shutdown on large models.** Models > 80 GB take several minutes to unload. `TimeoutStopSec=60` (systemd default) causes SIGABRT before unload completes, leading to restart loops. Set `TimeoutStopSec=300`.

- **`--parallel` default causes OOM.** llama-server defaults to `--parallel 4`. Each extra slot multiplies KV cache allocation. Set `--parallel 1` unless headroom is confirmed.

- **MXFP4 ROCm tax.** MXFP4 compute kernels are not fully optimized on ROCm/gfx1151. Observed ~10 tok/s penalty vs equivalent K-quant. CUDA MXFP4 does not have this problem.

- **Flash attention required for long context.** Without `--flash-attn on`, attention computation memory scales quadratically with sequence length and OOMs at moderate context.

- **GPU utilization % is not an efficiency proxy.** rocm-smi GPU% measures shader occupancy. Token generation on large models is memory-bandwidth-limited — 60–80% is expected and normal. Do not interpret it as wasted compute capacity.

- **rocWMMA must not be used with ROCm 7.0.2+ on gfx1151.** The `DGGML_HIP_ROCWMMA_FATTN=ON` build flag is slower than the standard HIP path at longer context. Use the default ROCm path.

- **Vulkan RADV outperforms ROCm for token generation.** Vulkan RADV achieves ~85 t/s vs ~64 t/s (ROCm) for Qwen3 30B on Strix Halo. The Forge uses ROCm for `GGML_CUDA_ENABLE_UNIFIED_MEMORY` compatibility, but Vulkan is worth revisiting as an alternative backend.

- **SELinux labeling.** Scripts and binaries under `/usr/local/lib/forge/` need `chcon -t bin_t`. Do not use `restorecon` — it resets to `lib_t` which prevents execution.

- **Hang detection is harder.** GPU% and temperature metrics are unreliable hang indicators on ROCm. Use throughput (tok/s from `/metrics`) as ground truth.

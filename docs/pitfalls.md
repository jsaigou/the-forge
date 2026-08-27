# Critical Pitfalls

## ROCm & GTT Memory (unified-memory APUs)

A unified-memory APU host (e.g. AMD Strix Halo) has no discrete GPU or PCIe bus - CPU and GPU
share the same DRAM pool. Model weights live in **GTT** (GPU-addressable system RAM), not a
separate VRAM pool.

- **Unified Memory flag:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` must be in **every** per-slot
  sysconfig env file (`/etc/sysconfig/forge-a{1,2,3,4}-env`), not just whichever slot a large
  config usually lands on - the scheduler places any config on whichever slot is free, never
  pinned to one physical slot. Without it, ROCm is capped at ~63 GB. This is easy to lose
  silently: a rewritten sysconfig file or a fresh per-slot env file that doesn't preserve
  hand-set variables will drop it, and the ceiling won't be reached again until you notice
  `n_ctx` or memory use looking wrong for a large ROCm-backed config.
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
  flash-attention kernels are the exception and *require* rocWMMA to run at all on this
  hardware, because their kernel has no device code for the standard path - check a given
  fork's own build docs rather than assuming either default.
- **GTT counter blind spot with Unified Memory:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` backs
  allocations with HMM-mapped host RAM, not classic GTT BOs, so a raw GTT-usage reading (e.g.
  `rocm-smi --showmeminfo gtt`) does not see them - it can report near-baseline usage while real
  process RSS is tens of GB. Vulkan and non-unified-memory ROCm configs still report correctly
  via GTT; this blind spot is specific to the unified-memory path. The collector adds this
  path's RSS on top of the raw GTT reading rather than comparing the two, so any other GPU
  consumer sharing the host (e.g. an image-generation service) isn't silently dropped from the
  total whenever a unified-memory config outweighs it.

## Backend Selection: Vulkan vs ROCm

Vulkan (RADV) is the default backend and generally outperforms ROCm on token generation across
tested model sizes; ROCm tends to have a modest prefill advantage. Neither backend consistently
wins by a wide margin - expect them to be close, and pick per-config based on the memory ceiling
below rather than assuming one is always faster.

**Hard ceiling:** Vulkan cannot load models above roughly **63 GB**
(`radv: Not enough memory for command submission`). ROCm with
`GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` reaches the full unified-memory GTT pool (~120 GB on a
128 GB unified-memory host).

Backend is set per-config and can be changed live via the catalog without a restart.

## Hybrid/Recurrent Models Force Full Prompt Reprocessing Every Turn (llama.cpp, not deployment-specific)

Affects any config whose GGUF architecture has SSM/recurrent layers alongside attention -
check GGUF metadata for `*.ssm.*` fields. Every turn of a multi-turn session logs:

```
forcing full prompt re-processing due to lack of cache data (likely due to SWA or hybrid/recurrent memory, ...)
```

...and reprocesses the **entire accumulated conversation** from scratch, even when the new
request is a near-exact prefix match of the previous one. Per-turn latency grows with
conversation length until it can exceed a calling agent's client-side timeout. GPU temp/power/GTT
usage stay completely normal throughout - the GPU is doing genuine, if wasteful, work, not hung.

**`--swa-full` does not fix this for hybrid/recurrent models.** That flag only disables
sliding-window-attention cache eviction - it has no effect on recurrent (SSM/Mamba/Gated-DeltaNet)
state, which is the actual cause here. A plain (non-hybrid) architecture with genuine
sliding-window attention is a different, unrelated case where `--swa-full` is the correct fix.

**`--ctx-checkpoints` (alias `--swa-checkpoints`) is the flag the warning itself implies would
fix this** - it's the context-checkpoint/restore mechanism SWA and hybrid caches need to avoid
reprocessing.

**Do not enable `--ctx-checkpoints` on a hybrid/recurrent model without first checking upstream
llama.cpp's current issue tracker for that exact code path.** This has historically been an area
with open, unresolved hang bugs specific to hybrid/recurrent checkpoint restore. Enabling
checkpoints without checking risks trading "slow but working" for "actually hangs" - verify the
current upstream state before flipping this on a model you rely on.

## vLLM on Unified-Memory APUs - PyTorch `hipMalloc` Ceiling

PyTorch's standard allocator uses `hipMalloc`, which is bounded by
`hipGetDeviceProperties().totalGlobalMem` - on a Strix Halo-class host this reports roughly
66 GB even though the real unified-memory GTT pool is ~120 GB. `hipMallocManaged` can exceed
this limit, but PyTorch does not use it - this is an upstream gap with no runtime workaround.

**Effect on vLLM:** vLLM's memory-snapshot logic reads the ~66 GB HIP figure as total memory. At
a high `--gpu-memory-utilization`, weights plus MoE profiling overhead can consume nearly all of
the resulting budget, leaving little to no room for KV cache on a large MoE model - a smaller
dense model with no MoE overhead can fit comfortably in the same budget where a large MoE model
cannot.

**Unblocked by:** a PyTorch/ROCm managed-memory allocator for APU targets (no upstream ETA), or
by llama.cpp gaining support for the model's architecture (which would let it move to the Vulkan
or ROCm backend instead of vLLM, subject to the size ceilings above).

## Hang Detection

- **Ordinary hang:** the collector polls each slot's `/metrics` on a short interval. A hang is
  flagged when the requests-processing counter is nonzero and throughput has stayed near zero
  for a sustained window.
- **GPU device-lost is a distinct failure mode, not a stall.** When the GPU driver loses the
  compute device (a `DeviceLost`/ring-timeout error, followed by the device reporting "wedged"),
  the inference server process can *survive*: its own health endpoint stays green and the
  process keeps running, but every real request errors out instantly. Because requests fail
  fast rather than hang, a stall detector keyed on "active request + near-zero throughput" never
  fires - the slot looks healthy while being completely unresponsive. Detection needs to come
  from the kernel/driver logs or from a 5xx-rate signal on the router, not from the stall
  detector alone. Recovery is a straightforward unload→reload.

## Service Reliability

- **Shutdown timeout:** unloading a large model (80GB+) can take several minutes. A short
  service stop timeout causes an abort-and-restart loop instead of a clean unload - size it
  generously (e.g. 300s) for any large-model slot.
- **Parallelism:** never rely on an inference server's own default for `--parallel`; always set
  it explicitly to fit within your memory ceiling and the context-splitting behavior described
  in [docs/modes.md](modes.md).
- **A slot can remain occupied after its unit reports inactive.** A process can still be
  exiting - flushing GPU memory, finishing in-flight work - for a short window after the
  service manager considers the unit stopped. Anything that reads slot occupancy should treat a
  slot as still occupied until the unload path itself confirms completion, not the instant the
  unit's own state flips.

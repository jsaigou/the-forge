ref: pitfalls:backend-selection
doc: pitfalls
slug: backend-selection
title: Backend Selection: Vulkan vs ROCm
category: gpu
source: docs/pitfalls.md

## Backend Selection: Vulkan vs ROCm

Vulkan (RADV STRIX_HALO) is the default backend and outperforms ROCm on token generation across all tested model sizes. Benchmarks on Strix Halo (gfx1151, May 2026):

| Model | Size | ROCm tg128 | Vulkan tg128 | Vulkan pp advantage |
|---|---|---|---|---|
| Qwen2.5-Coder 7B Q4_K_M | 4.4 GB | 42.8 t/s | 46.2 t/s (+8%) | ROCm +11% pp |
| Qwen3.6 35B MoE Q4_K_XL | 20.8 GB | 49.2 t/s | 55.0 t/s (+12%) | tied |
| GPT-OSS 120B Q6_K | 58.9 GB | 47.7 t/s | 48.3 t/s (tied) | Vulkan +12% pp |

**Hard ceiling:** Vulkan cannot load models > ~63 GB (`radv: Not enough memory for command submission`). ROCm with `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` reaches ~120 GB GTT. (Stale as of 2026-08-25: the 91 GB `nemotron` mode this used to cite as the reason a ROCm backend exists at all was retired by the operator 2026-08-21 - "rocm lineage obsolete, nemotron retired," per the catalog `builds` table's own recorded reason. `qwen38-27b-rocm` is the current live ROCm-backend config exercising this ceiling.)

**Backend is per-mode** in config.toml (`backend = "vulkan"` or `"rocm"`). The dashboard exposes a toggle on each mode button. The engine writes `FORGE_BACKEND` to the sysconfig env file; the launcher script selects the binary.

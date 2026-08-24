ref: diagnosis-trial:rocm-flag-audit
doc: diagnosis-trial
slug: rocm-flag-audit
title: 7. Post-upgrade flag audit: what's now obsolete vs. what's still load-bearing
category: build
source: docs/diagnosis-trial.md

## 7. Post-upgrade flag audit: what's now obsolete vs. what's still load-bearing

Prompted by: "now that Strix Halo has first-class support, double-check load/compile-flag
workarounds from 7.13 that might now degrade performance." Checked every gfx1151-specific
compile flag and runtime env var against current upstream issue trackers (not assumption):

| Flag | Verdict | Evidence |
|---|---|---|
| `GGML_HIP_ROCWMMA_FATTN=OFF` | **Keep** | [llama.cpp#24437](https://github.com/ggml-org/llama.cpp/issues/24437) (open, updated 5 days prior) confirms rocWMMA still regresses prefill up to −41% on gfx1151. Maintainer's actual fix path is deleting the rocWMMA kernel entirely (RDNA support was added to the *standard* kernel instead, merged in [#22051](https://github.com/ggml-org/llama.cpp/pull/22051), already in our July 2026 build) — our `OFF` setting already gets the real fix via the default path. |
| `GGML_HIP_NO_VMM=ON` | **Keep, likely required** | Matches still-open [ROCm/ROCm#6501](https://github.com/ROCm/ROCm/issues/6501) (updated 3 days prior): llama.cpp mmap allocation fails above 64 GB on gfx1151. `nemotron` (91 GB) and `nemotron-puzzle` (75 GB) both exceed that — this flag is almost certainly why either loads at all. |
| `GPU_MAX_HW_QUEUES=4` | **Keep** | Matches still-open [ROCm/ROCm#6437](https://github.com/ROCm/ROCm/issues/6437) (updated 9 days prior): long-prefill AQL queue hangs on gfx1151, no timeout/reset. Directly relevant to this deployment's large-context configs. |
| `HSA_OVERRIDE_GFX_VERSION=11.5.1` | **Harmless, no action** | Literally gfx1151's real ISA version — redundant either way. |
| `HSA_ENABLE_SDMA=0` | **Obsolete — safe to remove** | Almost certainly worked around SDMA engine-count underflow on APUs with <2 SDMA engines, fixed upstream in rocm-systems, merged 2026-06-12 — before our 7.15 build. |

**Live-tested the one real finding** (§8) rather than trusting the tracker alone.

---

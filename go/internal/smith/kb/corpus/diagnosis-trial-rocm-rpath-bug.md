ref: diagnosis-trial:rocm-rpath-bug
doc: diagnosis-trial
slug: rocm-rpath-bug
title: 4. The real bug: CMake's automatic RPATH doesn't cover transitive dependencies
category: build
source: docs/diagnosis-trial.md

## 4. The real bug: CMake's automatic RPATH doesn't cover transitive dependencies

While auditing `nemotron-puzzle`'s separate build tree (`llama.cpp-puzzle`, the `puzzle-port`
fork), the same library-resolution check was re-run **in a truly clean environment**
(`env -i`, no `LD_LIBRARY_PATH`) as a matter of habit — and it failed, resolving `libhipblas`/
`librocblas`/`libhipblaslt` to `/opt/rocm-therock-7.13/lib` and `libamdhip64`/`libhsa-runtime64`
to **ComfyUI's unrelated Python ROCm SDK** (`/opt/rocm-nightly/.../_rocm_sdk_core/`), via the
system `ldconfig` cache.

That prompted re-checking the **already "verified working" mainline `nemotron` cutover** the
same way — and it had the *identical* problem:

```
env -i HOME=$HOME PATH=/usr/bin:/bin ldd <built llama-server path> \
  | grep -iE 'hipblas|rocblas|amdhip|hsa-runtime'

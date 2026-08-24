ref: diagnosis-trial:rocm-live-validation
doc: diagnosis-trial
slug: rocm-live-validation
title: 8. Live validation: `HSA_ENABLE_SDMA` removal
category: build
source: docs/diagnosis-trial.md

## 8. Live validation: `HSA_ENABLE_SDMA` removal

With `HSA_ENABLE_SDMA` unset (default = enabled), loaded the actual production
`nemotron-puzzle` model — `Puzzle-75B-A9B-Q4_K_M` (51 GB, 2 shards) — via the fixed
puzzle-port binary:

- Load: ~49 s, no crash, no hang, health check OK.
- Real completion request (200 tokens, 22-token prompt): succeeded — 41.4 t/s prompt
  processing, 15.0 t/s decode.
- `journalctl -k --since '2 minutes ago'` during the whole session: zero `amdgpu`/`kfd`/`sdma`/
  hang/fault messages.
- Clean shutdown, no leftover process.

**Not yet applied to production sysconfig** — `HSA_ENABLE_SDMA=0` is still set in
the per-slot sysconfig env file and in the ComfyUI service's env file (the latter
deliberately left alone given an open, same-day PyTorch/ROCm UMA deadlock report
unrelated to llama.cpp). Decision on removing it from the sysconfig files pending.

---

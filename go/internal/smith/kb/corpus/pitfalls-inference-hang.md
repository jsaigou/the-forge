ref: pitfalls:inference-hang
doc: pitfalls
slug: inference-hang
title: Performance Monitoring
category: gpu
source: docs/pitfalls.md

## Performance Monitoring
- **Hang Detection:** `monitor.py` polls `/metrics` every 4s. 
- **Trigger:** Flagged if `requests > 0` AND `TPS < 0.1` for 90 seconds. 
- **Note:** Older GPU%/Temp heuristics are unreliable for ROCm dual-model contexts.
- **GPU device-lost is NOT a stall — the hang detector misses it entirely (2026-08-16 incident).** When the amdgpu driver loses the compute device (`vk::Queue::submit: ErrorDeviceLost` after a `ring comp_X.Y timeout` → `device wedged`), llama-server *survives*: `/health` stays green, the process keeps running, but **every request errors out instantly**. Because requests fail fast (not hang), `requests_processing` returns to 0 and the stall detector (active-request + ~0 TPS, sustained) never fires. The engine's health check (also `/health`) stays green too. The 2026-08-16 qwen38-27b incident left a slot "healthy" while completely unresponsive for 26 minutes. **Detection must come from the journals** (the kernel ring is `journalctl -k`, llama-server's error is `ErrorDeviceLost` in the `forge-a*` unit journal) or the **router's 5xx count** (a wedged slot 5xxes every request — smith's `SLOT_ERROR_STORM` alert + `gpu_device_lost` check). Recovery is a trivial unload→reload (smith auto-recover). A strong *pre*-hang signal is the engine's `waitGTTDrain` 20s-timeout warning ("GTT still … after 20s") — it fired before both hangs on 2026-08-16 and is now surfaced as `GTT_DRAIN_TIMEOUT`.

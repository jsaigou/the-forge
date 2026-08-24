ref: pitfalls:service-reliability
doc: pitfalls
slug: service-reliability
title: Service Reliability
category: services
source: docs/pitfalls.md

## Service Reliability
- **Shutdown Timeout:** `TimeoutStopSec` must be `300`. Large models (80GB+) take several minutes to unload. Default 60s leads to SIGABRT and restart loops.
- **Parallelism:** Never assume `llama-server` defaults; always define `--parallel` to fit within GTT limits.
- **`unload_slot()`'s own wait loop is capped at 60s, not `TimeoutStopSec`'s 300s:** after `systemctl stop`, it polls for `dead`/`failed`/`inactive` substate for at most 60s, then proceeds regardless — force-killing any lingering PID (`_find_lingering_gpu_pids()`) and waiting for GTT drain. In practice a real unload has left orphaned GPU PIDs after `systemctl stop` even for small models (confirmed live, 2026-07-21) — `systemctl`'s own view of the unit can go `inactive` while the actual `llama-server` process is still exiting. Any code reading slot occupancy (`engine._reconcile_slot_state()`, the scheduler, the dashboard) must treat a slot as still occupied until `unload_slot()` itself clears it, not the instant the unit stops being `active` — see `docs/scheduler.md`'s status/state-model fix.

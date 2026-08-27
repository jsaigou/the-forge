ref: pitfalls:orphaned-slot-unit
doc: pitfalls
slug: orphaned-slot-unit
title: A slot can stay occupied after systemd reports the unit inactive
category: slots
source: docs/pitfalls.md

- **`unload_slot()`'s own wait loop is capped at 60s, not `TimeoutStopSec`'s 300s:** after `systemctl stop`, it polls for `dead`/`failed`/`inactive` substate for at most 60s, then proceeds regardless - force-killing any lingering PID (`_find_lingering_gpu_pids()`) and waiting for GTT drain. In practice a real unload has left orphaned GPU PIDs after `systemctl stop` even for small models (confirmed live, 2026-07-21) - `systemctl`'s own view of the unit can go `inactive` while the actual `llama-server` process is still exiting. Any code reading slot occupancy (`engine._reconcile_slot_state()`, the scheduler, the dashboard) must treat a slot as still occupied until `unload_slot()` itself clears it, not the instant the unit stops being `active` - see `docs/scheduler.md`'s status/state-model fix.

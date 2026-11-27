# Host RAM is partitioned ahead of time; a live availability floor backs it up

Status: **accepted and deployed (2026-11-17, v0.6.89).** Decided with the operator after the Flash-Next review
(`progress.md` top entry, `docs/radiance-flashnext-handoff-2026-11-16.md`). Code: `go/internal/memledger`, `engine/memory.go`, `collector/run.go`.

## Context

On Strix Halo the GPU's GTT pages are ordinary system RAM. With llama.cpp the models were small enough that the GTT ceiling (120 GiB) bound first.
With a 90 GiB Radiance container the **host** binds first, and the old budget (`GTTTotal - gtt_used - slot RSS`) stopped describing reality.
Measured 2026-11-17 with the 84.37 GiB Flash-Next container on a1: Forge reported 96 of 120 GiB used (24 free); the host had 111 of 123.5 GiB used
(`MemAvailable` 12.3 GiB), swap was full, and about 11 GiB of tmpfs scratch was pinning RAM and zram. Direct (non-Forge) Radiance runs were invisible
to the slot accounting altogether. Separately, commit `22d0b34` had turned the host-headroom cap into `MemTotal - 8 GiB` compared against *remaining
free*, a constant that never binds: with a1 loaded the gate would still admit a 20 GiB load against about 4 GiB of real headroom (the 2026-08-22 incident
path). The commit was reacting to a real symptom, a refused load, but the cause was `safe_memory_bytes` holding whole-host usage (114.2 GiB) where an
incremental need (about 93 GiB) belonged.

The original goal of the reserve was a safe margin for the always-on engines and the OS. A single live check cannot give that: it only sees what is
resident now, so it reserves nothing for a stopped or restarting engine.

## Decision

Two independent guards; a new load may claim the smaller of them (and of the legacy GTT bound):

1. **Declared partition.** `model budget = MemTotal - OS floor - declared peak of every enabled always-on engine - growth margin`. Declared peaks come from
   the engines' catalog `memory_bytes` (the specialist occupants); a stopped or disabled engine is not declared. Models are charged what they actually hold:
   `GTT used - measured GPU use of the dedicated-slot processes + slot RSS (every backend)`. It is deterministic: a restart of an engine, tmpfs scratch or
   page cache cannot change what the models are allowed.
2. **Dynamic floor.** `MemAvailable - host floor`. The safety net for anything the partition does not know about.

Settings: `infra.memory` = `{os_floor_bytes (2 GiB), growth_margin_bytes (4 GiB), host_floor_bytes (8 GiB), swap_warn_bytes (1 GiB)}`, store-backed (no TOML),
read with `forge config get -db ... infra.memory`.

One pure function, `memledger.Compute`, serves the engine's live probe (`MemoryBudget`, the fit gate), the collector snapshot (so `scheduler/status`,
Smith, MCP, the TUI and the dashboard read the same numbers). `memory_budget.partition` is additive on the wire; `free_bytes` is now the ledger's free.
The old `ReservedBytes` subtraction is gone (the declared partition already holds the memory back, running or not; subtracting both double-counts).
Swap on ForgeHost is zram, i.e. compressed RAM; Smith gets `host_memory` and `swap_pressure` checks. The dashboard meter is drawn over host RAM with the
reserved regions visible.

## Evictions (added 2026-11-27)

What eviction gives back is computed by the same ledger, with the slot's holdings removed from the measurements: its GPU memory from the GTT
counter, its resident memory from the slot figure, and its anonymous and shared pages added to `MemAvailable` (file-backed pages are already
available). Until then an eviction was credited with GPU memory only, and Radiance 2.x slots, started with `--config` and no `--port`, were not
attributed to their slot at all, so their resident memory (about 30 GiB for Flash-Next) was missing from the budget. After evicting, the
scheduler measures again and loads only if the model fits.

## Consequences

- **`safe_memory_bytes` must be incremental.** A figure that includes the OS floor and the engines (the 114.2 GiB on benchmark 128) no longer fits an idle
  box. Re-measure it as the slot's own footprint (GTT plus RSS, about 93 GiB for the size container) when deploying this.
- The measured Flash-Next 3x131K footprint leaves about 11 GiB of model budget with the five always-on engines declared (about 13 GiB).
- Declared peaks must be honest; an engine that outgrows its declaration is caught by the dynamic floor, not by the partition.
- Swap: keep a small zram (2-4 GiB) as a cushion for cold engine pages; do not add disk swap; `swap_pressure` warns at 1 GiB in use.

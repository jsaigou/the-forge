ref: modes:slot-rename-a1-a2
doc: modes
slug: slot-rename-a1-a2
title: Slot rename: `primary`→`a1`, `secondary`→`a2` (2026-07-29)
category: slots
source: docs/modes.md

## Slot rename: `primary`→`a1`, `secondary`→`a2` (2026-07-29)

Forge's four inference slots have always had two names: an internal key
(`primary`, `secondary`, `a3`, `a4`) and a conceptual/display name (`a1`,
`a2`, `a3`, `a4` / dashboard labels `A1`-`A4`). `a3`/`a4` never needed a
second name because their internal key already matched their conceptual
position; `primary`/`secondary` were the historical odd ones out,
inherited from V4. Renamed for full consistency — **labels (`A1`-`A4`) and
ports (8080/8081/8087/8088) are unchanged**, only the internal identifier.

**Hard cutover, no back-compat alias** — `slot:"primary"` now 400s on
`/api/v1/load`/`/unload` and the MCP surface, same as this repo's other
precedent renames (`0003_headroom_rename.sql`, `0011_config_status_rename
.sql`). This is a single-operator system, not a multi-tenant API.

**What moved:** the catalog `slots` table (`name`/`unit` columns) plus
`slot_state`, `sched_queue.target_slot`, `reservations.bay`, and
`usage_events.slot` (migration `0014_slots_aN_rename.sql`, live in the DB
on next `forge` startup — no manual DB surgery); four Go literals
(`cmd/forge/merged_config.go`'s `PortRole` default, `internal/httpapi/
validate.go` + `internal/mcp/helpers.go`'s duplicate `validSlots` maps,
`internal/engine/engine.go`'s `Stub.Slots()` fallback); one frontend
literal (`web/src/components/ReservationModal.tsx`'s `SLOT_OPTIONS`); new
systemd unit files `forge-a1.service`/`forge-a2.service` + launcher
scripts `start-a1.sh`/`start-a2.sh` (manually installed on the deployment host, same
one-time-root-install pattern as the Headroom template unit) replacing
`forge-primary.service`/`forge-secondary.service`.

**The one real risk, verified before touching anything:** `SwitchMode()`
(`internal/engine/lifecycle.go`, used by `/api/v1/switch/{mode}`) uses
`svc.PortRole` as-is to pick the target slot — unlike `Load()`
(`/api/v1/load`), which explicitly overrides `svc.PortRole` with the
caller's chosen slot. So every `merged_config.go`-sourced mode always
targets whatever `PortRole` default is hardcoded there; that literal had
to move from `"primary"` to `"a1"` in lockstep with the DB rename, or
every `switch` call would have silently stopped placing anything.

Polkit needed no change (`forge-*.service` glob already covers the new
names). MCP tool JSON schemas needed no change (no `enum` pins the slot
value — `target_slot`/`bay` are free strings, validated at runtime by the
renamed `validSlots` maps).

---

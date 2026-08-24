# Single-instance scope is per-Config, not per-Model

ADR 0002 established Config as the unit of loading: the dashboard switcher, scheduler, registry, and usage tracking all key on Config. A natural follow-on was left implicit: the single-instance guard (only one copy of a given loadable unit resident at a time) must also be per-Config.

The engine-level guard (`engine/lifecycle.go:154`) has always been per-config-name — it rejects loading a mode name that's already active on another slot. This is consistent with 0002: two different configs of the same model (e.g. `qwen3-coder-256k` and `qwen3-coder-1m`) are distinct loadable units, and the engine correctly permits both to coexist on two slots. a0 routes by config name, so two configs of one model are not ambiguous to the router.

The handler-level guard (`httpapi/scheduler_handlers.go:findModelLoadedElsewhere`) was inconsistent: it resolved the requested config name to its registry **model_id** via `modeToModelID()` and rejected any same-model-id load, blocking two configs of the same model from coexisting. The comment at `lifecycle.go:148` even inverted the relationship, calling the engine's per-config guard a "safety net" behind a "handler-level guard [that] resolves by registry model id" — under 0002, the engine guard is the primary; the handler's model-id resolution is the artifact.

This ADR makes the scope explicit: **single-instance is enforced per-Config (per mode/config name), not per-Model.** The handler guard is corrected to match the engine: direct mode-name comparison, no registry model-id resolution. `modeToModelID()` is retired.

## Considered Options

- **Per-Config scope (chosen):** Two configs of the same model can coexist on different slots. Consistent with 0002's "Config is the unit of loading." The engine guard and handler guard converge on identical logic (handler = early-reject fast path; engine = defense-in-depth). a0 routing is unambiguous by config name. The config-scoped console gallery shows each config's loaded state independently — a card for `qwen3-coder-1m` shows "idle" even if `qwen3-coder-256k` is loaded, and its Load button works.
- **Per-Model scope (rejected):** At most one config of a given model is ever live. The handler resolves config → model_id and blocks same-model loads. This contradicts 0002's framing (Config is the unit, not Model) and forecloses the multi-config scenario that a0's name-based routing already supports. It also produces a broken gallery affordance: a config card shows "idle" with a live Load button that 409s on click because a sibling config of the same model is resident.

## Amendment (2026-08-22): same-weights coexistence is conditional on proven headroom

Per-Config scope stands — but the 2026-08-22 GTT-exhaustion crash (two sibling
configs of one Gemma GGUF loaded onto a2/a3 alongside a busy qwen38-27b,
wedging the host into a power cycle) showed the unconditional half of this ADR
is unsafe on a unified-memory box: "permitted to coexist" must not mean
"permitted regardless of memory".

Refined policy, decided by the operator same day:

1. **Same config name already resident** → refused immediately (unchanged).
2. **Different config, identical weights** (identity = resolved on-disk weight
   + mmproj paths via `engine.Manager.WeightIdentity`, NOT catalog artifact row
   IDs — the gemma pair carried duplicate artifact rows) → permitted only when
   the fit gate proves real headroom for a second instance. Without room,
   `Engine.Load` refuses fast with an explicit reason. Coalescing traffic onto
   the resident instance was rejected: sibling configs differ in generation
   flags (`--reasoning off` etc.), so routing a `-nothink` request to a
   thinking-enabled server would silently change behavior.

The enforcement point is `lifecycle.go`'s Load guard (the choke point every
caller funnels through); terminal-only shape (nothing fits even after every
eviction) so the scheduler's own eviction flow still functions. See
`memory_incident_test.go` for the regression pair.

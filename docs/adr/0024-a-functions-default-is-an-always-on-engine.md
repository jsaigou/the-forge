# A function's default must be an engine that is always on

Status: **accepted (2026-11-04).** Decided in a grilling session 2026-11-04 (the operator's call); implemented in commits 5343ac8 (the rule) and aecf353
(moved into `specialist.Occupants`), deployed as forge v0.6.79. Amends ADR-0022 decision 6, which said each function has a default but not which
engines may be one.

## Context

ADR-0022 decision 6: a0 routes by function (`embeddings`, `speech`, `transcription`, `alignment`, `decisions`). A request whose `model` names an engine
goes to it; otherwise the function's default answers (`specialist.Manager.Resolve`). The default is therefore the engine behind every request that names
no model, which is most of them: existing URLs and client shapes depend on it.

Nothing stopped an On demand or Disabled engine from being the default. An On demand default makes the unnamed path pay a cold start (3.2 s for a small
engine with a free slot, measured in ADR-0022) on whatever request happens to arrive first; a Disabled default has nothing running to answer. The
operator's reasoning: *"default should be an always on model. On demand or disabled makes zero logical sense as a default unless the whole system is in
maintenance mode."*

## Decision

1. **Only an enabled engine with Behavior Always on can be a function's default.** Setting any other engine as a default is refused: 422 on the
   `occupant` field, field code `engine_must_be_always_on`.
2. **A default cannot be taken out of that state.** While an engine is the default for a function, changing it to On demand or Disabled (the Behavior
   and policy toggles, or a full edit) is refused with 409 `default_engine_in_use`, whose `params.functions` names the functions it is the default for:
   set another default, or clear it, first. Nothing is cleared or re-pointed silently.
3. **Deleting an engine still clears its defaults** in the same transaction, as before. A function with no default answers `ErrNoMatch` to a request that
   names no engine, and the caller keeps its pre-pool behavior (ADR-0022).
4. **Existing data is not migrated.** An older default that is not Always on is not newly blocked from being changed or cleared. A check of the live pool
   on 2026-11-04 found all three defaults (embeddings, transcription, alignment) already compliant.
5. **Where it lives:** the rule is enforced by `specialist.Occupants` (`go/internal/specialist/occupants.go`), the one module through which occupants and
   defaults are written; the HTTP handlers only translate. The store carries no constraint for it.
6. **Its error codes are translated like any other.** Both codes have `en` and `ja` entries in `web/src/locales/*/errors.json`.
   `TestErrorCodesHaveTranslations` now also scans the modules that return coded errors (`moduleErrorFiles` in `errors_codes_test.go`), because those codes
   no longer pass through a `writeErrorCode` call the scan could see.

Maintenance mode is not modeled. To take a default engine out of service, set another default or clear it, change the engine, and set it back.

## Alternatives rejected

- **Allow On demand defaults.** Keeps the cold start on the unnamed path, which is the path latency-sensitive callers use.
- **Clear or re-point the default automatically when its engine is switched away.** A silent change to what unnamed requests hit.
- **A database constraint.** The rule spans two tables (defaults, occupants) and two columns of the second; the store stays plain and the module owns the
  rule, as for the other occupant rules.
- **Reusing `has_dependents` for the refusal.** Its text ("This has dependent {{resource}} — delete those first.") says the wrong thing here; the rule
  has its own code.

## Consequences

- Moving the default engine to On demand or Disabled is now a two-step action in the UI.
- Related rule changes made in the same work, recorded here so they are not re-litigated: a Disabled engine no longer counts as the owner of its catalog
  config (matching what the scheduler and a0 already did, `Manager.occupantForConfig`), and re-enabling it is checked against the config's current
  owner (ADR-0022 decision 3).
- Not decided: what a request does when a pre-existing default is Disabled. The API can no longer create that state, but data that already has it is not
  migrated.

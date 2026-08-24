# Dashboard builder sprint deferred; widget registry deleted (YAGNI)

Status: superseded by [ADR-0011](./0011-dashboard-builder-v1-scope-rendering-persistence.md) (2026-08-14 — the deferred sprint is now being planned and shipped; the registry is re-created with a different shape than the deleted v1).

## Context

On 2026-08-12, the "Phase 5: Dashboard restructure" sprint (`927f9c2`) extracted
the Dashboard's visualizations into 8 self-contained widget components under
`components/widgets/`. Alongside the extraction, `lib/widgetRegistry.ts` was
created as an 82-line catalog of widget metadata (ID, title, description, page) —
"substrate for a later dashboard-builder sprint" that would let operators
clone/hide/reorder widgets.

The file header explicitly stated: *"This sprint does NOT build a user-facing
editor... that's an explicitly separate, later sprint."* The code comment cited
`docs/v5-plan.md`'s "dashboard builder" note — but **that note does not exist** in
v5-plan.md. The decision was recorded only in `progress.md` (lines 344, 689).

As of 2026-08-13, the registry had **zero consumers** — no file in `web/src/`
imported it. The 8 widget components are imported directly by `Dashboard.tsx`,
bypassing the registry entirely. No sprint, issue, or roadmap entry schedules the
dashboard-builder work.

## Decision

Delete `widgetRegistry.ts` (YAGNI). The widget IDs are self-evident from the
component filenames (`ActivityHeatmapWidget`, `RightNowWidget`, etc.); re-creating
the registry takes ~10 minutes when the dashboard-builder sprint actually starts.
No code depends on it; no consumer breaks.

This ADR replaces the dangling `docs/v5-plan.md` citation and preserves the
deferral decision extracted from `progress.md`.

## Consequences

- 82 lines of dead code removed.
- The dashboard-builder sprint, if it ever ships, will re-create the registry
  from scratch (or design a different substrate — the current one may not match
  the eventual requirements anyway).
- `Dashboard.tsx`'s direct widget imports are unchanged — they never used the
  registry.

## Remediation

Implemented in Phase 4 of the QA remediation sprint (2026-08-13). See
`QA-REPORT.md` finding #9 and `docs/v5-qa-remediation-2026-08-13.md` Phase 4,
decision D2.

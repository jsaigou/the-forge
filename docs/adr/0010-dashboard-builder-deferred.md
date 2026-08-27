# Dashboard builder sprint deferred; widget registry deleted (YAGNI)

Status: superseded by [ADR-0011](./0011-dashboard-builder-v1-scope-rendering-persistence.md) — the deferred work was later planned and shipped; the registry was re-created with a different shape than the deleted v1.

## Context

An earlier Dashboard restructure extracted the Dashboard's visualizations into self-contained widget components under `components/widgets/`. Alongside the extraction, `lib/widgetRegistry.ts` was created as a small catalog of widget metadata (ID, title, description, page) intended as substrate for a later dashboard-builder feature that would let operators clone/hide/reorder widgets.

That later feature was never scheduled, and the registry had zero consumers — no file in `web/src/` imported it. The widget components were imported directly by `Dashboard.tsx`, bypassing the registry entirely.

## Decision

Delete `widgetRegistry.ts` (YAGNI). The widget IDs are self-evident from the component filenames (`ActivityHeatmapWidget`, `RightNowWidget`, etc.); re-creating the registry takes only a few minutes when the dashboard-builder feature actually starts.

## Consequences

- Dead code removed.
- The dashboard-builder feature, when it ships, re-creates the registry from scratch (or designs a different substrate — the deleted one may not match the eventual requirements anyway).
- `Dashboard.tsx`'s direct widget imports are unchanged — they never used the registry.

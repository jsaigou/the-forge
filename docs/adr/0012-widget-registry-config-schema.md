# Widget registry: config-schema-driven per-instance config

Status: accepted (companion to [ADR-0011](./0011-dashboard-builder-v1-scope-rendering-persistence.md)).

ADR-0011 decided custom pages are widget-stacks rendered through a registry,
but left the registry's shape open. The 7 existing widgets have inconsistent
prop surfaces: 4 take no props, 2 take `{ window_, rangeLabel }`, 1 takes
`{ window_ }`, and `RightNowWidget` hardcodes `window_="24h"` internally
(`RightNowWidget.tsx:41`) rather than accepting it as a prop.

## Decision

The widget registry declares a **configSchema** per widget type — prop name,
type, default, and (for selects) options. The page editor auto-renders a
per-instance config form from the schema. No hand-built config UI per widget.

- **`window_`** is a `select` prop on the 3 window-accepting widgets
  (`ResourceTrend`, `ElectricityBreakdown`, `PerModelSpendTable`). Options are
  scoped per-widget: Cost widgets offer `1mo/6mo/1y/all`; Resource widgets
  offer `24h/72h/1w/1m`. Default `24h` (or the widget's current default).
- **`rangeLabel`** is **not a config prop** — it's a derived display string.
  Widgets derive it internally from `window_` via a shared window→label map
  (the existing `COST_RANGES`/`RESOURCE_RANGES` shapes). This eliminates the
  `{ window_, rangeLabel }` vs `{ window_ }` split: all window-accepting
  widgets take just `{ window_ }`.
- The 4 no-prop widgets declare an empty configSchema. They're placed as-is.

## Prerequisite: widget normalization

Before the feature ships, normalize the widgets so every widget's configurable
surface matches its configSchema:

1. **`RightNowWidget`** — refactor to accept `window_` as a prop (default
   `"24h"`) instead of hardcoding it at `:41`. Becomes a 1-prop widget.
2. **`ResourceTrendWidget` + `ElectricityBreakdownWidget`** — drop
   `rangeLabel` from the prop signature; derive it internally from `window_`.
   Becomes 1-prop widgets (`{ window_ }`).
3. **`PerModelSpendTableWidget`** — already takes `{ window_ }`; no change.

After normalization: 4 zero-prop widgets + 3 one-prop (`window_`) widgets.
Schema declares `window_` as a `select` on the latter 3.

This is finishing the Phase 5 extraction that ADR-0010 interrupted — not a
new direction.

## Page management

- Custom pages are **appended after** the 3 default tabs (Overview · Cost ·
  Resources), which are fixed-order and code-owned (per ADR-0011).
- Custom pages are **reorderable among themselves** via drag-to-reorder.
- The stored `pages` array **IS the order** — no separate ordering key.
- **No hard cap** on custom page count for v1 (revisit if abused; a soft cap
  like 20 is trivial to add later as a validation rule).
- CRUD: create (name + empty widget list), rename, delete, reorder. Delete is
  irreversible (no undo in v1) — confirm dialog.

## Why

Uniform per-instance config UX: the editor is declarative (renders from the
schema), not imperative (hand-built forms per widget). A user can place a
`ResourceTrend` on a custom page and override its window from `24h` to `1w`
without editing JSON. Two `ResourceTrend` instances with different windows on
one page is supported.

## Considered options

- **Thin registry** (slug + name + component + defaultProps, no config editor
  in v1). Rejected — defers per-instance config to v2 and leaves widgets with
  baked-in hardcoded values (e.g. `RightNowWidget`'s `24h`), which is exactly
  the kind of non-uniform surface the registry exists to fix.
- **Thin registry + ad-hoc config UI** (props pass through opaquely, per-widget
  hand-built config forms). Rejected — not uniform; the hand-built forms
  wouldn't stay in sync with widget prop surfaces.

## Consequences

- **Widget normalization is a prerequisite sprint** (the 3 refactors above)
  before the registry + editor ship.
- **configSchema is extensible.** v1 supports `select` (for `window_`). Future
  types (`number`, `boolean`, `text`) and future widgets declaring their own
  schemas are additive — no registry redesign needed.
- **`rangeLabel` is no longer a prop** on any widget — it's derived. The
  shared window→label map lives in `lib/` (alongside the existing
  `COST_RANGES`/`RESOURCE_RANGES` constants, or a unified `rangeLabels.ts`).
- **Mobile** (ADR-0011 Q7) is handled by the stack composer — no spatial
  coordinates to reflow. The config form is a standard modal/drawer, mobile-
  friendly by default.

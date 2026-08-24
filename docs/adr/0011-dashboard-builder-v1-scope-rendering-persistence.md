# Dashboard builder v1: custom pages as data alongside bespoke defaults

Status: accepted (supersedes [ADR-0010](./0010-dashboard-builder-deferred.md)).

ADR-0010 deferred the dashboard-builder sprint and deleted `widgetRegistry.ts`
as YAGNI. The operator feedback round (deployed `v5.0.7-stripes`, commit
`6b0aacd`) made this the one deferred item now being planned. This ADR records
the v1 scope, rendering seam, and persistence model decided in the planning
session of 2026-08-14.

## Decision

Ship a custom-pages feature where:

1. **Hybrid rendering.** The 3 existing tabs (`overview`, `cost`, `resources`)
   stay as bespoke React code in `Dashboard.tsx` — `CostTab`'s ~130 lines of
   inline composition (stats grid, `ProxyCard`, headroom section, local-models
   list) are **not** refactored into widgets. Custom pages are ordered
   widget-stacks rendered through a registry. Two rendering paths coexist:
   default tabs hardcode their widget imports; custom pages resolve widgets by
   slug from the registry.

2. **System-wide, schema-evolvable.** One global settings key `dashboard.pages`
   holds only custom pages (defaults are code, not data). Each page object in
   the stored JSON may later gain an additive `owner` field (null =
   system-wide); the key stays `dashboard.pages`; the GET handler resolves
   system-wide + current-user pages when Access Policy v2 lands. No key-shape
   migration — adding per-user is additive.

3. **Stack composer.** Layout is an ordered list of widget instances, full-width,
   drag-to-reorder — no spatial coordinates (x/y/w/h). Single-column is a
   degenerate case of a column composer, so multi-column can be added later as
   an additive enhancement without breaking stored layouts. Mobile reflows for
   free (a stack is a stack at any width).

## Persistence shape

```json
{
  "pages": [
    {
      "id": "<slug-or-uuid>",
      "name": "My Ops View",
      "widgets": [
        { "slug": "right-now", "props": {} },
        { "slug": "resource-trend", "props": { "window_": "24h", "rangeLabel": "24h" } }
      ]
    }
  ]
}
```

Stored under settings key `dashboard.pages` via the existing `store.Settings`
KV interface (`go/internal/store/store.go:556`), mirroring the established
settings-group pattern in `infra_handlers.go` (GET returns resolved value with
defaults applied; PUT decodes partial body, validates, merges at raw-JSON
level, persists, audits; `apply: "immediate"` — no reload/restart).

## Considered options

- **Uniform rendering** (finish Phase 5's extraction: refactor `CostTab`'s
  inline bits into widgets, then all pages — defaults included — render through
  the registry as data). Rejected as a bigger prerequisite sprint for no v1
  gain; `CostTab`'s bespoke composition is operator-curated and doesn't need to
  be user-editable.
- **Per-user from day one** (layout keyed by `user_id` even though only one
  operator exists today). Rejected as premature coupling to the auth identity
  system; the schema-evolvable `owner` field gives the same outcome without
  the coupling.
- **Free-form grid** (react-grid-layout style: drag widgets to x/y/w/h).
  Rejected as fragile across screen sizes (mobile = redesign) and inconsistent
  with the existing visual language (every current widget is a full-width
  card in a vertical stack — tiling them side-by-side looks awkward without a
  widget redesign).

## Consequences

- **Two rendering paths.** `Dashboard.tsx` renders the 3 hardcoded tabs +
  dynamic custom pages. The 7 existing widgets must register (so they're
  placeable on custom pages) even though default tabs don't render through the
  registry.
- **"Default page" is not a data-model concept.** The store holds only custom
  pages, appended after the 3 code tabs. No `is_default` flag, no seed data.
- **`CostTab`'s composition can never be user-editable** (acceptable — it's
  bespoke operator-curated layout, not a widget-stack).
- **Stale comment** at `Dashboard.tsx:36` still cites the deleted
  `lib/widgetRegistry.ts` — to be cleaned up when the registry is re-created.
- **Multi-column / free-form grid** remain additive future enhancements (a
  single-column stack is a degenerate case); stored layouts survive the upgrade.
- **Per-user pages** remain additive (optional `owner` field); no migration.

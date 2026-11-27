# One UI token schema and six component contracts shared by the Forge dashboard and Voice Studio, each keeping its own skin

Status: accepted (2026-11-04). Implementation: `docs/ui-remediation-plan-2026-11.md` — Sprint 1
(Forge) and Sprint 4 (Voice Studio). Evidence: `docs/ui-review-2026-11.md`.

## Context

Two web UIs serve the same operator: the Forge dashboard (`web/`, React, 2,285-line `theme.css`) and
Voice Studio (sibling repo, vanilla-JS PWA, 4 CSS files). Their *skins* are deliberately different —
cool slate/electric vs. warm espresso/amber — and the review kept that (`docs/ui-review-2026-11.md`
Part B). What the review found instead is that neither app has **structural** tokens, and both have
drifted into parallel component habits:

- Forge tokenizes color only: no spacing scale, no type scale (~30 literal font sizes, 8.5–48px),
  16 distinct radii from one `--radius` token, z-index literals 1–500 whose intended tiers exist only
  as a prose comment (`theme.css:1459-1464`), one shadow token.
- Voice Studio tokenizes color and a type ramp (`--t-*`) but not spacing, radius or z: two
  generations of the card system inside `styles.css`, 10 radii, ad-hoc z 40–100, an untokenized
  fourth accent.
- The accessibility defects in both apps trace to missing *contracts*, not missing styling: the
  Settings registry toggle is a mouse-only `<span onClick>`; 7 of ~9 Forge modals and all 5 Voice
  Studio overlays lack dialog semantics; JS-built forms and several Settings panels have unassociated
  labels; icon-only buttons carry bare-glyph accessible names ("▶", "⧉").

The operator decision (recorded in the review): **shared system, distinct skins** — one schema and
one set of component contracts, adopted by both apps; palettes and typography personalities stay
per-app.

## Decision

1. **Schema, not palette.** What is shared is the token *schema* — names, semantics, and one
   canonical value ladder per scale — plus the six component contracts below. Every color value and
   font family stays per-app. Adoption is incremental, not big-bang: new rules use the tokens; a
   file's existing literals migrate when that file is otherwise reworked (S2/S3 for Forge, S4-A for
   Voice Studio); existing tokens become aliases of the shared slots instead of being renamed.
   There is **no shared package or npm dependency** — the apps have different stacks and stay
   dependency-light; this ADR is the single source of truth, verified by each sprint's review pass.

2. **The scales** (px in both apps; both codebases are already px-based):

   | Scale | Tokens and values |
   |---|---|
   | Spacing | `--sp-1`..`--sp-8` = 4, 8, 12, 16, 24, 32, 48, 64 px. Exempt: 0–2px hairline gaps and component-specific metrics (e.g. `--bay-w`), which are not spacing. |
   | Type | `--fs-2xs:10`, `--fs-xs:11`, `--fs-sm:12`, `--fs-base:13`, `--fs-md:14`, `--fs-lg:16`, `--fs-xl:18`, `--fs-2xl:22`, `--fs-3xl:28`, `--fs-4xl:40`, `--fs-5xl:48` (px). 8.5–9.5px literals migrate to `--fs-2xs` when touched. Voice Studio's ramp aliases onto these at S4-A: caption→base, mono→md, body→lg, h2→2xl, h1→3xl, hero→4xl. |
   | Radius | `--r-xs:4`, `--r-sm:6`, `--r-md:8`, `--r-lg:12`, `--r-xl:16`, `--r-pill:999px`. Exempt: 0 and ≤2px corners. Forge's `--radius` becomes an alias of `--r-lg`; 18/22/26px literals migrate to `--r-xl` when touched. |
   | Z-index | `--z-base:1`, `--z-raised:10`, `--z-sticky:40`, `--z-overlay:50`, `--z-pop:60`, `--z-tour:70`, `--z-max:500` — the ladder the theme.css prose already documented (modal 50, popover/confirm 60, tour 70). New overlays/modals/tooltips take a tier token; ad-hoc literals are a defect. |
   | Elevation | `--shadow-1` (cards/panels), `--shadow-2` (modals/overlays), `--shadow-3` (pops/toasts), themed per app. Forge's `--shadow` becomes an alias of `--shadow-2`; Voice Studio defines all three. |

3. **Color: semantic slots, per-app values, and one new rule.** Existing color tokens are **not
   renamed**; the slot table below is the shared vocabulary for cross-app work. The one *additive*
   rule: **every accent that carries text or an active state defines an on-accent text token**
   (`--on-accent`), themed per-app, replacing hardcoded white-on-accent assumptions (Forge's
   `.tab.active`, `.seg-ctl[aria-pressed]`, `theme.css:295,1715,1785,2224`). Voice Studio's
   `--on-amber` (`styles.css:12-16`, with its own rationale comment) is the precedent: accent fills
   barely change between themes, so their text color must not blindly follow the theme flip — that
   bug is live in Voice Studio's light theme (cream-on-amber, ≈1.8:1).

   | Slot | Forge | Voice Studio |
   |---|---|---|
   | page background | `--bg`, `--bg-2` | `--ink` |
   | surface | `--panel`, `--panel-2` | `--panel`, `--panel-2` |
   | text primary / dim / muted | `--text`, `--text-dim`, `--text-mute` | `--paper`, `--paper-dim` |
   | status ok / warn / critical | `--ok`, `--warn`, `--crit` | (theme-local: `--wave`, `--amber`, `--coral` map loosely) |
   | accent + on-accent | `--tab-active` + **new `--on-accent`** | `--amber` + `--on-amber` |

4. **Six component contracts.** Each is an acceptance criterion: a control either conforms or is a
   defect in review. Both apps implement each contract once and reuse it.

   a. **Switch** — for every boolean setting: a native `input[type="checkbox"]` (or `role="switch"`
      with `tabIndex` and Space/Enter), one switch visual per app, keyboard-operable, state exposed
      to assistive tech, label associated. A segmented control is for 3+ exclusive values, never for
      a boolean.
   b. **Modal** — one shared shell per app: portal/append-to-body, `role="dialog"` +
      `aria-modal="true"`, labelled by its heading (`aria-labelledby`), initial focus to the first
      focusable or primary action, focus trapped inside, Escape closes, focus restored to the
      trigger. One modal at a time per host; no stacked innerHTML swaps (Voice Studio's current
      pattern) and no non-portal inline overlays (Forge's MaintenanceBanner popover).
   c. **Label rule** — every input has an associated label (`for`/`id`, or a wrapping label that
      contains text); a placeholder is never the label; help text is linked via `aria-describedby`
      (Forge's `Field`/`InfoTip` idiom is the reference implementation); the settings registry
      carries `help`/`unit`/`min`/`max`/`step` and renders them.
   d. **Icon buttons** — an accessible name is mandatory; a bare glyph ("▶", "⧉", "⋯") is not a
      name; `title` supplements but never substitutes. Name the action, not the glyph
      ("Play sample", "Copy ID").
   e. **Segmented control & nav** — one markup (`button` + `aria-pressed`) and one CSS class per
      app; navigation tabs carry `aria-current="page"` and their `<nav>` is labeled; nav pills are
      never reused as generic buttons (Forge's `.tab` reuse in smith movers/filters retires during
      S2/S3).
   f. **Button hierarchy** — primary / secondary / ghost / danger and sm/md sizes, one class set per
      app; destructive actions use the armed-confirm pattern (Forge's `ConfirmButton`), never a
      native `confirm()`/`alert()`.

5. **Verification protocol (normative for every UI change in either repo).**
   - Forge: `cd web && npm run lint && npm run build && npm run test`, then a browser pass in **both
     themes** and a keyboard-only pass of the touched flow.
   - Voice Studio: `go build ./... && go vet ./... && go test ./...`, then a browser pass in both
     themes.
   - Any color pairing touched: compute the WCAG ratio from the token values — ≥4.5:1 for text,
     ≥3:1 for large text (≥24px, or ≥18.66px bold) and meaningful UI indicators — and state the
     numbers in the change. Ratios are computed, not eyeballed.
   - Theme rule: nothing ships dark-only; both palettes are in scope for every change.

## Consequences

- Positive: one vocabulary for reviewing and building across both repos; the accessibility backlog
  becomes "conform to the contract" instead of bespoke fixes per surface; the extraction sprints
  (S2, S4-A) consume the scales directly; Voice Studio's track is unblocked without waiting for any
  Forge code.
- Positive: least-churn adoption — existing tokens become aliases (`--radius`, `--shadow`, VS's
  `--t-*`), so no big-bang rename and no branch-wide regressions.
- Cost: a token-migration effort already sized in the sprint plan (S1/S2 Forge, S4 VS); reviewers
  must check scale adherence during those sprints.
- Risk: real layouts fall between steps (Forge's 15px/11.5px type, 18/22/26px radii, VS's 20px h2).
  The rule is *adjust the layout to the scale when the file is touched*, reviewed in the browser —
  not add new steps. If a third of a file's usages genuinely fall between steps, amend the scale
  here rather than fork it per-app.
- Risk: two repos can drift from the schema. Mitigation: no schema change without an amendment to
  this ADR, and each sprint's review pass includes a scale-adherence check.
- Not decided here: whether the type/radius ladders later gain a step between 12 and 16 (wait for
  extraction evidence); PWA theme-color specifics (S4-D); the Voice Studio engine-status surface
  (S5); retiring VS's `--t-*` aliases entirely.

## Adopters

- **talk-studio** (2026-11-24, `go/cmd/talk-studio/web/style.css`): a third consumer, built outside the system at first (16 px type, 40-46 px controls, its own colors) and brought in line the same
  day: the spacing, type, radius, z-index and elevation scales above, `--on-amber` and a darker `--coral-strong` fill (white on `--coral` measured 3.94:1, on `--coral-strong` 6.02:1), the button
  hierarchy (ghost, amber primary, coral destructive, armed-confirm delete), and **Voice Studio's skin**, since it sits beside it (same voices, same host). A control is 32 px high and grows to
  44 px on a coarse pointer. Contrast was computed for every text pairing in both themes (all at least 5.08:1 except the pair fixed above).


# Multilanguage support — Japanese as the first added locale

Status: accepted.

## Context

The Forge (web PWA, Go backend, smith, and the `forge` TUI/CLI) has been English-only since
inception. There is no i18n library anywhere in the repo, `<html lang="en">` is hard-coded, and
every user-facing string is written directly into JSX, Go handlers, smith's embedded prompts, and
TUI/CLI rendering. The operator asked for multilanguage support, Japanese first, across all four
surfaces.

Investigation (2026-09-27) found:

- ~140 web TS/TSX files with ~570 text-bearing JSX lines, plus large prose data files
  (`settings/fields.ts` 49 KB, `help/Diagnostics.tsx` 37 KB, `help/AskSmith.tsx` 24 KB,
  `lib/llamaFlags.ts` 19 KB, `help/Guide.tsx` 17 KB, `onboarding/OnboardingTour.tsx` 10 KB). No
  test runner exists in `web/` (lint is oxlint only).
- `lib/format.ts`'s `Intl.*` calls already pass `undefined`/`[]` (the browser's own locale), so
  date/number formatting mostly "just works" once the app has a locale of its own to pass instead
  of relying on the browser's.
- Backend error responses (`httpapi.writeError`, ~408 call sites; `writeValidationError`, ~248)
  are almost all English prose, not stable machine codes. The a0 router's error bodies
  (`internal/router/proxy.go`) are consumed by real external clients (LibreChat, OpenCode) and its
  own tests key on the exact strings — these cannot change shape or language.
- Smith's KB search tokenizer (`kbTokenRe = [a-z0-9._-]+`, `internal/smith/kb.go`) drops all CJK
  characters, so a Japanese question currently retrieves zero KB chunks. The corpus itself is
  generated from `docs/*.md` by `cmd/kbsync` with a drift test — forking it per-language would
  double the maintenance burden of content that's supposed to track the docs automatically.
- The TUI (`internal/tui`, bubbletea/lipgloss) pads and truncates by counting runes, not display
  columns, so double-width Japanese text will overflow every fixed-width table. `go-runewidth` is
  already an indirect dependency.
- No webfont is loaded (`--ui: system-ui, ...`), so Japanese glyphs already render correctly on
  every OS Forge runs on. No font work is needed.

## Decision

Ship multilanguage support in five phases, one language (Japanese) at a time, across all four
surfaces the operator asked for:

1. **Foundation** — `i18next`/`react-i18next` for the web app (typed keys catch mistakes since
   there's no test runner), a small hand-rolled catalog package for Go (`internal/i18n`), and a
   glossary written before any string is translated.
2. **Web static UI** — extract the ~570+ JSX strings and the large prose data files into per-area
   translation namespaces, one area per commit, byte-identical English output at every step.
3. **Backend error codes** — an additive `code`/`params` layer next to the existing English
   `error`/`message` fields. The English text never changes shape; the frontend translates the
   code when it recognizes one and falls back to the English text otherwise.
4. **Smith** — a language directive threaded through `ChatOptions`, a ja→en term-expansion map so
   Japanese questions can still retrieve the (English-source) KB, and additive translation keys on
   deterministic findings. Gated on a Japanese `braineval` run against `gemma4-e4b-qat` — it has
   never been evaluated in Japanese and its grounding quality there is unknown until measured.
5. **TUI/CLI** — a matching Go-side catalog plus display-width-aware padding/truncation
   (`go-runewidth`, promoted from indirect to direct).

### What stays English, deliberately, in every phase

- The a0 router (port 8085) and MCP (port 8095) response bodies — external consumers and their
  own tests depend on the exact strings.
- Identifiers: slot IDs, config/model names, CLI flags, llama.cpp flags, `kind:ref` KB citations,
  setting keys, `--json` machine output.
- The smith KB corpus source and its citations — translated automatically-generated content that
  drifts from `docs/` is worse than English content the model reads and answers about in Japanese.
- Raw `err.Error()` passthroughs where no stable code exists yet — shown as an English detail
  string alongside a generic code, not fabricated into a translation.
- Operator-entered catalog data (model notes, descriptions) — that's the operator's own words, not
  ours to translate.

### Locale selection

Per-browser (`localStorage`, mirroring the existing `foundry.console.theme` pattern in
`App.tsx`), not a server-synced setting — this is a personal display preference, not shared
operational state, exactly like the theme toggle it sits next to. The TUI/CLI reads `FORGE_LANG`
(falling back to `LANG`/`LC_ALL`) since it has no browser to store a preference in.

## Consequences

- English behavior is unchanged at every commit — this is purely additive until a Japanese
  catalog exists to render instead.
- Full plan, phase-by-phase file list, and verification steps:
  `~/.claude/plans/tingly-jumping-tarjan.md`.
- Adding a third language later is mostly namespace/catalog work, not architecture work — the
  code/params error layer, the `Lang` field on smith's chat options, and the Go catalog package
  are all designed to take an arbitrary language code, not just `en`/`ja`.
- The KB corpus itself will read oddly if the operator later wants a fully Japanese-language KB
  browsing experience (not just Japanese smith answers) — that's a separate, larger feature
  (locale-keyed KB pages) explicitly deferred, not silently dropped.

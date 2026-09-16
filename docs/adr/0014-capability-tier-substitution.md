# Capability-tier substitution: curated substitution, never derived; usage stays keyed to what actually ran

> Renamed 2026-09-14 from "performance-level routing" — the mechanism ranks
> relative *capability* ("how smart"), not throughput or latency, and the
> original name was found to invite exactly that misreading during the
> feature's first live curation pass. No decision recorded below changed;
> only the name. Full rename (schema, settings key, Go identifiers, HTTP
> headers, UI copy): `progress.md`'s 2026-09-14 entry.

Sprints P1–P4 (2026-09-13) let a0 serve a request from an already-loaded
config in place of the one asked for — `fallback_only` when the requested
config is genuinely infeasible to load right now, `prefer_smarter` whenever
a resident config outranks the requested one. This ADR records the two
decisions with the widest blast radius: how "equivalent or better" is
defined, and what a substitution does to usage accounting.

## Considered Options — defining "equivalent or better"

- **Derived from `benchmarks` capability scores (rejected):** `registry.
  Capability.Score` looks like a ready-made ranking key, but it isn't one.
  Scores are sparse (a handful of models), parsed from a TEXT column with
  silently-ignored errors, inherited unchanged by quantized sibling
  configs (so a Q4 and a Q8 of the same model would rank identically), and
  `benesFor`'s union means two models' scores on the same capability ID
  routinely come from *different benchmarks* — a 0.86 from GPQA and a 0.84
  from AIME are not the same axis. Automatic ranking on this data would
  substitute on noise.
- **Curated tiers (chosen):** the operator hand-orders configs into named
  `capability_tiers`, each member carrying a rank (lower = more capable).
  Generalizes a pattern already live and load-bearing in this codebase —
  `smith.brain_chain`, an operator-ordered list of config names walked in
  preference order for the exact same question ("is some already-loaded
  model an acceptable stand-in?"). A table (not a settings-KV blob, unlike
  `brain_chain`) because capability-tier substitution needs *multiple*
  independent groups, and because a table gives referential integrity
  across config renames/deletes for free (`ON DELETE SET NULL` degrades a
  class deletion to "no substitution" rather than a dangling reference).
- **Curated tiers + automatic hard guardrails (chosen, additive):** the
  operator's ranking is trusted for *quality*, never for *safety*. A
  config the operator ranked as equivalent can still be refused as a
  substitute if it fails a gate the operator's ranking says nothing about:
  same-weights identity (ADR-0006 already establishes why this matters —
  see below), a modality subset violation, a shorter context length,
  hidden visibility, or (until Sprint P4 added a live probe) any
  tool-calling request. The operator can misjudge "how smart"; these gates
  exist so they can't misjudge "will this silently break the request."

### Same-weights refusal is not new policy — it is ADR-0006 applied here

ADR-0006's amendment already forbade exactly this shape of substitution:
"Coalescing traffic onto the resident instance was rejected: sibling
configs differ in generation flags (`--reasoning off` etc.), so routing a
`-nothink` request to a thinking-enabled server would silently change
behavior." Capability-tier substitution is a second, independent mechanism
that could have reintroduced that exact bug — an operator could rank
`gemma4-26b-a4b-nothink` above `gemma4-26b-a4b` in some future class and
`prefer_smarter` would happily "substitute" one for the other, silently
changing whether the response thinks. The same-weights gate
(`sameWeights` in `capability_substitution.go`, mirroring `engine.WeightIdentity`'s
"row IDs are not identity; files are" rule without importing the engine
package) makes that class of mistake structurally unrepresentable rather
than relying on the operator to remember not to make it.

### Tool-calling: fail closed, then relax against a live signal

No catalog column records tool-calling support as of Sprint P3, so the
first cut fails closed unconditionally: any request carrying
`tools`/`functions`/`tool_choice` never substitutes, full stop. Sprint P4
found a real, zero-cost signal already available — `/props`'
`chat_template_caps.supports_tool_calls` — and relaxed the gate to a live
probe of the *peer's* own capability (the peer must already be loaded to
be a candidate at all, so this is always a current answer, never a stale
or guessed one). `response_format` (structured output) has no equivalent
signal anywhere and stays an unconditional refusal; inventing one would be
guessing.

## Considered Options — usage accounting under substitution

- **Attribute substituted usage to the requested model (rejected):**
  would make `usage_events.Model` mean "what the caller asked for," which
  it has never meant — the collector's `OnTokenSample` path
  (`cmd/forge/main.go`) writes `Model: mode`, the slot's actual resident
  config, with no knowledge of which request or which requested name
  triggered a given token. Retrofitting requester-awareness into that path
  for this one feature would be a large, lossy change to a column whose
  existing meaning ("what ran on the GPU") is exactly what every other
  consumer of `usage_events` already assumes.
- **Leave `usage_events` unchanged; disclose via headers and audit
  instead (chosen):** `usage_events.Model` keeps meaning "what ran," true
  under substitution by construction — the substitute's own config name is
  what gets billed and profiled, because it's what actually executed. The
  gap this leaves — "which requests got substituted, and why" — is filled
  by `X-Forge-Model-Requested`/`X-Forge-Model-Served`/`X-Forge-
  Capability-Substitution`/`X-Forge-Capability-Substitution-Reason` response headers (set in
  `ModifyResponse`, before any bytes are copied, so the body is never
  rewritten — preserving the streaming no-buffering guarantee) and a
  `capability_substituted(...)` annotation appended to the audit trail's existing
  free-form detail string, where `Target` stays the originally-requested
  name. An operator who needs "how often did X get substituted for Y"
  greps the audit log; nothing about cost or profiling data becomes a lie.

## Consequence

A substitution decision is made at most twice per request (once before
`EnsureLoaded(want)`, once more only if that load then genuinely fails)
and never chains to a third config — see `catalogChain`'s doc comment
(`internal/router/routing.go`) for the control flow this forces, and
`internal/sched/place.go`'s `CouldLoad` for the non-mutating feasibility
check `fallback_only` depends on to avoid paying `ensure_loaded_timeout_s`
just to discover a load was never going to succeed.

# Context is append-only: reduce what is created, never edit what exists

Status: proposed.

## Context

Local inference here runs on llama.cpp slots whose throughput is bounded by prefill. Every request
re-sends the whole conversation; the only thing that keeps a long agentic session affordable is the
server reusing the KV/prefix cache for the unchanged head of that conversation. Through 2026-10-03 the
plan for "context compression" (`docs/v5-compression-redesign.md` §5, r1) was to *edit* context in
flight: a compressor hop (`forge-compress`, formerly headroom-ai) that word-prunes messages, plus
proposed lossless transforms, tiers/modes, and history clearing/masking. On 2026-10-04 Jon directed a
stronger principle (P0): reduce the *creation* of context; never edit existing context.

Evidence that supports P0:

* **Cache probe (WS-I, `docs/compression-redesign/cache-regime-results.md`, 9 configs on ForgeHost, real
  slots, llama.cpp's own `timings`).** Append-only turns reuse 99.6-99.7% of the prefix on every config.
  One word edited 50% into the first message drops reuse to 0.4-1.2% (a full re-prefill) on
  `qwen38-flash-next`, `qwen36-35b-a3b`, `qwen36a`, `ornith-35b`, `gemma4-e4b-qat` and `gpt-oss-120b`;
  only the plain-attention models recover about 45%. On the ~7.9K-token probe prompt the edit cost 29.6 s
  on the flagship (217 tok/s cold prefill); extrapolated, a rewritten 50K-token history is about
  4 minutes of prefill. This is the hybrid/recurrent/SWA pattern: llama.cpp cannot roll state back to an
  arbitrary point.
* **Session simulation (`docs/v5-compression-redesign.md` §4.3).** Even the "gentle" strategy, compress
  only results older than the newest two, cost 2.6x the prefill of doing nothing (852 s vs 324 s, ideal
  cache) because each result is re-prefilled as it ages past the window. Current whole-body Kompress
  saved 5% of tokens and left 0 of 9 newest results exact.
* **ADR-0015** found no safe tool-call compaction lever: the best method (a deterministic heuristic)
  safely compacts 1-3% of content; only about 15.8% of tool-call content is ever safe to drop.
* **Headroom lessons.** headroom-ai's `--lossless` mode went silently dead after the 0.35.0 upgrade and
  nothing noticed; its own code records that rewriting breaks edit anchors; the replacement
  `forge-compress` OOM-killed the host 16 times in about 2.5 h on unbounded input (Sprint 9). A hop that
  rewrites every message is a permanent source of silent failure and was never measured against the cache.
* **Where context is created** (`.sweep/compression-fidelity/creation_stats.py`, 571 sessions, 185 M
  chars): tool outputs 59.2%, model reasoning text 21.6%, tool-call arguments 14.8%, assistant text 3.7%,
  user text 0.6%. The mass is at the source, where we control tool outputs, tool schemas, reasoning
  effort and client configuration.

## Decision

**Context is append-only.** Nothing we operate (a0, a compressor, any proxy, or our own tooling) rewrites,
drops, reorders, truncates or re-encodes any element of a conversation already sent. The only sanctioned
way to shrink a context is to start a new one, seeded deliberately, leaving the old one untouched.
Reduction happens at creation: tool-layer output budgets with exact in-band continuation (never
summaries), tool-schema budgets, static per-consumer tool sets, reasoning-effort defaults, and client
configuration.

**Testable invariant (CONTRACTS C1).** For `POST /v1/chat/completions` and any other message-bearing
route, every element of `messages` forwarded upstream is identical to what was received.

**Allowed non-content edits** (enumerated; anything else needs a contract change and an amendment to this ADR):

| edit | where |
|---|---|
| `model` alias / virtual-model / capability-tier substitution (resolved name only) | existing router |
| reasoning parameter translation (`reasoning_effort` to template kwargs): request params, not history | `router/reasoning.go` |
| `stream_options.include_usage` injection (remote backends) | `router/usage.go` |
| auth / routing headers | existing |
| `tools[]` removal by a static per-consumer filter, only if D9 approves; never content-dependent (C6) | WS-H1 |

**Compressor retired (CONTRACTS C7).** `compressor.passthrough_all` is `true` and permanent (already true
on production, verified by WS-A on 2026-10-04, so the D1 bypass has been applied). No new compressor
proxies are provisioned on provider link; the Settings UI labels it retired; existing units stay idle until
WS-R2 deletes `forge-compress`, its units and UI after at least 14 days of ledger data. **Re-enabling any
message-rewriting hop requires an amendment to this ADR.**

**Observation instead of editing.** a0 gains an observe-only creation ledger (C2/C4): sizes and names
only, never content. It attributes created context to consumer/tool/model and detects clients that edit
their own history (prefix-hash mismatch, collapsed cache reuse) so violations are visible.

Superseded by this decision: r1's lossless transforms, the lossy Kompress path, the modes/tiers/policy
pipeline, the benefit panel, and history clearing/masking as something we do.

## Consequences

* Cancels the r1 design and its workstreams (B2, B3, C, D1, D2, E1-E3, G). `forge-compress` is dead
  weight pending deletion; ADR-0015's conclusion is reinforced.
* We keep the large cache win on every append (about 0.3 s instead of tens of seconds) and give up the
  small token savings compression offered (D8 accepts this).
* Reduction is only possible where we control the source: our MCP servers, a0 request parameters,
  client configuration, operator habits. For tools we do not own (OpenCode `read`/`grep`/`bash`,
  LibreChat attachments) the levers are configuration and upstream requests; the ledger shows what mass remains.
* Clients that edit their own history (e.g. a client-side clearing feature) are not blocked, only
  detected and reported.
* Tool filtering is the single subtractive request edit and is parked behind D9 and ledger data.
* The cache probe used one prompt shape and one edit depth; unprobed models, or future llama.cpp builds
  with better state rollback, could change the magnitude (not the direction). Revisit if a re-run shows
  edit reuse recovering on the hybrid models.
* Implementation note for C1 (resolved 2026-10-04, Jon): a0 re-serialises the body via `map[string]any` and `json.Marshal`
  (`mutateBody` in `go/internal/router/proxy.go`), which preserves JSON values but not bytes. The invariant is therefore
  **content-identical**: every `messages` element decodes to exactly the same JSON values (numbers compared losslessly). True raw-byte
  splicing is an optional, deferred workstream.

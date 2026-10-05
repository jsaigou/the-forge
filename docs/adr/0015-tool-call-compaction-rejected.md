# Tool-call compaction (model-based or formatting-based) rejected — no viable lever found

Status: accepted.

## Context

`forge-compress` (`go/internal/compress`, `docs/v5-headroom-replacement.md`) does token-level
extractive **compression**: it scores every word in every message on every request via
Kompress-v2-base and keeps the ones above threshold. It is deliberately stateless (a hard
decision from the OOM incident that killed headroom-ai in production) and has no concept of a
*stale tool call* — a `read` result from 40 turns ago that a later `edit` already superseded gets
scored identically to a live dependency, every single request.

A research spike investigated whether Laya (Convai Innovations' open-weight "System 1" typed-
decision classifier, Apache-2.0) or an equivalent LLM-based judge could add a **compaction**
layer on top — deciding whether a whole prior tool call is still needed, rather than scoring its
tokens. Full methodology, harness, and raw numbers:
`~/.claude/plans/polymorphic-scribbling-unicorn.md` (plan) and the session's write-up
(reproduced in the vault — see below; the throwaway Python harness lives outside this repo at
`~/laya-poc/`, per this repo's own convention that research harnesses aren't production code).

The investigation built a real dataset (100 OpenCode sessions from this machine, 3,393 real tool
calls in a frozen test split, deterministic hindsight ground truth — no LLM grades its own
answers) and tested every angle raised, including two the operator specifically asked for after
the first pass: a "dumb stripper" control (delete everything non-pinned, zero model cost) and a
deterministic-only alternative (no ML at all) after the model-based methods failed.

## Decision

**Do not build a tool-call compaction layer for `forge-compress`, by any method tested.**

Every method landed in the same narrow band, well short of a usable bar:

| method | keep_result AUC | safe compaction achieved (held out) |
|---|---|---|
| rule heuristic (supersession + recency) | 0.513 (chance) | — |
| Laya zero-shot classifier | 0.579 | 2.21% of content, at 1.29% false-drop |
| Flash-Next LLM teacher (local, same model smith uses) | 0.577 | not separately measured — same AUC band |
| CLM-v0.1-8B zero-shot (frozen Qwen3-8B + contrastive heads; added 2026-10-02) | 0.42–0.50 (chance, 3 framings) | not measured — blending with the deterministic heuristic below adds +0.0001 AUC even test-tuned; see `.sweep/laya-compaction-poc-2026-09-25.md` addendum |
| **improved deterministic heuristic** (tool-type rules + result-size signal, no model) | **0.873** | **3.33%** of content, at 1.60% false-drop |

The deterministic heuristic is the only one that cleared a reasonable "can it discriminate"
bar — for free — but the real, safe savings it unlocks are **1–3% of content**, not the 20–25%
that would justify new cross-message state and ongoing threshold maintenance. The reason is
structural, not a tooling failure: only ~15.8% of this dataset's tool-call content is *ever*
safe to compact by result, regardless of method quality (that's the true keep/drop/truncate
class balance) — a perfect oracle tops out below what would make this worthwhile.

A separate, distinct angle — stripping **formatting** (ANSI codes, trailing whitespace, blank-
line runs, decorative separators) from content that's being kept anyway — was also checked with
a real tokenizer (Qwen3.6-35B-A3B, this repo's own bake-off standard), not a character
approximation. Real tool output here carries almost no decorative overhead (zero ANSI codes in a
610-candidate sample), and BPE tokenization already compresses what little whitespace exists, so
the measured reduction was 0.03% of tokens — negligible. A provably-lossless JSON-minification
pass (parse → re-serialize compact) found a real 17.8% reduction, but only on the 2.2% of
candidates that are pure parseable JSON, netting 0.43% in aggregate.

**forge-compress remains compression-only, as designed.** The gap it doesn't cover (removing
whole stale tool calls) is real, but every lever tried to close it — model-based and
deterministic, whole-item and sub-item — tops out in the low single digits on real data.

## Why the LLM teacher didn't settle it either

The expectation going in was that a full LLM judge (Flash-Next, the same model this repo already
trusts as smith's reasoning tier) would set the ceiling other methods could be measured against.
It didn't — its AUC (0.577) landed in the same band as the small classifier, and its confusion
matrix showed a *worse* false-drop rate on genuinely-needed content (only 20.4% of true "keep"
candidates were correctly kept). The likely cause is the state each method was given, not a
capability limit: every method judged a candidate from a goal string and an isolated result
preview, never what actually happened between the call and the compaction point. A judge with a
fuller view of the conversation might do meaningfully better — this was not built or tested, and
is the one open thread left if this is ever revisited (see Consequences).

## Consequences

- No new code ships. `go/internal/compress` and `go/cmd/forge-compress` are unchanged.
- Two reusable operational findings survive independent of this rejection, both worth keeping in
  institutional memory for any future local-classifier or local-LLM-judge work on ForgeHost:
  1. **`torch`'s CPU intra-op threading is actively harmful for a small (~400M-param) classifier
     on this hardware** — 283ms at 1 thread vs. 8,926ms at 32 threads, monotonically worse with
     more threads (the ROCm-flavored torch build's OpenBLAS thread pool and torch's OpenMP layer
     both oversubscribe 16 physical cores for a batch-of-1 forward pass) — the opposite of how
     Kompress's own ONNX runtime scales on the identical box. Set `torch.set_num_threads(1)` for
     any future work like this.
  2. **`reasoning_effort: none` (already a real field on `internal/router`) is load-bearing for
     any structured-output call to a local reasoning model via a0.** Without it, a reasoning
     model's `reasoning_content` channel silently consumes the whole `max_tokens` budget before
     the actual answer ever appears — 37 of 41 real calls silently failed to parse on the first
     attempt of this investigation, with no exception raised (the HTTP call itself succeeded).
     Setting it also cut that phase's wall-clock 6.25x.
- If this is ever revisited, the one untested and potentially different approach is giving a
  judge (model or otherwise) real context about what happened *between* a tool call and the
  compaction point — not just an isolated preview and a goal string. That gap, not model choice,
  is the most likely reason every method here converged on the same weak result.
- Cross-reference: the operator-facing write-up lives in the vault (`Infra/` or an equivalent
  research-findings area) for anyone outside this repo's context to find without needing this
  repo's own history.

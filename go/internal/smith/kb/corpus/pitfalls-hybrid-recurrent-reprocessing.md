ref: pitfalls:hybrid-recurrent-reprocessing
doc: pitfalls
slug: hybrid-recurrent-reprocessing
title: Hybrid/Recurrent Models Force Full Prompt Reprocessing Every Turn (llama.cpp, not deployment-specific)
category: config
source: docs/pitfalls.md

## Hybrid/Recurrent Models Force Full Prompt Reprocessing Every Turn (llama.cpp, not deployment-specific)

Affects any mode whose GGUF architecture has SSM/recurrent layers alongside attention —
confirmed via GGUF metadata on **`ornith-35b`** and all **`qwen36`** variants (`general.architecture
= qwen35moe`, with `qwen35moe.ssm.*` fields present). Every turn of a multi-turn session logs:

```
forcing full prompt re-processing due to lack of cache data (likely due to SWA or hybrid/recurrent memory, ...)
```

...and reprocesses the **entire accumulated conversation** from scratch, even when the new
request is a 99%+ prefix match of the previous one. Per-turn latency grows with conversation
length (confirmed: prompt-eval throughput drops from ~1050 tok/s to ~350–400 tok/s within a
single reprocess pass as it works through the growing prompt) until it exceeds a calling
agent's client-side timeout — this is what caused the Ornith→DeepSeek Hermes failovers on
2026-07-01 and 2026-07-02, not a hardware fault. GPU temp/power/GTT usage stay completely
normal throughout; the GPU is doing genuine (if wasteful) work, not hung.

**`--swa-full` does not fix this for hybrid/recurrent models.** That flag (from llama.cpp PR
#13194) only disables sliding-window-attention cache eviction — it has no effect on recurrent
(SSM/Mamba/Gated-DeltaNet) state, which is the actual cause here. Gemma4 (26B-A4B and 31B) is
plain `gemma4` architecture with no SSM layers, so its own occurrences of this warning (166+
seen on `forge-secondary`) are the genuine SWA case and `--swa-full` should apply there — not
yet verified live, since surfacing this took priority.

**`--ctx-checkpoints` (alias `--swa-checkpoints`) is the flag the warning itself implies would
fix this** — it's the context-checkpoint/restore mechanism SWA and hybrid caches need to avoid
reprocessing. That setting predates this investigation and was established for a narrower
reason: preventing a GTT leak, confirmed only on `qwen36` (see `forge-v2-reference/CLAUDE.md`:
*"Required on qwen36... Gemma4 does not require it"*).

**Correction (2026-07-23, live config audit — see `docs/modes.md`):** the "blanket-applied to
all modes" claim above was never actually true of the live config, in either V4's `config.toml`
or v0.5's `forge.toml`. Only `swallow-32b`/`swallow-8b` actually set `--ctx-checkpoints 0`;
`qwen36` and every Gemma4/Ornith mode that sets it explicitly uses `16`, not `0`, and several
modes (`gpt-oss-120b`, `nemotron`, `nemotron-nano`, `nemotron-puzzle`, `qwen25coder`,
`qwen3coder-q6`) don't set it at all (llama.cpp default, currently 32). Whatever this repo's
docs previously believed about a blanket-`0` rollout, it didn't happen — treat this file's
"Every Forge mode currently sets `--ctx-checkpoints 0`" claim (and any other doc repeating it)
as historical/aspirational, not current fact.

**Do not just flip `--ctx-checkpoints` to a positive value without re-reading
[investigations.md](investigations.md) item 9 first.** As of 2026-07-02, upstream llama.cpp has
*no maintainer-approved fix* for hybrid/recurrent checkpoint restore, and there are currently
**open, unresolved hang bugs** in that exact code path — including one reproduced on
`Qwen3.6-35B-A3B MoE` specifically (llama.cpp#22450). Enabling checkpoints today risks trading
"slow but working" for "actually hangs." Full research trail, upstream PR/issue timeline, and
the unblock condition are tracked there — check it before touching this flag again.

**Separate failure mode — Ornith's broken embedded chat template (FIXED 2026-08-03):** the
original `ornith-1.0-35b-Q8_0.gguf` shipped with a `tokenizer.chat_template` whose
assistant-turn branch rendered `<think>` **unconditionally** (no `last_query_index` guard,
md5 `fa5eb1eee497fc8f3bccc3fcf60d1a51`). Every prior turn's reasoning was re-injected into the
prompt, so on long agentic sessions the model re-read its own stale plan and re-issued the
same tool call — presenting as an **infinite tool-call loop**, plus context inflation per turn
(byte-identical to [HF discussion #31](https://huggingface.co/deepreinforce-ai/Ornith-1.0-35B/discussions/31)).
This is distinct from (and independent of) the full-reprocess issue above. Fixed by swapping to
the unsloth build (`Ornith-1.0-35B-Q8_0.gguf`, template md5 `b3f24e8a...`) **and** forcing
froggeric v21.3 via `--chat-template-file`; see `docs/modes.md`.

**Text-repetition loops → samplers + `preserve_thinking`, not the template (2026-08-03):** a
separate "repeats the same text output" symptom is a **sampling** issue, not the template bug.
Default llama-server samplers are `repeat_penalty 1.0` (off), `dry_multiplier 0.0` (off), temp
1.0 — the exact profile that degenerates into text-repetition loops on reasoning-heavy MoE
models. Ornith now sets `--repeat-penalty 1.08 --dry-multiplier 0.9 --temp 0.7`. **Also
required for Ornith specifically: `--chat-template-kwargs '{"preserve_thinking":false}'`** —
Ornith is Qwen 3.5 lineage, which must NOT preserve historical reasoning; froggeric v21.3's
default `preserve_thinking: true` made the model burn its whole token budget inside `<think>`
with empty `content` (verified live). This is the opposite of `qwen36`/`qwen36-mtp`, which
correctly use `{"preserve_thinking":true}` because Qwen 3.6 is the preserving lineage.

**Third, separate failure mode — reasoning-budget not enforced + reasoning leak (FIXED
2026-08-04):** even with the two fixes above, Ornith's `<think>` length is highly variable
(100–4300+ tokens) and could still blow past `--reasoning-budget` entirely, starving a
small-`max_tokens` client request of any room for `content` — a documented, industry-wide
reasoning-model failure mode (DeepSeek R1, Gemini hit the same class of bug), not specific to
this fleet. A previous agent's attempted fix (`--reasoning off`) was believed deployed but never
actually was — caught via the audit log and a direct `ps` check on the live process, not
assumed. Root-caused instead to two llama.cpp bugs already fixed upstream as of commit
`91c631b21` (2026-07-13): `99f3dc322` (server silently discarded per-request
`reasoning_budget_tokens` overrides) and `91c631b21` itself (reasoning leak specific to
force-opened bare `<think>` templates — directly applicable, froggeric v21.3 is exactly that).
See `docs/modes.md`'s Ornith entry for the full fix/test/deploy writeup. **The "Kintsugi" hybrid-cache
patch below was found to be living only as uncommitted working-tree edits on the deployment host** (no git
history at all, despite the name implying a maintained fork) — now committed
(`fcd4e598d`/rebased to `52bb51280`) and backed up off-host; see that same `docs/modes.md` entry.

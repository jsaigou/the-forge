ref: research:qwen36-mtp-results
doc: research
slug: qwen36-mtp-results
title: 1. Qwen3.6-35B-A3B — MTP Results
category: model-selection
source: research.md

## 1. Qwen3.6-35B-A3B — MTP Results

### ✓ RESOLVED 2026-05-19 — 79 t/s observed on mainline build

**Production result:** `qwen36-mtp` mode (mainline build, UD-Q4_K_XL, draft-mtp, 131K ctx): **~79 t/s**

| Config | Build | tok/s |
|---|---|---|
| Q6_K_P, no spec | llama-mtp fork | ~47 |
| Q4, no spec | llama-mtp fork | 50.2 |
| Q4 + draft-mtp | llama-mtp fork | 47.7 |
| **UD-Q4_K_XL + draft-mtp n=3** | **mainline PR #22673** | **~79** |

1.68× improvement over the production Q6 mode. The "zero speedup" finding was specific to the old
fork build. The mainline implementation is significantly more efficient for MTP on this architecture.

### Why the fork gave zero speedup but mainline doesn't
The old fork (`am17an/llama.cpp`) had the MTP draft path wired up but the mainline merge (PR #22673)
included further optimisation work (device-to-host embedding transfers, MTP scheduling, GGML graph
integration) that was not in the fork snapshot we built from. The DeltaNet recurrence constraint is
still present but the mainline is sufficiently efficient that MTP's batched verification adds net value.

### Previous zero-speedup analysis (now superseded)
~~MTP enabled, 98% acceptance rate, zero tok/s improvement~~ — was specific to the fork build.
DeltaNet SSM layers are sequential at the state level, but the mainline MTP implementation amortises
the overhead of multiple token evaluations in a way the fork did not.

### Research tasks

**R1.1 — Confirm DeltaNet as bottleneck via profiling**
- Use `rocprof` or llama.cpp's built-in `--verbose` timing to break down per-layer time
- Determine what fraction of forward-pass time is DeltaNet vs attention vs MoE
- If DeltaNet < 50% of time, the current hypothesis is wrong and something else is limiting

**R1.2 — Track llama.cpp for DeltaNet-aware MTP**
- Monitor `ggml-org/llama.cpp` issues and PRs for "DeltaNet MTP", "SSM speculative", "chunked MTP"
- The chunked DeltaNet kernel (`fused Gated Delta Net (chunked)`) already parallelises prefill —
  a similar approach might be applicable to short MTP draft sequences
- Watch am17an's repo (`am17an/llama.cpp`) for follow-up PRs building on PR #22673

**R1.3 — Test throughput at 128K context (practical lower bound)** ✓ DONE
- Measured 2026-05-15 using llama-server /completion timings:

| Config | tok/s |
|---|---|
| 128K baseline | 46.93 |
| 128K + ngram | 46.93 |
| 524K baseline (current production) | 46.95 |

- **Context size has zero effect on Qwen3.6 throughput.** All three results are within 0.02 tok/s.
- This definitively confirms DeltaNet as the bottleneck: its recurrent state update is fixed-size
  regardless of context, and KV cache bandwidth (which grows with context for attention models)
  is simply not in the critical path.
- Kills the "community 80 tok/s claims were at short context" hypothesis — if KV cache were the
  bottleneck, 128K vs 524K would show a clear gap. It doesn't.
- ngram shows no benefit on essay/prose generation (no repetitive structure to exploit);
  the earlier agentic coding gain is real but workload-specific (code variable/pattern repetition).

**R1.4 — Evaluate ngram speculation as fallback** ✓ DONE
- Confirmed: ngram is slightly faster on agentic coding workloads
- `--spec-type ngram-map-k` bypasses DeltaNet entirely — pattern matching on previous output
- Gain is modest but real on structured/repetitive output; zero cost on cache misses

**R1.5 — Investigate `--spec-draft-ctx-size` for draft context reduction**
- The MTP draft context currently allocates KV for the full 524K context (even for the 1 MTP layer)
- If `--spec-draft-ctx-size N` can limit draft KV to a smaller window (e.g., 8K),
  draft-gen cost per token might drop significantly
- Check if this flag applies to internal (same-GGUF) MTP or only external draft models

---

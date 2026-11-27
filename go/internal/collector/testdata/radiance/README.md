# Recorded Radiance /metrics scrapes

Real, unedited `/metrics` output of the Radiance server used in the sequence/context study
(`docs/radiance-seqs-context-study-2026-11-11.md`), recorded on ForgeHost on 2026-11-11 and copied
read-only on 2026-11-12 from `~/radiance-test/results/`. Radiance build commit `ac51376`
(version 1.17.0), model `qwen35moe` (Qwen3.6-35B-A3B fp8), `--tp 1 --kv-cache-dtype fp8`, 1 sequence,
`--max-model-len 262144`.

| File | Source on ForgeHost | Recorded |
|---|---|---|
| `seqstudy-s1-l262144-d4.metrics-{ready,idle,after-reach,final}.txt` | `seqstudy-s1-l262144-d4.metrics-*.txt` (draft depth 4) | 18:42 to 18:46 |
| `seqstudy-s1-l262144-d0.metrics-{ready,idle,after-reach,final}.txt` | `seqstudy-s1-l262144-d0.metrics-*.txt` (no speculation) | 19:02 to 19:06 |

`ready` = right after the server answered /health; `idle` = after the warm-up request;
`after-reach` = after the needle-in-haystack request that filled the context (236,863 prompt tokens,
first answer after 233.6 s); `final` = after the last request. Nothing here carries a key.

## What these scrapes show about progress counters (used by the hang rule)

Every scrape was taken with `vllm:num_requests_running 0`: the study scraped between requests,
never during one. So **no fixture is a mid-prefill scrape.** What they do show, d4 idle -> after-reach
(one 236,863-token prefill plus the study's other requests in between):

- `radiance:engine_steps_total` 3 -> 473, of which `engine_steps_prefill_total` 1 -> 134;
- `radiance:prefill_tokens_total` 15 -> 266,928, i.e. about 2,000 tokens per prefill step, so a prefill
  of that size is made of roughly 130 steps spread over the 233 s, one about every 1.7 s;
- `vllm:generation_tokens_total` / `radiance:decode_tokens_total` do not move during the prefill.

That is consistent with the source comment (`core/server/metrics.cpp`: these counters are "charged
per step") and means a mid-prefill scrape every 2 s should see the step counter move. It does not
prove it; a scrape taken during a long prefill is still needed.

Also visible: `radiance:prefill_tokens_per_second` is 47.48 and `decode_tokens_per_second` 0.43 in the
after-reach scrape with nothing running. The rate gauges hold a value at idle, so the Radiance hang
rule does not consult them.

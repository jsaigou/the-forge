ref: modelselection:kv-cache-grows-with-context
doc: modelselection
slug: kv-cache-grows-with-context
title: KV Cache Grows with Context
category: model-selection
source: modelselection.md

### KV Cache Grows with Context

The 1.2× rule assumes modest context. At very large contexts (512K–1M tokens), the KV cache dominates:

```
KV cache ≈ 2 × n_layers × n_ctx × n_kv_heads × head_dim × bytes_per_element
```

For a 131K-layer attention transformer at 1M context with q4_0 K-cache and q8_0 V-cache, the KV cache adds several GB. At 1M context with a dense 70B model this can exceed 40 GB. MoE and hybrid-SSM models fare better because their SSM layers carry **no KV cache** — state is fixed-size regardless of sequence length.

**Cache quantization flags** (`--cache-type-k`, `--cache-type-v`) are essential at long context:
- `q4_0` for K: 0.5 bytes/element, ~4× reduction vs F16
- `q8_0` for V: 1 byte/element, ~2× reduction
- Quality loss is minimal for most workloads; V-cache is more sensitive than K-cache

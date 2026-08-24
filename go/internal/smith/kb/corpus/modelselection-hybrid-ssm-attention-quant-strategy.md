ref: modelselection:hybrid-ssm-attention-quant-strategy
doc: modelselection
slug: hybrid-ssm-attention-quant-strategy
title: Hybrid SSM/Attention (Qwen3.6, Nemotron Super)
category: model-selection
source: modelselection.md

### Hybrid SSM/Attention (Qwen3.6, Nemotron Super)

Models like Qwen3.6-35B-A3B and Nemotron-3-Super-120B-A12B interleave standard attention blocks with SSM (state space model) layers — GatedDeltaNet in Qwen3.6, Mamba-style in Nemotron.

Key properties:
- SSM layers maintain a **fixed-size recurrent state** — no KV cache growth with context length
- SSM state updates are **sequential per token** — each token depends on the previous state
- KV cache only grows for the attention layers

This makes hybrid SSMs memory-efficient at long context but with a sequential compute constraint that affects speculative decoding (see MTP section).

---

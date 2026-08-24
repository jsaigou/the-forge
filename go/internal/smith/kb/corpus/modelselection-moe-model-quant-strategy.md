ref: modelselection:moe-model-quant-strategy
doc: modelselection
slug: moe-model-quant-strategy
title: MoE Models (Qwen3.6-35B-A3B, Nemotron-3-Super-120B-A12B)
category: model-selection
source: modelselection.md

### MoE Models (Qwen3.6-35B-A3B, Nemotron-3-Super-120B-A12B)

Only a fraction of parameters are active per token (e.g., Nemotron: 12B active out of 120B total). VRAM/RAM usage reflects total params; compute reflects active params. Result: MoE models load large but decode fast. The inactive expert weights must still reside in memory — they are read during routing even if not used for the full forward pass.

**Quant implication:** because experts are rarely activated together, lower-precision quants have less cumulative error — the importance matrix approach works very well. IQ variants and UD quants with elevated embedding precision are particularly effective for MoE.

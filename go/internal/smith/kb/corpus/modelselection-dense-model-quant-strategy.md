ref: modelselection:dense-model-quant-strategy
doc: modelselection
slug: dense-model-quant-strategy
title: Dense Models (Gemma 27B, Llama 70B)
category: model-selection
source: modelselection.md

### Dense Models (Gemma 27B, Llama 70B)

All parameters active on every token. Inference is almost purely memory-bandwidth-bound — smaller quants mean fewer bytes per weight, faster weight reads, proportionally faster decode. A 33% file size reduction from Q6→Q4 translates to near-linear speed gain (~30–35% faster decode). Quantization sweet spot: **Q4_K_L or IQ4_NL** — good quality, maximum speed.

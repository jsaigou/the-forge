ref: modelselection:picking-a-quantization-level
doc: modelselection
slug: picking-a-quantization-level
title: Picking a quantization level
category: model-selection
source: modelselection.md

### Picking a quantization level

1. **Calculate the VRAM budget:**
   `budget = total_available_RAM × 0.85` (leave 15% for OS and KV cache overhead)

2. **Check file size fits:**
   `file_size × 1.2 ≤ budget`

3. **At the same file size, prefer:**
   IQ4_NL ≈ UD-Q4_K_XL > Q4_K_L > Q4_K_M

4. **Always choose `_L` over `_M`** when the size difference is < 1 GB — the embedding quality improvement is free.

5. **For long context (> 256K tokens):** drop one quant level to leave room for KV cache. Enable `--cache-type-k q4_0 --cache-type-v q8_0`.

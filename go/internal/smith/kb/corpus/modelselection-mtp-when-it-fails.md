ref: modelselection:mtp-when-it-fails
doc: modelselection
slug: mtp-when-it-fails
title: When MTP fails
category: model-selection
source: modelselection.md

### When MTP fails

- **Mamba SSM models (Nemotron Super)**: Mamba-2 recurrent states are sequential — verification cost ≈ generation cost. Net speedup: zero.
- **Qwen3.6 without MTP heads**: The stock abliterated Q6 GGUF has no embedded MTP tensors; enabling MTP adds overhead for zero benefit. Use the `MTP-GGUF` variant (UD-Q4_K_XL) for Qwen3.6 MTP — mainline PR #22673 achieves 1.7× confirmed on this deployment.
- **High-entropy outputs**: Creative writing, free-form reasoning. Low acceptance rates mean overhead exceeds gains.
- **MoE routing instability**: Some MoE models have draft acceptance rates below 50%, making ngram speculation competitive or better.

ref: modelselection:mtp-when-it-fails
doc: modelselection
slug: mtp-when-it-fails
title: When MTP fails
category: model-selection
source: docs/modes.md

### When MTP fails

- **Mamba SSM models (e.g. Nemotron Super)**: Mamba-2 recurrent states are sequential - verification cost approximately equals generation cost. Net speedup: zero.
- **A base model without embedded MTP heads**: a stock GGUF with no MTP tensors gets zero benefit from enabling MTP - it just adds overhead. Use an MTP-specific GGUF variant instead; mainline llama.cpp PR #22673 has measured roughly a 1.7x speedup on models built with matching draft heads.
- **High-entropy outputs**: Creative writing, free-form reasoning. Low acceptance rates mean overhead exceeds gains.
- **MoE routing instability**: Some MoE models have draft acceptance rates below 50%, making ngram speculation competitive or better.

ref: modelselection:red-flags-before-you-download
doc: modelselection
slug: red-flags-before-you-download
title: Red flags before you download
category: model-selection
source: modelselection.md

### Red flags before you download

- `file_size × 1.2 > available RAM` → will OOM or force context reduction
- MXFP4 on AMD → expect a 10+ tok/s penalty until ROCm kernels improve
- MTP GGUF for an SSM/hybrid model → overhead with zero benefit
- Q6_K on a model where Q5_K_L fits → Q5_K_L at the same memory budget often gets you more via imatrix calibration
- `--parallel` left at default → multiply base memory by 4; almost always causes OOM on consumer hardware

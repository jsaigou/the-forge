ref: modelselection:mtp-when-it-works
doc: modelselection
slug: mtp-when-it-works
title: When MTP works
category: model-selection
source: modelselection.md

### When MTP works

- **Pure attention transformers** (Llama, Mistral, Gemma 4): Verification is a single parallel forward pass. At 60–80% acceptance rate, expect **30–50% throughput improvement**.
- **Dense models**: Lower memory bandwidth per verification step since fewer weights are read.
- **Repetitive or predictable output**: Code generation, templated responses, retrieval-augmented answers.

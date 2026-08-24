ref: modelselection:the-1.2x-rule
doc: modelselection
slug: the-1.2x-rule
title: The 1.2× Rule
category: model-selection
source: modelselection.md

### The 1.2× Rule

```
VRAM needed ≈ GGUF file size × 1.2
```

The overhead covers: KV cache (at default context), runtime buffers, framework state, and compute workspace. Example — this deployment (120 GB unified system RAM): models up to ~100 GB GGUF file size are viable.

ref: pitfalls:silent-context-reduction
doc: pitfalls
slug: silent-context-reduction
title: Contiguous GTT allocation can silently reduce n_ctx
category: config
source: docs/pitfalls.md

- **Contiguous Allocation:** The kernel may fail to find large contiguous GTT blocks. Always verify `n_ctx` via `/props` after startup - it silently downscales on allocation failure.

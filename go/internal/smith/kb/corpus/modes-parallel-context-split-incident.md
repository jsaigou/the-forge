ref: modes:parallel-context-split-incident
doc: modes
slug: parallel-context-split-incident
title: The `--parallel` context-splitting behavior
category: config
source: docs/modes.md

`--parallel N` in llama.cpp does **not** allocate N times the configured context - it
**splits** the single configured `context` value across N slots. `/props`'
`default_generation_settings.n_ctx` (what the engine reads to verify a load) reports the
**per-slot** value: `configured_context / parallel`.

For example, a config with `context=131072` and `--parallel 2` reports
`default_generation_settings.n_ctx: 65536` in `/props` - exactly half. This is easy to
misdiagnose as a GTT/kernel silent context reduction (the failure mode covered elsewhere in this
corpus), but it is not that: the model genuinely never gets the full configured context per
conversation, and every request is capped at the per-slot value, not the configured one. It is
working exactly as `--parallel N` is documented to work, not a bug or a kernel-level allocation
failure.

For a config meant to serve one long conversation at its full configured context, set
`--parallel 1`. A higher value only makes sense for a short-context, high-concurrency
fast-worker config where several small, independent requests are the actual intended use - and
even then, it comes at a real throughput cost, since compute is split across concurrent slots
instead of dedicated to one request at a time.

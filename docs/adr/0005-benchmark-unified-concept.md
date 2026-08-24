# Benchmark is one concept with three sources and three subject levels

Capability scores (GPQA, HLE), performance metrics (decode_tps, safe_memory_bytes), provider-reported specs (context_length), and quality assessments (abliteration_quality) were previously scattered across separate structures (`models.toml [capabilities]`, `[performance]`, `[quality]`, `model_profiles` table, `provider_models` columns). We unified them under one concept — **Benchmark** — with a `source` discriminator ("published" | "self_measured" | "provider_reported") and a subject reference that can point at a Model, Variant, Config, or Offering.

This enforces the F7 fabrication-prevention gate structurally: published benchmarks require `source_url` + `source_date` at validation — unsourced scores are rejected, not just discouraged (the V4 failure mode was comments-as-citation with no enforced provenance). It also prevents the apples-to-oranges comparison trap: self-measured benchmarks are local-only and per-Config (controlled, reproducible via PROFILE); provider-reported specs are per-Offering and reflect the provider's cap, not the model's intrinsic capability.

## Considered Options

- **Unified Benchmark with source + subject (chosen):** One table, one validation gate, one query surface. F7 gate is structural.
- **Separate tables per kind (rejected):** capability_scores, performance_metrics, provider_specs, quality_assessments. More schema, more validation code, and the F7 provenance gate would need to be duplicated or forgotten on the capability_scores table specifically (which is exactly where the fabrication happened).

# Offering is distinct from Config, not unified under Model

Remote provider models (e.g. `moonshotai/kimi-k2.7-code` on AI&) share a consumer-facing identity with local models (both are route targets for a0), but share almost nothing else: no weights on disk, no Engine/Build, no launch recipe, no local lineage, no self-measured benchmarks. We considered unifying them under one Model entity with nullable fields (Option A), but the moment you try, every concept except Model is null — a sign the unification is forced. Instead, **Offering** is a first-class concept: a remote availability of a (Model, Variant) through a specific Provider, carrying `wire_model`, pricing, currency, context_length, and a Provider FK. A Model can have both local Configs and remote Offerings simultaneously (e.g. Qwen3-Coder-Next: load locally or route to AI&/OpenRouter).

The same Model on two Providers is two distinct Offerings — they are not interchangeable, even when the underlying weights are identical, because they differ in data residency (inherited from Provider), cost, currency, reliability, and provider-reported caps. The dashboard must surface these differences; silent cross-provider fallback with different data residency is a footgun (the catalog provides the data; fallback policy is a router concern, not catalog).

## Considered Options

- **Offering as distinct concept (chosen):** Model and Variant are hosting-agnostic. Config is local-only; Offering is remote-only. Both are route targets. Clean separation, no nullable-everywhere Model entity.
- **Unify under Model with nullable fields (rejected):** Forces Model to carry nulls for Variant, Artifact, Config, lineage on every remote entry. The "everything's a Model" framing collapses under the weight of what's actually different.

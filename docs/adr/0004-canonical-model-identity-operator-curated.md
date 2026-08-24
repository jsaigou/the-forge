# Canonical Model identity is operator-curated, not auto-derived

Provider `wire_model` names are inconsistent across providers — different casing, org prefixes, version suffixes, model-vs-chat naming (e.g. `deepseek-chat` on deepseek.com vs `deepseek-v3` on AI&; `moonshotai/kimi-k2.7-code` vs `kimi-k2.7-code`). Algorithmic matching is unreliable. We decided canonical Model identity is **operator-curated**: when adding an Offering, the operator picks which existing Model it belongs to (or creates a new one). OSS Models can be seeded from their HuggingFace repo; proprietary Models (GPT-4, Claude) are manually created. When two Models are later discovered to be the same, they are merged (Offerings re-pointed to the survivor).

This is not a matching problem — it's a **distinguishing** problem. The same model on different providers is a *different thing* from the consumer's perspective because of data residency. The Offering IS the disambiguation; the curated Model identity is what lets you see "these are the same weights, available two ways, with consequential differences."

## Considered Options

- **Operator curation + HF seed + merge (chosen):** No auto-detection. HF seed for OSS origins. Merge as escape hatch for when curation was wrong. Matches how HuggingFace (`base_model` is author-curated) and OpenRouter (human-mapped canonical namespace) handle the same problem.
- **Algorithmic matching by wire_model name (rejected):** Names are too inconsistent across providers — casing, org prefix, version suffix, model-vs-chat naming all vary. Would produce wrong matches.
- **Don't unify — each Offering is its own card (rejected):** Loses the ability to compare providers for the same model and to show one card per real Model with Offerings listed.

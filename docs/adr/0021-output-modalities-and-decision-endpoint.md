# The catalog says what a model can produce, and a0 serves decision models on `/v1/systemone`

Status: accepted (2026-11-02). Implementation: `docs/lfm3-capability-plan.md` (Integration sprints). Evidence:
`docs/lfm3-evaluation-results.md`.

## Context

The catalog described only what a model can **read**: `models.modalities` and the nullable `configs.modalities`
override hold `text`, `vision` and `audio` (migration 0028). Every model was assumed to **produce** text, and `a0`
hard-coded `output_modalities: ["text"]` on `/v1/models`.

Liquid AI's LFM3 family (2026-10-27) broke that assumption. The two `d1` models are native **decision models**: given a
`state` and named questions they return typed answers (`choice`, `score`, `noul`) with probabilities in one forward
pass and generate no text. The Audio and Omni models produce **speech**. Upstream llama.cpp (our production build,
plain `a91eab6`, ADR-0019) already serves both: a decision model on `POST /v1/systemone` (the TypeSafe System One
API) and speech on `/tts`, and it reports `architecture.output_modalities` (`decisions`, `audio`) from `/v1/models`.

What was measured before deciding (details in the results doc):

- The upstream TypeSafe spec has a required `model` body field. llama-server accepts it and **ignores** it, so a wrong
  name still answers with the loaded model. A router that wants named routing must validate the name itself.
- d1 rejects any prompt over `--ubatch-size` tokens (default 512) with a 500, because the whole prompt must fit one
  physical batch, and each question re-reads the whole state. `--batch-size 16384 --ubatch-size 16384` works to about
  14K tokens per question and costs roughly 4-5 GB extra per slot.
- On two synthetic routing sets d1-2B and d1-Omni-2B matched a prompted `gemma4-e4b-qat` judge in accuracy at about
  46 ms for two questions against about 172 ms for one, and return per-option probabilities.

## Decision

1. **Output modalities are a second two-level catalog fact**, parallel to the input one: `models.output_modalities`
   (what the architecture can produce, default `["text"]`) and nullable `configs.output_modalities` (per-config
   override; NULL derives). Values: `text`, `audio` (the model speaks) and `decisions`. They mirror llama-server's
   `architecture.output_modalities` so there is one vocabulary. Migration `0098` is additive; the previous binary keeps
   running on the new schema, and a model written without the field reads back as `["text"]`.
2. **`store.ResolveOutputModalities` is the single resolver** (the config override wins; `audio` also needs a linked,
   present mmproj because the speech codec lives in the projector; `decisions` and `text` never depend on it). It
   differs from the input resolver on one point on purpose: it does **not** force `text`. A decision model resolves to
   exactly `["decisions"]`.
3. **a0 serves `POST /v1/systemone`, spec-compatible.** `model` is in the body and a0 resolves and validates it the same
   way it does for chat (catalog config, alias, hidden, unknown), loads the model on demand, applies the capability and
   media gates, proxies the body unchanged, and audits and counts the request. There is no failover and no capability
   substitution: decision models are not interchangeable.
3a. **Capability errors are explicit.** `unsupported_capability` (400) when a decisions request names a model that does not
   produce decisions, and when a chat request names a decision-only model. The existing `unsupported_modality`,
   `model_not_found` and `model_not_routable` keep their meaning.
4. **`/v1/models` hides decision-only models by default**, so clients that list every model (LibreChat, OpenCode) are
   unchanged. `?output=decisions` returns the decision-capable ones. Every entry still carries its real
   `architecture.output_modalities` (`audio`, `decisions`), where it used to say `["text"]` for everything.
5. **d1 configs ship with `--batch-size 16384 --ubatch-size 16384`**, with the limit documented: about 16K tokens per
   question, and cost grows with the number of questions.

## Consequences

- Positive: the Forge can serve a fast classifier with calibrated-looking probabilities and no generated text, discover
  it from `/v1/models`, and route to it by name like any other model. The same output vocabulary covers speech models
  for the later audio phases without another schema change.
- Positive: nothing changes for existing consumers. Text models keep `["text"]`, the default listing is unchanged, and
  the migration is additive.
- Cost: a d1 slot costs memory for the large batch (7.6 GB measured for d1-2B), and long states with many questions
  are slow (28K total tokens took 7.3 s).
- Cost: two more badge kinds, two more form toggles and two more locale entries per language; the web UI mirrors the
  input toggles.
- Risk: the catalog value could disagree with what a loaded slot reports. The decision is to trust the catalog (it works
  for unloaded models, which is the point) and verify against `/v1/models` on the slot during onboarding.
- Not decided here: the OpenAI-shaped audio adapter (`/v1/audio/transcriptions`, `/v1/audio/speech` for slot models), the
  realtime WebSocket proxy, and residency for interactive speech. They stay sketches in the plan.

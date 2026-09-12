-- SPDX-License-Identifier: Apache-2.0
-- Schema v79 (a0 model-discovery sprint, 2026-09-13). Two real modality
-- facts the catalog was never told, both now load-bearing: a0's /v1/models
-- is becoming the single source of truth for OpenCode's model capabilities
-- (see BuildModelsResponse/store.ResolveModalities), so a model whose
-- vision wiring is invisible to that resolution rule is now advertised to
-- every consumer as text-only, not just displayed inconsistently on one
-- page as before.
--
-- Both UPDATEs follow the guarded, no-op-safe idiom already used by
-- 0028_modalities.sql / 0020_gemma_logo.sql — a no-op on any DB shaped
-- differently than the one this was written against.

-- (1) DeepSeek V4.1-Flash (wire model deepseek-flash) is natively
-- multimodal, but its `models` row still carries the '["text"]' default
-- from 0028, which only ever set vision on three LOCAL models. `offerings`
-- has no modality column of its own (see store.Offering) -- the joined
-- `models` row via offerings.model_id is the ONLY source a0 has for a
-- remote provider's vision support. Keyed through offerings.wire_model
-- rather than models.name so a display-name edit can't make this guard
-- miss.
UPDATE models SET modalities = '["text","vision"]'
WHERE id IN (SELECT model_id FROM offerings WHERE wire_model = 'deepseek-flash')
  AND modalities = '["text"]';

-- (2) qwen38-flash-next's mmproj was wired 2026-09-12 as a raw
-- `--mmproj <path>` appended to configs.extra_args, not via
-- mmproj_artifact_id (no artifact-creation HTTP route exists yet -- see
-- progress.md's 2026-09-12 entry). Nothing parses --mmproj out of
-- extra_args anywhere in Go, so store.ResolveModalities sees
-- MMProjArtifactID == 0 and correctly-by-its-own-rule-but-wrongly-in-fact
-- concludes "text only". An explicit configs.modalities override is the
-- modelled way to say otherwise. The EXISTS guard makes this a no-op on
-- any DB where that flag isn't actually present; the IS NULL guard never
-- clobbers an operator's own override.
--
-- Remove this override if/when a real mmproj artifact row is registered
-- for this config (i.e. once store.CreateArtifact is wired to httpapi and
-- someone points mmproj_artifact_id at it instead) -- at that point the
-- derive rule owns the fact correctly on its own again.
UPDATE configs SET modalities = '["text","vision"]'
WHERE name = 'qwen38-flash-next'
  AND modalities IS NULL
  AND EXISTS (SELECT 1 FROM json_each(configs.extra_args) WHERE value = '--mmproj');

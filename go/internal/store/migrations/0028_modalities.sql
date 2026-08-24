-- SPDX-License-Identifier: Apache-2.0
-- Schema v28 (Sprint J1, pre-release readiness round 2, 2026-08-05). Modality
-- is a typed, two-level fact, not a `key_features` string: `models.modalities`
-- is what the architecture supports (text/vision/audio); `configs.modalities`
-- is nullable and overrides the model-level default when a specific config
-- can't deliver everything the model architecturally supports (missing
-- mmproj, a build lacking mtmd support for that modality, a modality
-- stripped during quantization). NULL means "derive from the model + mmproj
-- presence" (see registry.resolveModalities); a config never needs an
-- explicit row unless its real capability diverges from that default.
ALTER TABLE models  ADD COLUMN modalities TEXT NOT NULL DEFAULT '["text"]';
ALTER TABLE configs ADD COLUMN modalities TEXT;

-- Data fix: `Multimodal` was a free-text key_features string standing in for
-- what this column now expresses properly. Every WHERE below is guarded by
-- name (same no-op-safe pattern as 0020_gemma_logo.sql /
-- 0024_comfyui_logo.sql) -- a no-op on any DB without these exact models.
--
-- Model-level modalities, researched (not guessed) against each model's own
-- HuggingFace card:
--   - Nemotron 3 Nano Omni: NVIDIA's card lists input types "Video, Audio,
--     Image, Text" -> text+vision+audio at the architecture level.
--   - Gemma 4 26B A4B (MTP): "Vision (image input) via mmproj", no audio.
--   - Qwen3.6 35B (Aggressive / MTP): Text/Image/Video only, no audio
--     (video folds into vision -- there is no separate enum value for it).
UPDATE models SET modalities = '["text","vision","audio"]'
WHERE name = 'Nemotron 3 Nano Omni';

UPDATE models SET modalities = '["text","vision"]'
WHERE name IN ('Gemma 4 26B A4B (MTP)', 'Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP');

-- Config-level override: `nemotron-nano`'s deployed build (build_id 5,
-- standard-vulkan, last commit 2026-07-21) predates llama.cpp's
-- Parakeet/audio mtmd support (PR #22520, merged upstream 2026-07-28 --
-- confirmed absent via `grep -ri parakeet` across the build's own source
-- tree, zero hits). The model is architecturally omni-capable; this config
-- cannot deliver audio until the build is updated, so it gets an explicit
-- override rather than silently inheriting a modality it can't serve.
UPDATE configs SET modalities = '["text","vision"]'
WHERE name = 'nemotron-nano';

-- Drop the superseded 'Multimodal' key_features string, now that the typed
-- column carries the same fact. json_each-filtered (not an exact-string
-- guard) because the four rows are not uniformly formatted -- Gemma's was
-- seeded with space-separated JSON ('["Uncensored", "MoE", ...]') while the
-- other three are compact, an artifact of different write paths over time.
-- Scoped to these 4 names only, so it can't touch an unrelated model; a
-- name with no 'Multimodal' entry is an inherent no-op either way.
UPDATE models
SET key_features = (
  SELECT json_group_array(value)
  FROM json_each(models.key_features)
  WHERE value != 'Multimodal'
)
WHERE name IN (
  'Nemotron 3 Nano Omni', 'Gemma 4 26B A4B (MTP)',
  'Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP'
);

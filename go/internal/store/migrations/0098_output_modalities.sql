-- SPDX-License-Identifier: Apache-2.0
-- LFM3 integration (docs/lfm3-capability-plan.md, ADR-0021): the catalog only said what a model can READ (models.modalities:
-- text/vision/audio). Decision models (d1) produce typed decisions and no text, and speech models produce audio, so
-- what a model can PRODUCE is a second, separate fact. Same two levels as the input side: models.output_modalities is what
-- the architecture can produce; configs.output_modalities is a nullable per-config override (NULL = derive).
-- Values: text, audio (speech), decisions. The default keeps every existing model exactly as it was: it produces text.
-- Widening: both columns are additive, so the previous binary keeps running on this schema.
ALTER TABLE models  ADD COLUMN output_modalities TEXT NOT NULL DEFAULT '["text"]';
ALTER TABLE configs ADD COLUMN output_modalities TEXT;

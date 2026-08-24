-- SPDX-License-Identifier: Apache-2.0
-- Schema v26 (Sprint D — benchmarks surfaced, pre-release readiness,
-- 2026-08-04). Data restore, not a schema change: subject_type='model' has
-- always been a valid benchmarks row (0008_model_catalog.sql), and is the
-- correct scope for a capability score (intrinsic to the weights, not the
-- quant/build) — but the models.toml -> catalog migration (Phase B,
-- 2026-07-24) never carried these rows forward, so every model card's
-- Capabilities list has been empty since. historical/models.toml still has
-- the F7-corrected curated set (2026-07-23); each row here matches it after
-- a fresh re-verification against currently published sources this sprint,
-- with two corrections:
--   - gpt_oss_120b.knowledge: historical value 43.1% cited
--     intuitionlabs.ai/articles/openai-gpt-oss-open-weight-models, which
--     does not mention Humanity's Last Exam anywhere. The real figure, from
--     OpenAI's own model card (arxiv 2508.10925, Table 3, "high" reasoning):
--     19.0% with tools / 14.9% without. Corrected to 19.0% (with tools, to
--     match this file's existing "w/ tools" convention).
--   - nemotron_super.knowledge: historical value 18.26% cited arxiv
--     2604.12374 (NVIDIA's own paper), which does not mention Humanity's
--     Last Exam anywhere either (confirmed by a full-text search of the
--     paper). No reliable alternative primary source was found this
--     session. Dropped rather than re-publish an unverifiable citation —
--     matches this repo's own rule against fabricated benchmark data.
-- gemma4_31b_mtp's two rows are deliberately not included: that model no
-- longer exists in the live catalog as of this sprint (superseded by
-- "Gemma 4 26B A4B (MTP)" at some point after 2026-07-29's OOM
-- investigation) — the INSERT below is a natural no-op for it via the
-- `SELECT id FROM models WHERE name = ...` join, same as any other
-- non-matching row in this migration pattern.
-- Each INSERT is guarded by NOT EXISTS so re-running this migration (or
-- running it against a DB that already has these rows some other way) is a
-- no-op, matching 0020_gemma_logo.sql / 0024_comfyui_logo.sql / 0025_model_logos.sql.

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.843', 'published',
       'https://tech-insider.org/google-gemma-4-open-model-benchmarks-2026', '2026-07-23',
       'model', m.id, 'GPQA Diamond'
FROM models m WHERE m.name = 'Gemma 4 31B (MTP)'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'reasoning');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'knowledge', '0.227', 'published',
       'https://artificialanalysis.ai/models/gemma-4-31b', '2026-07-23',
       'model', m.id, 'Humanity''s Last Exam'
FROM models m WHERE m.name = 'Gemma 4 31B (MTP)'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'knowledge');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.823', 'published',
       'https://artificialanalysis.ai/models/gemma-4-26b-a4b', '2026-08-04',
       'model', m.id, 'GPQA Diamond'
FROM models m WHERE m.name = 'Gemma 4 26B A4B (MTP)'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'reasoning');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'knowledge', '0.172', 'published',
       'https://artificialanalysis.ai/models/gemma-4-26b-a4b', '2026-07-23',
       'model', m.id, 'Humanity''s Last Exam'
FROM models m WHERE m.name = 'Gemma 4 26B A4B (MTP)'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'knowledge');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.86', 'published',
       'https://lushbinary.com/blog/qwen-3-6-developer-guide-benchmarks-architecture-api-self-hosting', '2026-08-04',
       'model', m.id, 'GPQA Diamond'
FROM models m WHERE m.name IN ('Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP')
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'reasoning');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.734', 'published',
       'https://www.buildfastwithai.com/blogs/qwen3-6-35b-a3b-review', '2026-08-04',
       'model', m.id, 'SWE-bench Verified'
FROM models m WHERE m.name IN ('Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP')
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'coding');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.515', 'published',
       'https://www.labellerr.com/blog/qwen3-6-35b-a3b-open-source-ai-model/', '2026-08-04',
       'model', m.id, 'Terminal-Bench 2.0'
FROM models m WHERE m.name IN ('Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP')
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'agentic_logic');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.623', 'published',
       'https://arxiv.org/pdf/2604.12374', '2026-07-23',
       'model', m.id, 'GPQA Diamond'
FROM models m WHERE m.name = 'Nemotron 3 Super 120B'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'reasoning');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.809', 'published',
       'https://huggingface.co/openai/gpt-oss-120b', '2026-08-04',
       'model', m.id, 'GPQA Diamond'
FROM models m WHERE m.name = 'GPT-OSS 120B'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'reasoning');

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'knowledge', '0.190', 'published',
       'https://arxiv.org/html/2508.10925v1', '2025-08-05',
       'model', m.id, 'Humanity''s Last Exam'
FROM models m WHERE m.name = 'GPT-OSS 120B'
  AND NOT EXISTS (SELECT 1 FROM benchmarks b WHERE b.subject_type = 'model' AND b.subject_id = m.id AND b.metric = 'knowledge');

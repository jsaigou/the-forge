-- 0049_feedback_round3_catalog.sql
-- Operator feedback 2026-08-14 (round 3): catalog data fixes.
--
-- (1) Qwen regroup: "Qwen 3 to Qwen 3.4 should be treated as one family.
--     Qwen 3.5 to Qwen 3.9 should be treated as a different family." The
--     3.0-gen family "Qwen 3" keeps Qwen3 Coder Next; the 3.6-point
--     releases (Qwen3.6 35B x2) plus the remote 3.6/3.7/3.8 API models
--     merge into one family renamed "Qwen 3.5+".
--
-- (2) Carbon 8B: creator was empty (no company watermark rendered —
--     "missing a company logo for huggingscience") and the model logo was
--     the family's uploaded raster. Set the corporate creator and the new
--     authored carbon8b vector mark (vendor-icons.mjs, 2026-08-14).
--
-- (3) Published benchmarks for the remote offering model cards, which had
--     zero model-scoped rows ("all the model cards for offerings are
--     missing benchmarks"). Figures are vendor-published scores sourced
--     2026-08-14 from official model cards / release blogs; capability
--     metric vocabulary mirrors the existing rows (reasoning = GPQA
--     Diamond, coding = SWE-bench, agentic_logic = Terminal-Bench,
--     knowledge = HLE), values are 0-1 fractions.
--
-- All statements are name/id-guarded and no-op on a fresh DB (0046
-- pattern), so this is safe to ship to every install.

-- (1) Qwen 3.5+ regroup. Rename the 3.6 family, then move the remote
-- 3.6/3.7/3.8 API models into it (guarded so an already-regrouped or
-- empty DB is untouched).
UPDATE families SET name = 'Qwen 3.5+'
  WHERE name = 'Qwen 3.6'
  AND EXISTS (SELECT 1 FROM models WHERE name = 'qwen3.8-max');

UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'Qwen 3.5+')
  WHERE name IN ('qwen3.6-flash', 'qwen3.7-plus', 'qwen3.8-max')
  AND EXISTS (SELECT 1 FROM families WHERE name = 'Qwen 3.5+');

-- (2) Carbon 8B identity.
UPDATE models SET creator = 'HuggingScience', logo = 'carbon8b'
  WHERE name = 'Carbon 8B';

-- (3) Published benchmarks (source_url + source_date required by the
-- published-source CHECK constraint).
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.901', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro', '2026-06-22', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'deepseek-v4-pro';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.806', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro', '2026-06-22', 'model', id, 'SWE-bench Verified'
  FROM models WHERE name = 'deepseek-v4-pro';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.879', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro-0813', '2026-08-13', 'model', id, 'Terminal-Bench 2.1'
  FROM models WHERE name = 'deepseek-v4-pro';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.881', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash', '2026-06-22', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'deepseek-v4-flash';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.790', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash', '2026-06-22', 'model', id, 'SWE-bench Verified'
  FROM models WHERE name = 'deepseek-v4-flash';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.827', 'published', 'https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash-0731', '2026-07-31', 'model', id, 'Terminal-Bench 2.1'
  FROM models WHERE name = 'deepseek-v4-flash';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.912', 'published', 'https://huggingface.co/zai-org/GLM-5.2', '2026-06-17', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'glm-5.2';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.621', 'published', 'https://huggingface.co/zai-org/GLM-5.2', '2026-06-17', 'model', id, 'SWE-bench Pro'
  FROM models WHERE name = 'glm-5.2';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.810', 'published', 'https://huggingface.co/zai-org/GLM-5.2', '2026-06-17', 'model', id, 'Terminal-Bench 2.1'
  FROM models WHERE name = 'glm-5.2';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.926', 'published', 'https://www.alibabacloud.com/blog/qwen3-8-max-a-new-bar-for-coding-and-cowork_603421', '2026-08-03', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'qwen3.8-max';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.677', 'published', 'https://www.alibabacloud.com/blog/qwen3-8-max-a-new-bar-for-coding-and-cowork_603421', '2026-08-03', 'model', id, 'SWE-bench Pro'
  FROM models WHERE name = 'qwen3.8-max';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.866', 'published', 'https://www.alibabacloud.com/blog/qwen3-8-max-a-new-bar-for-coding-and-cowork_603421', '2026-08-03', 'model', id, 'Terminal-Bench 2.1'
  FROM models WHERE name = 'qwen3.8-max';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.903', 'published', 'https://www.alibabacloud.com/blog/qwen3-7-plus-multimodal-agent-intelligence_603206', '2026-06-03', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'qwen3.7-plus';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.777', 'published', 'https://www.alibabacloud.com/blog/qwen3-7-plus-multimodal-agent-intelligence_603206', '2026-06-03', 'model', id, 'SWE-bench Verified'
  FROM models WHERE name = 'qwen3.7-plus';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.703', 'published', 'https://www.alibabacloud.com/blog/qwen3-7-plus-multimodal-agent-intelligence_603206', '2026-06-03', 'model', id, 'Terminal-Bench 2.0'
  FROM models WHERE name = 'qwen3.7-plus';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.860', 'published', 'https://huggingface.co/Qwen/Qwen3.6-35B-A3B', '2026-04-16', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'qwen3.6-flash';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'coding', '0.734', 'published', 'https://huggingface.co/Qwen/Qwen3.6-35B-A3B', '2026-04-16', 'model', id, 'SWE-bench Verified'
  FROM models WHERE name = 'qwen3.6-flash';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.515', 'published', 'https://huggingface.co/Qwen/Qwen3.6-35B-A3B', '2026-04-16', 'model', id, 'Terminal-Bench 2.0'
  FROM models WHERE name = 'qwen3.6-flash';

INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'reasoning', '0.935', 'published', 'https://huggingface.co/moonshotai/Kimi-K3', '2026-07-16', 'model', id, 'GPQA Diamond'
  FROM models WHERE name = 'kimi-k3';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'agentic_logic', '0.883', 'published', 'https://huggingface.co/moonshotai/Kimi-K3', '2026-07-16', 'model', id, 'Terminal-Bench 2.1'
  FROM models WHERE name = 'kimi-k3';
INSERT INTO benchmarks (metric, value, source, source_url, source_date, subject_type, subject_id, notes)
SELECT 'knowledge', '0.435', 'published', 'https://huggingface.co/moonshotai/Kimi-K3', '2026-07-16', 'model', id, 'Humanity''s Last Exam'
  FROM models WHERE name = 'kimi-k3';

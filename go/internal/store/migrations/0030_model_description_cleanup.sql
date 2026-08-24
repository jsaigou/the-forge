-- SPDX-License-Identifier: Apache-2.0
-- Schema v30 (pre-release feedback round, 2026-08-06). Data fix, not a
-- schema change: most model descriptions restated the model's own name,
-- quantization suffix (Q4_K_M, Q8_0, UD-Q6_K_XL, ...), and context length —
-- all of which the card/detail views already surface as their own
-- structured fields (card title, "Configured/Trained context" spec row,
-- "File size"/variant name). Trimmed to the parts no other field carries:
-- fine-tune/variant branding, architecture notes, and use-case framing.
-- Each UPDATE is keyed on the exact old description text, so it's a no-op
-- on any DB where the row doesn't exist or was already edited since.
UPDATE models SET description = '120B-A5B, HauhauCS Aggressive abliterated.'
WHERE name = 'GPT-OSS 120B' AND description = '120B A5B HauhauCS Aggressive abliterated 131K context.';

UPDATE models SET description = 'HauhauCS Uncensored fine-tune, QAT, with MTP (multi-token prediction) drafting.'
WHERE name = 'Gemma 4 26B A4B (MTP)' AND description = 'Gemma 4 26B A4B HauhauCS Uncensored QAT Q4_K_M + MTP';

UPDATE models SET description = '30B-A3B MoE, translation-focused.'
WHERE name = 'Hy-MT2 30B' AND description = 'Hy-MT2 30B-A3B Translation';

UPDATE models SET description = '30B-A3B.'
WHERE name = 'Nemotron 3 Nano Omni' AND description = '30B-A3B 128K context';

UPDATE models SET description = 'MoE coding model — 256 experts / 8 active, agentic coding.'
WHERE name = 'Ornith 1.0 35B' AND description = 'Ornith-1.0-35B MoE coding model — 256 experts / 8 active, 262K context, agentic coding.';

UPDATE models SET description = 'Fast worker for short concurrent requests.'
WHERE name = 'Qwen2.5 Coder 7B' AND description = 'Qwen2.5-Coder-7B Q4_K_M — fast worker, 32K context.';

UPDATE models SET description = 'Max-fidelity solo coder.'
WHERE name = 'Qwen3 Coder Next' AND description = 'Qwen3-Coder-Next UD-Q6_K_XL — max fidelity solo coder, 262K context.';

UPDATE models SET description = 'RL-tuned Japanese/English, fast worker.'
WHERE name = 'Qwen3 Swallow 8B RL' AND description = 'Qwen3-Swallow-8B-RL-v0.2 Q4_K_M — RL-tuned Japanese/English, fast worker, 131K context.';

UPDATE models SET description = 'HauhauCS Aggressive abliterated — thinking, multimodal.'
WHERE name = 'Qwen3.6 35B (Aggressive)' AND description = 'Qwen3.6-35B-A3B HauhauCS Aggressive abliterated Q6_K_P — thinking, 524K context, multimodal.';

UPDATE models SET description = 'Stock — draft-MTP n=3, ~79 t/s (1.7x production qwen36).'
WHERE name = 'Qwen3.6 35B MTP' AND description = 'Qwen3.6-35B-A3B-MTP stock UD-Q4_K_XL — draft-mtp n=3, 256K context, ~79 t/s (1.7× production qwen36).';

UPDATE models SET description = 'RL-tuned Japanese/English.'
WHERE name = 'Swallow 32B' AND description = 'Qwen3-Swallow-32B-RL-v0.2 Q8_0 — RL-tuned Japanese/English, 40K context.';

-- SPDX-License-Identifier: Apache-2.0
-- Schema v72 — smith.comfyui.keep_files seam (S7-followup smith UX sprint,
-- 2026-08-26, operator feedback: comfyui_prune re-proposed the same
-- rejected-for-deletion files every sweep with no memory of the rejection).
--
-- proposeComfyUIDelete (propose.go) now filters comfyui_prune's candidate
-- list against this operator-maintained exclusion list (full paths) before
-- building a delete_files proposal — a direct "keep this" that doesn't
-- require building a real ComfyUI workflow the way the existing
-- comfyUIKeepGuidance advice does. Also: comfyui_prune itself no longer runs
-- in the automatic deep sweep at all (checks.go's new Check.ManualOnly) —
-- this list only matters for files that keep surfacing across on-demand
-- checks the operator explicitly triggers.
--
-- TWO-LAYER KNOWLEDGE ARCHITECTURE (same rule as every other smith.* seam
-- added this series): ships EMPTY, no deployment-specific file paths in
-- source. The operator maintains this list per-install (a "Keep" button on
-- the delete_files proposal's file rows, web/src/components/smith/
-- DeleteFilesCard.tsx).

INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.comfyui.keep_files', '[]', strftime('%s', 'now'));

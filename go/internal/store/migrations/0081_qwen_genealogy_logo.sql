-- SPDX-License-Identifier: Apache-2.0
-- Schema v81 (operator feedback, 2026-09-13). Data fix, not a schema
-- change: the "Qwen" genealogy's uploaded logo is Alibaba's corporate mark,
-- not Qwen's own — every Qwen 3.5+ model inherits it via
-- registry.resolveLogos (0040_icon_inheritance_takeover.sql's model-level
-- clear left these models with logo='', correctly deferring to the parent,
-- but the parent mark itself was wrong). web/src/assets/icons/manifest.ts
-- already vendors a real, distinct "qwen" brand mark (used by
-- creatorIcon.ts's CREATOR_ALIASES for any model whose bare `creator` is
-- "qwen") — this points the genealogy at that same slug instead of the
-- uploaded image, matching the DeepSeek/Gemma/Kimi/Ornith genealogies,
-- which already reference manifest slugs rather than raw uploads.
-- Guarded on the row still holding an uploaded (data URI) image, so this is
-- a no-op against a DB where the logo was never wrong, already fixed, or
-- deliberately set to something else.
UPDATE genealogies SET logo = 'qwen', logo_dark = ''
WHERE name = 'Qwen' AND logo LIKE 'data:%';

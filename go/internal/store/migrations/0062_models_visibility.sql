-- SPDX-License-Identifier: Apache-2.0
-- Catalog decommission workstream (2026-08-22): model-level visibility flag,
-- mirroring configs.visibility (0011). visible (default) = model card shows
-- in the Models gallery; hidden = Settings-only (operator management), card
-- excluded from GET /api/v1/models/cards and config-scoped Cards for configs
-- under a hidden model. Lets decommissioned models keep catalog history rows
-- while their cards disappear and their local GGUF files get deleted.
ALTER TABLE models ADD COLUMN visibility TEXT NOT NULL DEFAULT 'visible'
CHECK (visibility IN ('visible', 'hidden'));

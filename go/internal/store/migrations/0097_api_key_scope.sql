-- SPDX-License-Identifier: Apache-2.0
-- ADR-0020 F11: a key could only be all-or-nothing for its role, so an agent that needed to edit the catalog got an
-- admin key. scope narrows a forge key to a part of the API. '' (every existing key) is unscoped, exactly as before;
-- 'catalog' admits only /api/v1/catalog/* (enforced in the HTTP auth middleware) and must expire within 24 hours
-- (enforced when the key is minted).
ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT '' CHECK (scope IN ('', 'catalog'));

-- SPDX-License-Identifier: Apache-2.0
-- Foundry → Forge rebrand: the canonical dashboard key kind is now 'forge'.
-- The api_keys CHECK constraint (0001) only admits ('foundry','router','mcp'),
-- so minting a 'forge' key would fail on every existing database. Rebuild the
-- table with both kinds admitted; legacy 'foundry' rows are kept verbatim so
-- pre-cutover minted sk-foundry-* keys keep authenticating until re-mint
-- (authz.ParseToken normalizes the legacy prefix).
CREATE TABLE api_keys_new (
    keyid        TEXT    PRIMARY KEY,
    kind         TEXT    NOT NULL CHECK (kind IN ('forge', 'foundry', 'router', 'mcp')),
    name         TEXT    NOT NULL,              -- agent/consumer identity (requested_by)
    secret_hash  TEXT    NOT NULL,              -- Argon2id encoded string
    role         TEXT    CHECK (role IN ('viewer', 'operator', 'admin')), -- forge/foundry kind only
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER                        -- soft revoke; NULL = active
);

INSERT INTO api_keys_new
    (keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at)
SELECT keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at
FROM api_keys;

DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;

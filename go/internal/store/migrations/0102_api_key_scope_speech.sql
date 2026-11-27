-- SPDX-License-Identifier: Apache-2.0
-- docs/speech-routing-plan.md (P1): a third key scope, 'speech', for the speech front door (forge-tts). It admits only the two speech-engine calls
-- (enforced in the HTTP auth middleware), may live up to a year, and must be bound to a loopback address (both enforced when the key is minted).
-- The scope column carries a CHECK constraint that only admits '' and 'catalog' (0097), and SQLite cannot alter a constraint, so rebuild the table
-- with the same columns and the wider list. Every existing row is kept verbatim.
CREATE TABLE api_keys_new (
    keyid        TEXT    PRIMARY KEY,
    kind         TEXT    NOT NULL CHECK (kind IN ('forge', 'foundry', 'router', 'mcp')),
    name         TEXT    NOT NULL,              -- agent/consumer identity (requested_by)
    secret_hash  TEXT    NOT NULL,              -- Argon2id encoded string
    role         TEXT    CHECK (role IN ('viewer', 'operator', 'admin')), -- forge/foundry kind only
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER,                       -- soft revoke; NULL = active
    display_name TEXT    NOT NULL DEFAULT '',
    bound_ip     TEXT    NOT NULL DEFAULT '',
    expires_at   INTEGER,
    scope        TEXT    NOT NULL DEFAULT '' CHECK (scope IN ('', 'catalog', 'speech'))
);

INSERT INTO api_keys_new
    (keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at, display_name, bound_ip, expires_at, scope)
SELECT keyid, kind, name, secret_hash, role, created_at, last_used_at, revoked_at, display_name, bound_ip, expires_at, scope
FROM api_keys;

DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;

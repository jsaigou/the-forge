-- SPDX-License-Identifier: Apache-2.0
-- Schema v4 (Sprint 0-AUTH — docs/v5-sprint0-auth-design.md §4).
-- Auth-v2 tables are intentionally separate from 0002_polish.sql and
-- 0003_headroom_rename.sql, and are created empty here. BE-AUTH Phase A/B/C
-- fill them at runtime.
-- Conventions: unix-seconds INTEGER timestamps, 0/1 booleans, JSON as TEXT.

-- Assurance recorded on the session (elevation state).
ALTER TABLE sessions ADD COLUMN assurance        TEXT    NOT NULL DEFAULT 'password';
ALTER TABLE sessions ADD COLUMN assurance_at     INTEGER;         -- when elevated
ALTER TABLE sessions ADD COLUMN network_principal TEXT;            -- if network-bootstrapped

-- Link local accounts to trusted network principals.
CREATE TABLE identity_links (
    provider   TEXT    NOT NULL,          -- 'tailscale' | 'forward_auth_header'
    principal  TEXT    NOT NULL,          -- e.g. 'user@github'
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (provider, principal)
);

-- WebAuthn credentials (one row per registered authenticator).
CREATE TABLE webauthn_credentials (
    id           TEXT    PRIMARY KEY,     -- credential id (base64url)
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    public_key   BLOB    NOT NULL,        -- COSE public key
    sign_count   INTEGER NOT NULL DEFAULT 0,
    transports   TEXT,                    -- JSON array
    label        TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
);

-- TOTP secrets (one active per user; enrollment is confirm-before-activate).
CREATE TABLE totp_secrets (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret     TEXT    NOT NULL,          -- base32; §9 Q6: rely on 0600 DB file
    confirmed  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- Recovery codes (hashed) for TOTP/passkey lockout.
CREATE TABLE recovery_codes (
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT   NOT NULL,            -- HMAC-SHA256(pepper, code), hex (see auth design §8.1)
    used_at  INTEGER
);

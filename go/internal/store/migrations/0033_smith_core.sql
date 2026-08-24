-- SPDX-License-Identifier: Apache-2.0
-- Schema v33 (smith P0 contracts — docs/v5-smith.md §4.4/§4.6/§5).
-- Renumbered from 0032 → 0033: the operator's multi-provider routing sprint
-- (router_providers.enabled + offerings.priority) landed first and owns v32.
-- The smith is the Foundry self-diagnosis agent (internal/smith). This
-- migration creates its core table set — conversations + messages (Tier 2
-- chat, consumed from P3), investigations + findings (Tier 1 diagnostics,
-- consumed from P1), and actions (the propose-don't-do mutation model,
-- consumed from P2) — and seeds the smith.model setting.
-- Conventions: unix-seconds INTEGER timestamps, 0/1 booleans, JSON as TEXT.

CREATE TABLE IF NOT EXISTS smith_conversations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    title      TEXT    NOT NULL DEFAULT '',
    tier       TEXT    NOT NULL DEFAULT 'deterministic', -- deterministic | reasoning
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS smith_messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    conversation_id INTEGER NOT NULL,
    kind            TEXT    NOT NULL,  -- user | smith_deterministic | smith_reasoning | action | runbook | notice
    content         TEXT    NOT NULL DEFAULT '',
    evidence        TEXT,              -- JSON; nullable
    created_at      INTEGER NOT NULL,
    FOREIGN KEY (conversation_id) REFERENCES smith_conversations(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_smith_messages_conversation
    ON smith_messages(conversation_id, id);

CREATE TABLE IF NOT EXISTS smith_investigations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger         TEXT    NOT NULL,  -- manual | anomaly:<code> | check:<check_id>
    status          TEXT    NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','in_progress','resolved','dismissed')),
    opened_at       INTEGER NOT NULL,
    closed_at       INTEGER,           -- NULL while open/in_progress
    summary         TEXT    NOT NULL DEFAULT '',
    conversation_id INTEGER,           -- linked Tier 2 conversation (P3); NULL until then
    FOREIGN KEY (conversation_id) REFERENCES smith_conversations(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_smith_investigations_status
    ON smith_investigations(status, opened_at);

CREATE TABLE IF NOT EXISTS smith_findings (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    investigation_id INTEGER,          -- NULL for standalone sweep findings
    check_id         TEXT    NOT NULL,
    severity         TEXT    NOT NULL
                     CHECK (severity IN ('ok','info','warn','crit')),
    summary          TEXT    NOT NULL,
    evidence         TEXT    NOT NULL DEFAULT '{}',  -- JSON
    sweep_kind       TEXT    NOT NULL
                     CHECK (sweep_kind IN ('manual','scheduled','anomaly')),
    created_at       INTEGER NOT NULL,
    FOREIGN KEY (investigation_id) REFERENCES smith_investigations(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_smith_findings_created ON smith_findings(created_at);
CREATE INDEX IF NOT EXISTS idx_smith_findings_check ON smith_findings(check_id, created_at);
CREATE INDEX IF NOT EXISTS idx_smith_findings_investigation
    ON smith_findings(investigation_id);

CREATE TABLE IF NOT EXISTS smith_actions (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    investigation_id INTEGER,
    conversation_id  INTEGER,
    kind             TEXT    NOT NULL,  -- load_config | unload_slot | restart_foundry_unit |
                                        -- run_script | catalog_change | settings_change |
                                        -- delete_files | runbook
    title            TEXT    NOT NULL,
    detail           TEXT    NOT NULL DEFAULT '{}',  -- JSON
    risk             TEXT    NOT NULL CHECK (risk IN ('info','low','high')),
    status           TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','approved','rejected','executing',
                                       'done','failed','superseded')),
    self_evicting    INTEGER NOT NULL DEFAULT 0,     -- bool: evicts smith's own slot
    handoff          TEXT,                           -- JSON handoff state; NULL when n/a
    created_by       TEXT    NOT NULL,               -- smith | <username>
    approved_by      TEXT,
    audit_ref        TEXT,
    created_at       INTEGER NOT NULL,
    resolved_at      INTEGER,
    FOREIGN KEY (investigation_id) REFERENCES smith_investigations(id) ON DELETE SET NULL,
    FOREIGN KEY (conversation_id) REFERENCES smith_conversations(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_smith_actions_status ON smith_actions(status, created_at);

-- smith.model seed (docs/v5-smith.md §4.3, decision-log item 2): the Tier 2
-- brain is SETTING-DRIVEN ONLY — seeded once here with the operator's initial
-- pick, freely changeable afterwards, no hardcoded default anywhere in code.
-- Empty/unresolvable ⇒ deterministic-only mode. The INSERT OR IGNORE never
-- overwrites an operator's later choice on re-migration.
INSERT OR IGNORE INTO settings (key, value, updated_at)
    VALUES ('smith.model', '"qwen36-mtp"', strftime('%s', 'now'));

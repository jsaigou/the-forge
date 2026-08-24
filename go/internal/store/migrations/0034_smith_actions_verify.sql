-- SPDX-License-Identifier: Apache-2.0
-- Schema v34 (smith P2 — action model + handoff, docs/v5-smith.md §4.6/§4.5).
--
-- Rebuilds smith_actions (copy-then-rename — SQLite cannot ALTER a CHECK
-- constraint, and the P2 execution/verify model needs a wider `status` enum
-- than 0033 shipped: `done_unverified`, the "claimed it deployed but didn't"
-- lesson made structural). Safe to rebuild wholesale: smith_actions has zero
-- Go references anywhere in the codebase as of this migration (the table
-- has never been written to), so it is provably empty on every real
-- deployment — no data-preserving translation logic is needed, just the
-- standard rebuild-and-swap (same pattern as 0011_config_status_rename.sql).
--
-- New columns beyond 0033: finding_id (links a proposal back to the
-- persisted finding that generated it — smith_findings, not smith_actions,
-- is what Diagnostics actually renders, so this is the real linkage),
-- dedupe_key (auto-propose identity, for reuse/supersede across sweeps),
-- result (the execution OUTCOME — kept separate from `detail`, which is the
-- mutation REQUEST; merging them would make the audit trail ambiguous),
-- executed_at + verified_at (the execute → post-verify timeline, alongside
-- the existing created_at/resolved_at).
CREATE TABLE smith_actions_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    investigation_id INTEGER,
    conversation_id  INTEGER,
    finding_id       INTEGER,
    kind             TEXT    NOT NULL,  -- load_config | unload_slot | restart_foundry_unit |
                                         -- settings_change | runbook
    title            TEXT    NOT NULL,
    detail           TEXT    NOT NULL DEFAULT '{}',  -- JSON; the request
    risk             TEXT    NOT NULL CHECK (risk IN ('info','low','high')),
    status           TEXT    NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','approved','rejected','executing',
                                       'done','done_unverified','failed','superseded')),
    self_evicting    INTEGER NOT NULL DEFAULT 0,     -- bool: evicts smith's own slot
    handoff          TEXT,                           -- JSON handoff state; NULL when n/a
    dedupe_key       TEXT,                           -- auto-propose identity; NULL for manual
    result           TEXT,                           -- JSON outcome; the response
    created_by       TEXT    NOT NULL,               -- smith | <username>
    approved_by      TEXT,
    audit_ref        TEXT,
    created_at       INTEGER NOT NULL,
    executed_at      INTEGER,
    verified_at      INTEGER,
    resolved_at      INTEGER,
    FOREIGN KEY (investigation_id) REFERENCES smith_investigations(id) ON DELETE SET NULL,
    FOREIGN KEY (conversation_id)  REFERENCES smith_conversations(id)  ON DELETE SET NULL,
    FOREIGN KEY (finding_id)       REFERENCES smith_findings(id)       ON DELETE SET NULL
);

INSERT INTO smith_actions_new
    (id, investigation_id, conversation_id, kind, title, detail, risk, status,
     self_evicting, handoff, created_by, approved_by, audit_ref, created_at, resolved_at)
SELECT id, investigation_id, conversation_id, kind, title, detail, risk, status,
       self_evicting, handoff, created_by, approved_by, audit_ref, created_at, resolved_at
FROM smith_actions;

DROP TABLE smith_actions;
ALTER TABLE smith_actions_new RENAME TO smith_actions;

CREATE INDEX IF NOT EXISTS idx_smith_actions_status  ON smith_actions(status, created_at);
CREATE INDEX IF NOT EXISTS idx_smith_actions_dedupe  ON smith_actions(dedupe_key, status);
CREATE INDEX IF NOT EXISTS idx_smith_actions_finding ON smith_actions(finding_id);

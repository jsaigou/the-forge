-- SPDX-License-Identifier: Apache-2.0
-- Schema v56 (smith autonomous-remediation Sprint 2 — procedure engine,
-- docs/v5-smith.md §13). One smith_procedure_runs row per KindProcedure
-- smith_action (1:1, enforced by the unique index below) — the durable
-- record a daemon restart resumes from, and the per-step journal Sprint 4's
-- supervision/evaluation harness reads.
--
-- status mirrors the runner's own state machine (procedure.go):
--   running             -- actively executing (or resumable after a crash —
--                          heartbeat_at, not wall-clock age, decides staleness)
--   awaiting_checkpoint -- paused for an operator decision (Step.Checkpoint);
--                          never reaped by age, only by an explicit
--                          approve/abort call
--   completed / failed / aborted -- terminal; smith_actions.status finalizes
--                          alongside these via the normal finalizeResult path
--
-- lease_id is the internal/maintenance.Gate lease this run holds, when its
-- procedure's Impact.NeedsMaintenance is true — NULL otherwise. On boot,
-- main.go checks for a live (running/awaiting_checkpoint) run holding the
-- currently-active maintenance lease before force-exiting an orphaned
-- window, replacing Sprint 1's blanket ReconcileOnBoot force-exit for
-- procedure-held windows specifically (Sprint 1's gate.go doc comment flags
-- this exact follow-up).
--
-- steps_result is a JSON array of per-step outcomes (title, argv, exit
-- code, duration, redacted stdout/stderr tail, verify results) — appended
-- to as each step finishes. checkpoint_note is the operator-facing summary
-- shown when status = awaiting_checkpoint.
CREATE TABLE IF NOT EXISTS smith_procedure_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id       INTEGER NOT NULL,
    procedure_id    TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','awaiting_checkpoint','completed','failed','aborted')),
    current_step    INTEGER NOT NULL DEFAULT 0,
    lease_id        TEXT,
    steps_result    TEXT    NOT NULL DEFAULT '[]',  -- JSON array
    checkpoint_note TEXT,
    started_at      INTEGER NOT NULL,
    heartbeat_at    INTEGER NOT NULL,
    finished_at     INTEGER,
    FOREIGN KEY (action_id) REFERENCES smith_actions(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_smith_procedure_runs_action ON smith_procedure_runs(action_id);
CREATE INDEX IF NOT EXISTS idx_smith_procedure_runs_status ON smith_procedure_runs(status, heartbeat_at);

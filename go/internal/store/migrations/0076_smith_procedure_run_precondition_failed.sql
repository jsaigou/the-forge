-- SPDX-License-Identifier: Apache-2.0
-- Schema v76 (v0.5 feedback sprints, Sprint 2 smith hardening, 2026-08-27).
--
-- Rebuilds smith_procedure_runs (copy-then-rename — SQLite cannot ALTER a
-- CHECK constraint, same pattern as 0034_smith_actions_verify.sql) to widen
-- status's allowed set with 'precondition_failed'. Unlike 0034's case, this
-- table has real production data (build_refresh evaluation runs and
-- others) — every existing row is copied across unchanged, nothing is
-- reclassified.
--
-- Why: runProcedureSteps' precondition gate (Sprint 6, docs/v5-smith.md
-- §13) was persisting 'failed' for a precondition-not-met run, identical to
-- a real mid-run execution failure — the run never executed a single step,
-- so this is "not applicable to this host," not "attempted and broke."
-- Every run-history/scorecard reader (ListProcedureRuns, ProcedureScorecard,
-- Diagnostics' timeline) needs to tell these apart, per the amd/skills
-- rocm-doctor exit-code contract reviewed this session (distinct exit codes
-- for not-applicable / attempted-and-failed / user-declined, rather than
-- collapsing all three into one non-zero status).
CREATE TABLE smith_procedure_runs_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id       INTEGER NOT NULL,
    procedure_id    TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running','awaiting_checkpoint','completed','failed',
                                      'precondition_failed','aborted')),
    current_step    INTEGER NOT NULL DEFAULT 0,
    lease_id        TEXT,
    steps_result    TEXT    NOT NULL DEFAULT '[]',
    checkpoint_note TEXT,
    started_at      INTEGER NOT NULL,
    heartbeat_at    INTEGER NOT NULL,
    finished_at     INTEGER,
    FOREIGN KEY (action_id) REFERENCES smith_actions(id) ON DELETE CASCADE
);

INSERT INTO smith_procedure_runs_new
    (id, action_id, procedure_id, status, current_step, lease_id, steps_result,
     checkpoint_note, started_at, heartbeat_at, finished_at)
SELECT id, action_id, procedure_id, status, current_step, lease_id, steps_result,
       checkpoint_note, started_at, heartbeat_at, finished_at
FROM smith_procedure_runs;

DROP TABLE smith_procedure_runs;
ALTER TABLE smith_procedure_runs_new RENAME TO smith_procedure_runs;

CREATE UNIQUE INDEX IF NOT EXISTS idx_smith_procedure_runs_action ON smith_procedure_runs(action_id);
CREATE INDEX IF NOT EXISTS idx_smith_procedure_runs_status ON smith_procedure_runs(status, heartbeat_at);

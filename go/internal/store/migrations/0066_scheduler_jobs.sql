-- SPDX-License-Identifier: Apache-2.0
-- P3 scheduler jobs: cron-style forced model loads (forge/p3sched track).
-- One row per operator-defined job: at each fire time the daemon's jobs
-- runner calls sched.EnsureLoaded(config_name, slot?) with
-- requested_by="cron:<name>". Timestamps are REAL unix seconds here (the
-- runner stores sub-minute precision fire times); NULL last_run_at = never
-- fired, NULL next_run_at = not yet scheduled (recomputed on
-- create/update/start).
CREATE TABLE scheduler_jobs (
    id          INTEGER PRIMARY KEY,
    name        TEXT    UNIQUE NOT NULL,
    cron        TEXT    NOT NULL,
    config_name TEXT    NOT NULL,
    slot        TEXT,
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_run_at REAL,
    next_run_at REAL,
    created_by  TEXT,
    created_at  REAL    NOT NULL
);

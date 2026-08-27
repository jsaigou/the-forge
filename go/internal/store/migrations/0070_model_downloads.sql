-- Schema v70 — HF model acquisition: resilient download engine.
--
-- model_downloads is one HF acquisition job (a repo + revision, one or more
-- files — a sharded GGUF downloads as a single job so registration only
-- fires once every shard has verified). model_download_files is one file
-- within that job; a single-file model has exactly one row.
--
-- state lifecycle: pending_approval (smith proposed it, nobody approved
-- yet) -> queued -> running -> paused -> verifying -> registering -> done,
-- with failed/cancelled reachable from any non-terminal state. paused is
-- also where a running job lands after a daemon restart (boot reconcile —
-- never resumed automatically without the operator's next resume click).
--
-- config_name: '' (default) means "auto-register a brand-new Model/
-- Variant/Artifact/Config on completion"; non-empty repoints an existing
-- Config's weight artifact instead (fetch_model's old single-file
-- behavior, preserved as an option).
CREATE TABLE IF NOT EXISTS model_downloads (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    repo            TEXT    NOT NULL,
    revision        TEXT    NOT NULL DEFAULT 'main',
    dest_dir        TEXT    NOT NULL DEFAULT '',  -- relative to Paths.ModelsDir; '' = repo-derived
    config_name     TEXT    NOT NULL DEFAULT '',
    state           TEXT    NOT NULL DEFAULT 'pending_approval'
                    CHECK (state IN ('pending_approval','queued','running','paused',
                                      'verifying','registering','done','failed','cancelled')),
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    bytes_total     INTEGER NOT NULL DEFAULT 0,
    error           TEXT    NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    proposed_by     TEXT    NOT NULL DEFAULT '',  -- '' = started directly by an operator, not smith
    created_config_id INTEGER,                    -- set once registration succeeds
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_model_downloads_state ON model_downloads(state);

CREATE TABLE IF NOT EXISTS model_download_files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    download_id     INTEGER NOT NULL REFERENCES model_downloads(id) ON DELETE CASCADE,
    filename        TEXT    NOT NULL,             -- path within the HF repo tree
    dest_rel_path   TEXT    NOT NULL,              -- relative to Paths.ModelsDir
    sha256_expected TEXT    NOT NULL DEFAULT '',
    sha256_actual   TEXT    NOT NULL DEFAULT '',
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    bytes_total     INTEGER NOT NULL DEFAULT 0,
    state           TEXT    NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending','running','paused','verified','failed')),
    sort_order      INTEGER NOT NULL DEFAULT 0     -- shard order (00001-of-00003 first)
);
CREATE INDEX IF NOT EXISTS idx_model_download_files_download ON model_download_files(download_id);

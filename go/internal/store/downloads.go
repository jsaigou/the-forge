// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// downloads.go — HF model-acquisition job persistence (migration 0070).
// internal/hfdownload owns the state machine; this file is pure storage.

// ModelDownloadRow is one HF acquisition job (a repo + revision, one or
// more files — see ModelDownloadFileRow for the per-file rows).
type ModelDownloadRow struct {
	ID              int64
	Repo            string
	Revision        string // "" on read never happens — defaults to "main" on create
	DestDir         string // relative to Paths.ModelsDir; "" = repo-derived
	ConfigName      string // "" = auto-register a new Model/Variant/Artifact/Config
	State           string
	BytesDone       int64
	BytesTotal      int64
	Error           string
	Attempts        int
	ProposedBy      string // "" = started directly by an operator, not smith
	CreatedConfigID int64  // 0 until registration succeeds
	CreatedAt       time.Time
	UpdatedAt       time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
}

// ModelDownloadFileRow is one file within a download job — more than one
// row means a sharded GGUF.
type ModelDownloadFileRow struct {
	ID             int64
	DownloadID     int64
	Filename       string // path within the HF repo tree
	DestRelPath    string // relative to Paths.ModelsDir
	SHA256Expected string
	SHA256Actual   string
	BytesDone      int64
	BytesTotal     int64
	State          string
	SortOrder      int
}

// ModelDownloads persists HF acquisition job state.
type ModelDownloads interface {
	// Create inserts the job row plus every file row in one transaction —
	// a job with zero files is a programming error the caller must not
	// reach.
	Create(ctx context.Context, d ModelDownloadRow, files []ModelDownloadFileRow) (int64, error)
	Get(ctx context.Context, id int64) (ModelDownloadRow, error)
	List(ctx context.Context) ([]ModelDownloadRow, error)
	// ListRunning returns every job whose state is "running" — the boot
	// reconcile query (a daemon restart must flip these to "paused"
	// rather than leave a job claiming to be running when nothing is
	// fetching it).
	ListRunning(ctx context.Context) ([]ModelDownloadRow, error)
	UpdateState(ctx context.Context, id int64, state, errMsg string) error
	UpdateProgress(ctx context.Context, id int64, bytesDone, bytesTotal int64) error
	IncrementAttempts(ctx context.Context, id int64) error
	SetCreatedConfig(ctx context.Context, id int64, configID int64) error
	Delete(ctx context.Context, id int64) error

	ListFiles(ctx context.Context, downloadID int64) ([]ModelDownloadFileRow, error)
	UpdateFileProgress(ctx context.Context, fileID int64, bytesDone, bytesTotal int64) error
	UpdateFileState(ctx context.Context, fileID int64, state, sha256Actual string) error
}

type modelDownloadsView struct{ d *DB }

func (d *DB) ModelDownloads() ModelDownloads { return modelDownloadsView{d} }

func (v modelDownloadsView) Create(ctx context.Context, d ModelDownloadRow, files []ModelDownloadFileRow) (int64, error) {
	if d.Revision == "" {
		d.Revision = "main"
	}
	if d.State == "" {
		d.State = "pending_approval"
	}
	now := unixOf(orNow(d.CreatedAt))
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: model_downloads.create: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO model_downloads (repo, revision, dest_dir, config_name, state,
		   bytes_done, bytes_total, error, attempts, proposed_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Repo, d.Revision, d.DestDir, d.ConfigName, d.State,
		d.BytesDone, d.BytesTotal, d.Error, d.Attempts, d.ProposedBy, now, now)
	if err != nil {
		return 0, fmt.Errorf("store: model_downloads.create: %w", err)
	}
	id, _ := res.LastInsertId()

	for _, f := range files {
		if f.State == "" {
			f.State = "pending"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_download_files (download_id, filename, dest_rel_path,
			   sha256_expected, sha256_actual, bytes_done, bytes_total, state, sort_order)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, f.Filename, f.DestRelPath, f.SHA256Expected, f.SHA256Actual,
			f.BytesDone, f.BytesTotal, f.State, f.SortOrder); err != nil {
			return 0, fmt.Errorf("store: model_downloads.create: file %q: %w", f.Filename, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: model_downloads.create: commit: %w", err)
	}
	return id, nil
}

const modelDownloadCols = `id, repo, revision, dest_dir, config_name, state,
	bytes_done, bytes_total, error, attempts, proposed_by, created_config_id,
	created_at, updated_at, started_at, finished_at`

func scanModelDownload(row interface{ Scan(...any) error }) (ModelDownloadRow, error) {
	var d ModelDownloadRow
	var createdConfigID sql.NullInt64
	var startedAt, finishedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&d.ID, &d.Repo, &d.Revision, &d.DestDir, &d.ConfigName, &d.State,
		&d.BytesDone, &d.BytesTotal, &d.Error, &d.Attempts, &d.ProposedBy, &createdConfigID,
		&createdAt, &updatedAt, &startedAt, &finishedAt); err != nil {
		return ModelDownloadRow{}, err
	}
	d.CreatedConfigID = intOf(createdConfigID)
	d.CreatedAt = time.Unix(createdAt, 0).UTC()
	d.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	d.StartedAt = timeOf(startedAt)
	d.FinishedAt = timeOf(finishedAt)
	return d, nil
}

func (v modelDownloadsView) Get(ctx context.Context, id int64) (ModelDownloadRow, error) {
	row := v.d.sql.QueryRowContext(ctx, `SELECT `+modelDownloadCols+` FROM model_downloads WHERE id = ?`, id)
	d, err := scanModelDownload(row)
	if err == sql.ErrNoRows {
		return ModelDownloadRow{}, fmt.Errorf("%w: model_download %d", ErrNotFound, id)
	}
	if err != nil {
		return ModelDownloadRow{}, fmt.Errorf("store: model_downloads.get: %w", err)
	}
	return d, nil
}

func (v modelDownloadsView) listWhere(ctx context.Context, where string, args ...any) ([]ModelDownloadRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT `+modelDownloadCols+` FROM model_downloads `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: model_downloads.list: %w", err)
	}
	defer rows.Close()
	out := []ModelDownloadRow{}
	for rows.Next() {
		d, err := scanModelDownload(rows)
		if err != nil {
			return nil, fmt.Errorf("store: model_downloads.list: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (v modelDownloadsView) List(ctx context.Context) ([]ModelDownloadRow, error) {
	return v.listWhere(ctx, "")
}

func (v modelDownloadsView) ListRunning(ctx context.Context) ([]ModelDownloadRow, error) {
	return v.listWhere(ctx, "WHERE state = ?", "running")
}

func (v modelDownloadsView) UpdateState(ctx context.Context, id int64, state, errMsg string) error {
	now := unixOf(time.Now())
	var finishedAt any
	switch state {
	case "running":
		// Only stamp started_at the first time a job starts running —
		// resuming a paused job must not reset "how long has this taken".
		if _, err := v.d.sql.ExecContext(ctx,
			`UPDATE model_downloads SET started_at = COALESCE(started_at, ?) WHERE id = ?`, now, id); err != nil {
			return fmt.Errorf("store: model_downloads.update_state: stamp started_at: %w", err)
		}
	case "done", "failed", "cancelled":
		finishedAt = now
	}
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_downloads SET state = ?, error = ?, updated_at = ?, finished_at = COALESCE(finished_at, ?) WHERE id = ?`,
		state, errMsg, now, finishedAt, id)
	if err != nil {
		return fmt.Errorf("store: model_downloads.update_state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model_download %d", ErrNotFound, id)
	}
	return nil
}

func (v modelDownloadsView) UpdateProgress(ctx context.Context, id int64, bytesDone, bytesTotal int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_downloads SET bytes_done = ?, bytes_total = ?, updated_at = ? WHERE id = ?`,
		bytesDone, bytesTotal, unixOf(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: model_downloads.update_progress: %w", err)
	}
	return nil
}

func (v modelDownloadsView) IncrementAttempts(ctx context.Context, id int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_downloads SET attempts = attempts + 1, updated_at = ? WHERE id = ?`, unixOf(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: model_downloads.increment_attempts: %w", err)
	}
	return nil
}

func (v modelDownloadsView) SetCreatedConfig(ctx context.Context, id int64, configID int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_downloads SET created_config_id = ?, updated_at = ? WHERE id = ?`,
		configID, unixOf(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: model_downloads.set_created_config: %w", err)
	}
	return nil
}

func (v modelDownloadsView) Delete(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM model_downloads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: model_downloads.delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model_download %d", ErrNotFound, id)
	}
	return nil
}

func (v modelDownloadsView) ListFiles(ctx context.Context, downloadID int64) ([]ModelDownloadFileRow, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, download_id, filename, dest_rel_path, sha256_expected, sha256_actual,
		   bytes_done, bytes_total, state, sort_order
		 FROM model_download_files WHERE download_id = ? ORDER BY sort_order, id`, downloadID)
	if err != nil {
		return nil, fmt.Errorf("store: model_downloads.list_files: %w", err)
	}
	defer rows.Close()
	out := []ModelDownloadFileRow{}
	for rows.Next() {
		var f ModelDownloadFileRow
		if err := rows.Scan(&f.ID, &f.DownloadID, &f.Filename, &f.DestRelPath,
			&f.SHA256Expected, &f.SHA256Actual, &f.BytesDone, &f.BytesTotal, &f.State, &f.SortOrder); err != nil {
			return nil, fmt.Errorf("store: model_downloads.list_files: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (v modelDownloadsView) UpdateFileProgress(ctx context.Context, fileID int64, bytesDone, bytesTotal int64) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_download_files SET bytes_done = ?, bytes_total = ? WHERE id = ?`,
		bytesDone, bytesTotal, fileID)
	if err != nil {
		return fmt.Errorf("store: model_downloads.update_file_progress: %w", err)
	}
	return nil
}

func (v modelDownloadsView) UpdateFileState(ctx context.Context, fileID int64, state, sha256Actual string) error {
	_, err := v.d.sql.ExecContext(ctx,
		`UPDATE model_download_files SET state = ?, sha256_actual = ? WHERE id = ?`,
		state, sha256Actual, fileID)
	if err != nil {
		return fmt.Errorf("store: model_downloads.update_file_state: %w", err)
	}
	return nil
}

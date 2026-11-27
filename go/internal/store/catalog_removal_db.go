// SPDX-License-Identifier: Apache-2.0

package store

// The database side of catalog_removal.go: reading what refers to a Config, and deleting the rows of one removal in a single transaction.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RemovalReferences implements Catalog.
func (v catalogView) RemovalReferences(ctx context.Context) (RemovalReferences, error) {
	refs := RemovalReferences{Settings: map[string]string{}}
	fail := func(what string, err error) (RemovalReferences, error) {
		return refs, fmt.Errorf("store: catalog.removal_references: %s: %w", what, err)
	}
	each := func(query string, scan func(*sql.Rows) error) error {
		rows, err := v.d.sql.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := each(`SELECT key, value FROM settings`, func(r *sql.Rows) error {
		var k string
		var val []byte
		if err := r.Scan(&k, &val); err != nil {
			return err
		}
		refs.Settings[k] = string(val)
		return nil
	}); err != nil {
		return fail("settings", err)
	}
	if err := each(`SELECT name, kind, params FROM specialist_occupants ORDER BY name`, func(r *sql.Rows) error {
		var e PoolEngineRef
		var params string
		if err := r.Scan(&e.Name, &e.Kind, &params); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(params), &e.Params); err != nil {
			return fmt.Errorf("pool engine %q params: %w", e.Name, err)
		}
		refs.PoolEngines = append(refs.PoolEngines, e)
		return nil
	}); err != nil {
		return fail("pool engines", err)
	}
	named := func(dst *[]NamedConfigRef) func(*sql.Rows) error {
		return func(r *sql.Rows) error {
			var n NamedConfigRef
			if err := r.Scan(&n.Name, &n.Config); err != nil {
				return err
			}
			*dst = append(*dst, n)
			return nil
		}
	}
	if err := each(`SELECT name, config_name FROM scheduler_jobs ORDER BY name`, named(&refs.Jobs)); err != nil {
		return fail("scheduler jobs", err)
	}
	if err := each(`SELECT requested_by, model FROM sched_queue WHERE status IN ('queued', 'loading') ORDER BY enqueued_at`, named(&refs.Queued)); err != nil {
		return fail("load queue", err)
	}
	if err := each(`SELECT label, model, end_ts FROM reservations ORDER BY label`, func(r *sql.Rows) error {
		var res ReservationRef
		var end int64
		if err := r.Scan(&res.Label, &res.Config, &end); err != nil {
			return err
		}
		res.End = time.Unix(end, 0)
		refs.Reservations = append(refs.Reservations, res)
		return nil
	}); err != nil {
		return fail("reservations", err)
	}
	aliases, err := v.ListModelAliases(ctx)
	if err != nil {
		return fail("model aliases", err)
	}
	refs.Aliases = aliases
	subject := func(dst *[]SubjectRef) func(*sql.Rows) error {
		return func(r *sql.Rows) error {
			var s SubjectRef
			if err := r.Scan(&s.Type, &s.ID); err != nil {
				return err
			}
			*dst = append(*dst, s)
			return nil
		}
	}
	if err := each(`SELECT subject_type, subject_id FROM benchmarks`, subject(&refs.Benchmarks)); err != nil {
		return fail("benchmarks", err)
	}
	if err := each(`SELECT subject_type, subject_id FROM notes`, subject(&refs.Notes)); err != nil {
		return fail("notes", err)
	}
	return refs, nil
}

// RemoveCatalogRows implements Catalog. Configs go first (their variant and weight references are ON DELETE RESTRICT); deleting a model cascades
// to its variants, artifacts and offerings; benchmarks, notes and favorites have no foreign key and are deleted by subject.
func (v catalogView) RemoveCatalogRows(ctx context.Context, rows RemovalRows) error {
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: catalog.remove: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	exec := func(what, query string, args ...any) error {
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("store: catalog.remove: %s: %w", what, err)
		}
		return nil
	}
	for _, s := range rows.Subjects {
		if err := exec("benchmarks", `DELETE FROM benchmarks WHERE subject_type = ? AND subject_id = ?`, s.Type, s.ID); err != nil {
			return err
		}
		// notes only knows model, config and offering subjects; a variant subject matches nothing.
		if err := exec("notes", `DELETE FROM notes WHERE subject_type = ? AND subject_id = ?`, s.Type, s.ID); err != nil {
			return err
		}
	}
	for _, id := range rows.ConfigIDs {
		if err := exec("favorites", `DELETE FROM favorites WHERE subject_type = 'config' AND subject_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM configs WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("store: catalog.remove: config %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return fmt.Errorf("%w: config %d", ErrNotFound, id)
		}
	}
	for _, id := range rows.ArtifactIDs {
		if err := exec("artifact", `DELETE FROM artifacts WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if rows.ModelID != 0 {
		res, err := tx.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, rows.ModelID)
		if err != nil {
			return fmt.Errorf("store: catalog.remove: model %d: %w", rows.ModelID, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return fmt.Errorf("%w: model %d", ErrNotFound, rows.ModelID)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: catalog.remove: commit: %w", err)
	}
	return nil
}

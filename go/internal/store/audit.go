// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type auditView struct{ d *DB }

// Audit returns the audit-log writer. The DB row is primary; when
// WithAuditMirror is set, each entry is also appended as one JSON line
// (V4 audit.py format lineage) — mirror failures never fail the DB write.
// AuditEntry.Detail must never contain secrets (caller's contract, enforced
// by convention as in V4's SensitiveFilter).
func (d *DB) Audit() Audit { return auditView{d} }

func (v auditView) Write(ctx context.Context, e AuditEntry) error {
	e.TS = orNow(e.TS)
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail, remote_addr)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		unixOf(e.TS), e.Actor, e.Action, nullStr(e.Target), nullStr(e.Detail),
		nullStr(e.RemoteAddr))
	if err != nil {
		return fmt.Errorf("store: audit.write: %w", err)
	}
	v.mirror(e)
	return nil
}

// List implements Audit.List — see that method's doc comment for the
// actionPrefix+target contract. Read-only, best-effort caps: an empty
// actionPrefix matches every action (LIKE '%').
func (v auditView) List(ctx context.Context, actionPrefix, target string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, ts, actor, action, target, detail, remote_addr FROM audit_log
		 WHERE action LIKE ? AND (? = '' OR target = ?)
		 ORDER BY ts DESC, id DESC LIMIT ?`,
		actionPrefix+"%", target, target, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit.list: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var target, detail, remoteAddr sql.NullString
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &target, &detail, &remoteAddr); err != nil {
			return nil, fmt.Errorf("store: audit.list: scan: %w", err)
		}
		e.TS = time.Unix(ts, 0).UTC()
		e.Target = strOf(target)
		e.Detail = strOf(detail)
		e.RemoteAddr = strOf(remoteAddr)
		out = append(out, e)
	}
	return out, rows.Err()
}

// mirror appends the entry to the optional JSONL file. Best-effort by
// design: the DB committed already, and audit writes must never take the
// request path down over a full disk.
func (v auditView) mirror(e AuditEntry) {
	if v.d.auditMirror == "" {
		return
	}
	line, err := json.Marshal(map[string]string{
		"ts":          e.TS.UTC().Format(time.RFC3339),
		"actor":       e.Actor,
		"action":      e.Action,
		"target":      e.Target,
		"detail":      e.Detail,
		"remote_addr": e.RemoteAddr,
	})
	if err != nil {
		return
	}
	v.d.auditMu.Lock()
	defer v.d.auditMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(v.d.auditMirror), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(v.d.auditMirror, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

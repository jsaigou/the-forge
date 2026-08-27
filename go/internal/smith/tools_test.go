// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── structural guarantee: ToolEnv's shape is frozen ─────────────────────

// TestToolEnv_ShapeFrozen locks ToolEnv's exact field set. Adding a
// capability (especially anything that could mutate) must be a conscious
// edit to this test, not a drive-by widening — the whole point of routing
// every tool's Run through *ToolEnv instead of *Smith (tools.go's doc
// comment).
func TestToolEnv_ShapeFrozen(t *testing.T) {
	want := []string{"RunSelected", "KBSearch", "ListFindings", "Catalog", "Web", "HF", "HFDownload", "Now"}
	got := fieldNames(reflect.TypeOf(ToolEnv{}))
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToolEnv fields = %v, want exactly %v (if this is intentional, update the freeze)", got, want)
	}
}

func fieldNames(t reflect.Type) []string {
	out := make([]string, t.NumField())
	for i := range out {
		out[i] = t.Field(i).Name
	}
	return out
}

// ── tool registry schema sanity ──────────────────────────────────────────

func TestToolRegistry_SchemasValid(t *testing.T) {
	seen := map[string]bool{}
	idPattern := "abcdefghijklmnopqrstuvwxyz0123456789_"
	for _, tool := range toolRegistry {
		if tool.ID == "" {
			t.Fatalf("tool with empty ID: %+v", tool)
		}
		if seen[tool.ID] {
			t.Errorf("duplicate tool ID %q", tool.ID)
		}
		seen[tool.ID] = true
		if tool.ID[0] < 'a' || tool.ID[0] > 'z' {
			t.Errorf("tool ID %q must start with a lowercase letter", tool.ID)
		}
		for _, r := range tool.ID {
			if !contains(idPattern, r) {
				t.Errorf("tool ID %q has invalid character %q", tool.ID, r)
			}
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.ID)
		}
		if tool.Params["type"] != "object" {
			t.Errorf("tool %q params type = %v, want \"object\"", tool.ID, tool.Params["type"])
		}
		props, _ := tool.Params["properties"].(map[string]any)
		if required, ok := tool.Params["required"].([]string); ok {
			for _, r := range required {
				if _, ok := props[r]; !ok {
					t.Errorf("tool %q requires %q but it's not in properties", tool.ID, r)
				}
			}
		}
		if _, err := json.Marshal(tool.Params); err != nil {
			t.Errorf("tool %q params not JSON-marshalable: %v", tool.ID, err)
		}
		if tool.Run == nil {
			t.Errorf("tool %q has a nil Run", tool.ID)
		}
	}
}

func contains(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// ── the empirical no-write guarantee ─────────────────────────────────────

func TestTools_NoWriteAgainstRealStore(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	ctx := context.Background()

	// Seed one finding so list_findings has something real to read.
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})
	if _, err := s.persistFindings(ctx, []Finding{{
		CheckID: "gtt_ceiling", Severity: SeverityWarn, Summary: "test finding",
	}}, "manual", s.d.Now(), nil); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	tables := []string{
		"smith_findings", "smith_investigations", "smith_actions",
		"smith_conversations", "smith_messages", "smith_web_cache", "smith_binaries",
	}
	before := map[string]int{}
	for _, tbl := range tables {
		before[tbl] = countRows(t, db, tbl)
	}

	env := s.toolEnv(ctx)
	argsByTool := map[string]string{
		"run_check":        `{"check_ids":["gtt_ceiling"]}`,
		"list_findings":    `{}`,
		"kb_search":        `{"query":"gtt"}`,
		"catalog_lookup":   `{"kind":"configs"}`,
		"web_search":       `{"query":"test"}`,
		"web_fetch":        `{"url":"http://example.invalid"}`,
		"hf_search":        `{"query":"test"}`,
		"hf_preflight":     `{"repo":"org/model"}`,
		"download_status":  `{}`,
		// download_start is deliberately excluded from this loop — it's
		// the one tool that writes anything (see hfDownloadTool's doc
		// comment), and env.HFDownload is nil in this harness anyway
		// (unavailable, a no-op). TestDownloadStartToolOnlyProposesJob
		// below is its own dedicated test, against a REAL wired engine,
		// proving exactly what it writes and nothing more.
	}
	for _, tool := range toolRegistry {
		if tool.ID == "download_start" {
			continue
		}
		args, ok := argsByTool[tool.ID]
		if !ok {
			t.Fatalf("no test args registered for tool %q — add one", tool.ID)
		}
		if _, err := runTool(ctx, env, tool, json.RawMessage(args)); err != nil {
			// Some tools legitimately error in this harness (e.g. web_fetch
			// against a fake host with Web nil returns a result, not an
			// error, so this shouldn't fire — but tolerate it either way,
			// the row-count assertion below is what matters).
			t.Logf("tool %q returned an error (tolerated): %v", tool.ID, err)
		}
	}

	for _, tbl := range tables {
		after := countRows(t, db, tbl)
		if after != before[tbl] {
			t.Errorf("table %s: rows %d -> %d — a read-only tool must never write", tbl, before[tbl], after)
		}
	}
}

// TestDownloadStartToolOnlyProposesJob is download_start's dedicated proof
// (see TestTools_NoWriteAgainstRealStore's comment on why it's separate):
// against a REALLY wired hfdownload.Service, the tool must create exactly
// one model_downloads row in pending_approval, touch no smith_* table, and
// never start a worker (no state transition beyond the insert — proven by
// polling for a moment and confirming the state never leaves
// pending_approval, since a real Start would flip it to running).
func TestDownloadStartToolOnlyProposesJob(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	dl := hfdownload.New(hfdownload.Deps{
		Store: db, Cfg: func() *config.Config { return &config.Config{} }, Logf: t.Logf,
	})
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), HFDownload: dl})

	tables := []string{"smith_findings", "smith_actions", "smith_conversations"}
	before := map[string]int{}
	for _, tbl := range tables {
		before[tbl] = countRows(t, db, tbl)
	}
	downloadsBefore := countRows(t, db, "model_downloads")

	env := s.toolEnv(ctx)
	tool, ok := findTool("download_start")
	if !ok {
		t.Fatal("download_start not registered")
	}
	result, err := runTool(ctx, env, tool, json.RawMessage(`{"repo":"testorg/testmodel","filename":"model.gguf","size_bytes":1000000000}`))
	if err != nil {
		t.Fatalf("download_start: %v", err)
	}
	resMap, ok := result.(map[string]any)
	if !ok || resMap["status"] != "proposed" {
		t.Fatalf("download_start result = %+v, want status=proposed", result)
	}

	for _, tbl := range tables {
		if after := countRows(t, db, tbl); after != before[tbl] {
			t.Errorf("table %s: rows %d -> %d — download_start must never touch smith_* tables", tbl, before[tbl], after)
		}
	}
	if after := countRows(t, db, "model_downloads"); after != downloadsBefore+1 {
		t.Fatalf("model_downloads rows %d -> %d, want exactly +1", downloadsBefore, after)
	}

	jobID := resMap["job_id"].(int64)
	job, err := db.ModelDownloads().Get(ctx, jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != "pending_approval" {
		t.Errorf("job state = %q, want pending_approval — the tool must never start it", job.State)
	}
	time.Sleep(50 * time.Millisecond) // give a hypothetical stray goroutine time to misbehave
	job, err = db.ModelDownloads().Get(ctx, jobID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != "pending_approval" {
		t.Errorf("job state drifted to %q after the tool returned — nothing should be running", job.State)
	}
}

func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	row := db.SQL().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestRunSelected_DoesNotPersistOrProposeOrLock proves the specific
// RunChecks hazard runSelected exists to avoid: no findings/actions rows,
// and no s.sweeping lock taken (a concurrent RunChecks still succeeds).
func TestRunSelected_DoesNotPersistOrProposeOrLock(t *testing.T) {
	db := openDB(t)
	seedBrainCatalog(t, db)
	ctx := context.Background()
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog()})

	beforeFindings := countRows(t, db, "smith_findings")
	beforeActions := countRows(t, db, "smith_actions")

	env := s.toolEnv(ctx)
	findings, err := env.RunSelected(ctx, []string{"gtt_ceiling", "disk_space"})
	if err != nil {
		t.Fatalf("RunSelected: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2", len(findings))
	}

	if got := countRows(t, db, "smith_findings"); got != beforeFindings {
		t.Errorf("smith_findings rows changed: %d -> %d", beforeFindings, got)
	}
	if got := countRows(t, db, "smith_actions"); got != beforeActions {
		t.Errorf("smith_actions rows changed: %d -> %d", beforeActions, got)
	}

	// The real proof runSelected never takes s.sweeping: a concurrent
	// RunChecks must succeed, not return ErrAlreadyRunning.
	if _, err := s.RunChecks(ctx, ScopeQuick, nil, SweepManual); err != nil {
		t.Errorf("RunChecks after runSelected = %v, want nil (runSelected must not hold s.sweeping)", err)
	}
}

// ── nil-dep degrade table ────────────────────────────────────────────────

func TestTools_NilDepsDegradeCleanly(t *testing.T) {
	cases := []struct {
		tool string
		args string
	}{
		{"run_check", `{"check_ids":["gtt_ceiling"]}`},
		{"list_findings", `{}`},
		{"kb_search", `{"query":"x"}`},
		{"catalog_lookup", `{"kind":"configs"}`},
	}
	env := &ToolEnv{} // every field nil
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			tool, ok := findTool(tc.tool)
			if !ok {
				t.Fatalf("unknown tool %q", tc.tool)
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("tool %q panicked against nil deps: %v", tc.tool, r)
				}
			}()
			if _, err := runTool(context.Background(), env, tool, json.RawMessage(tc.args)); err == nil {
				t.Errorf("tool %q with nil deps should return an error, not a fake success", tc.tool)
			}
		})
	}
}

// web_search/web_fetch with nil Web degrade to a result, not an error (the
// model can still answer) — a deliberately different contract from the
// other four tools above, documented in webSearchTool/webFetchTool.
func TestTools_WebToolsNilWebDegradesToResult(t *testing.T) {
	env := &ToolEnv{}
	for _, id := range []string{"web_search", "web_fetch"} {
		tool, _ := findTool(id)
		args := `{"query":"x"}`
		if id == "web_fetch" {
			args = `{"url":"http://example.invalid"}`
		}
		result, err := runTool(context.Background(), env, tool, json.RawMessage(args))
		if err != nil {
			t.Errorf("%s: err = %v, want nil (degrade to a result)", id, err)
		}
		wtr, ok := result.(webToolResult)
		if !ok {
			t.Fatalf("%s: result type = %T, want webToolResult", id, result)
		}
		payload, _ := wtr.Payload.(map[string]any)
		if payload["unavailable"] == nil {
			t.Errorf("%s: payload = %+v, want an \"unavailable\" explanation", id, payload)
		}
	}
}

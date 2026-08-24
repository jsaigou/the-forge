// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestKBLookup(t *testing.T) {
	c, ok := New(Deps{}).KBLookup("pitfalls:gtt-ceiling")
	if !ok {
		t.Fatal("expected pitfalls:gtt-ceiling to resolve")
	}
	if c.Title == "" || c.Body == "" {
		t.Errorf("chunk missing title/body: %+v", c)
	}
	if _, ok := New(Deps{}).KBLookup("pitfalls:does-not-exist"); ok {
		t.Error("expected unknown ref to miss")
	}
}

// TestKBRefIntegrity is the P4 exit criterion mechanized (docs/v5-smith.md
// §9's P4 row: "findings carry accurate KBRefs"). It scans every
// checks*.go source file (checks.go plus any check split into its own
// checks_<module>.go file, e.g. P5's checks_blocked_recheck.go or P6's
// checks_tailscale.go/checks_comfyui.go/checks_binaries.go) for every
// `KBRefs: []string{...}` literal — checks only ever emit static string
// refs, never computed ones, so a source scan finds every possible ref
// without needing to fabricate trigger conditions for every check — and
// asserts each one resolves via KBLookup. A future check that ships a
// typo'd ref fails the build here, not silently at runtime. Widened from a
// single checks.go read (P4) to a glob (P6) after noticing the original
// regex silently missed any ref emitted from a checks_*.go split-out file.
func TestKBRefIntegrity(t *testing.T) {
	matches, err := filepath.Glob("checks*.go")
	if err != nil {
		t.Fatalf("glob checks*.go: %v", err)
	}
	var files []string
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		files = append(files, m)
	}
	if len(files) == 0 {
		t.Fatal("no checks*.go source files found — glob or working dir drifted")
	}

	refLitRe := regexp.MustCompile(`KBRefs:\s*\[\]string\{([^}]*)\}`)
	quotedRe := regexp.MustCompile(`"([^"]+)"`)

	found := map[string]string{} // ref -> file it was found in
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range refLitRe.FindAllStringSubmatch(string(raw), -1) {
			for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
				found[q[1]] = path
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no KBRefs literals found in any checks*.go file — regex or files drifted")
	}
	for ref, file := range found {
		if _, ok := New(Deps{}).KBLookup(ref); !ok {
			t.Errorf("%s emits KBRef %q but no chunk resolves it — fix kb/manifest.json or the ref itself", file, ref)
		}
	}
}

func TestKBSearch_Corpus(t *testing.T) {
	s := New(Deps{})
	results, err := s.KBSearch(context.Background(), "gtt ceiling", 5)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one match for 'gtt ceiling'")
	}
	if results[0].Ref != "pitfalls:gtt-ceiling" {
		t.Errorf("expected pitfalls:gtt-ceiling to rank first, got %q (score %.2f)", results[0].Ref, results[0].Score)
	}
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted by score descending at index %d", i)
		}
	}
}

// TestKBSearch_LongChunkDoesNotDrownOutRelevance is a regression test for a
// real ranking bug found live during P4 verification (docs/v5-smith.md §9's
// grounding-check exit criterion): asking smith's chat "If I set --parallel
// 2 on a mode with context=131072, what n_ctx will each request actually
// see, and why?" answered "not documented" even though
// modes:parallel-context-split-incident exists specifically to answer this
// — pitfalls:gtt-ceiling (the single longest chunk in the corpus, ~7.6 KB
// of accumulated incident history) out-scored it purely by repeating
// generic words ("context", "mode", "configured") far more times, which
// raw term-frequency counted at full linear weight. Fixed by capping
// per-token body term frequency (bodyTermCap in kb.go); this test pins the
// exact query that broke it.
func TestKBSearch_LongChunkDoesNotDrownOutRelevance(t *testing.T) {
	s := New(Deps{})
	q := "If I set --parallel 2 on a mode with context=131072, what n_ctx will each request actually see, and why?"
	results, err := s.KBSearch(context.Background(), q, 3)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one match")
	}
	if results[0].Ref != "modes:parallel-context-split-incident" {
		t.Errorf("top result = %q (score %.2f), want modes:parallel-context-split-incident — a long, "+
			"unrelated-but-verbose chunk may be drowning out the topically correct one again",
			results[0].Ref, results[0].Score)
	}
}

func TestKBSearch_NoMatch(t *testing.T) {
	s := New(Deps{})
	results, err := s.KBSearch(context.Background(), "zzznonexistentqueryterm", 5)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no matches, got %d", len(results))
	}
}

func TestKBSearch_LimitClamped(t *testing.T) {
	s := New(Deps{})
	// A broad query should hit many corpus chunks; limit=1 must actually cap it.
	results, err := s.KBSearch(context.Background(), "model context memory backend", 1)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}
	if len(results) > 1 {
		t.Errorf("expected at most 1 result, got %d", len(results))
	}
}

// TestKBSearch_NilStoreDegrades confirms the standing nil-tolerance
// convention: no Store wired ⇒ DB evidence sources contribute nothing and
// KBSearch still succeeds (never an error) with corpus-only results.
func TestKBSearch_NilStoreDegrades(t *testing.T) {
	s := New(Deps{Store: nil})
	results, err := s.KBSearch(context.Background(), "gtt", 5)
	if err != nil {
		t.Fatalf("KBSearch with nil Store returned error: %v", err)
	}
	for _, r := range results {
		if r.Kind != "doc" {
			t.Errorf("expected only doc-kind results with nil Store, got kind %q", r.Kind)
		}
	}
}

// TestKBSearch_DBEvidenceSources seeds real rows into each of the five
// live-DB evidence tables and confirms KBSearch surfaces them, with
// secrets redacted out of audit_log's JSON detail field.
func TestKBSearch_DBEvidenceSources(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := db.Notifications().Upsert(ctx, "ZZQUERYTOKEN_NOTIF", "warn", "a3",
		"hit ZZQUERYTOKEN_NOTIF during a sweep", "ZZQUERYTOKEN_NOTIF:a3", time.Now()); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO mode_history (mode, ts, configured_ctx, actual_ctx, load_time_s, result) VALUES (?, ?, ?, ?, ?, ?)`,
		"zzquerytoken-mode", 1000, 8192, 8192, 12.5, "ok"); err != nil {
		t.Fatalf("seed mode_history: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail, remote_addr) VALUES (?, ?, ?, ?, ?, ?)`,
		1000, "operator", "zzquerytoken_action", "some-target", `{"api_key":"sk-forge-should-not-leak","note":"zzquerytoken detail"}`, "127.0.0.1"); err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}
	// model_profiles is FK'd to configs.id since the 0042 surrogate-key
	// migration — seed a minimal configs row (foreign_keys off just for
	// this insert, same pattern as internal/profile's tests) since a full
	// families/models/variants/artifacts/engines chain isn't otherwise
	// needed here.
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("pragma off: %v", err)
	}
	configRes, err := db.SQL().ExecContext(ctx,
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id) VALUES ('zzquerytoken-profile', 1, 1, 1)`)
	if err != nil {
		t.Fatalf("seed configs: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("pragma on: %v", err)
	}
	configID, err := configRes.LastInsertId()
	if err != nil {
		t.Fatalf("seed configs: last insert id: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO model_profiles (config_id, model_id, n_ctx, backend, parallel, safe_memory_bytes, prefill_tps, decode_tps, actual_n_ctx, fingerprint, measured_at)
		 VALUES (?, '', ?, 'vulkan', 1, ?, ?, ?, ?, 'fp', ?)`,
		configID, 8192, int64(4096)<<20, 100.0, 50.0, 8192, 1000); err != nil {
		t.Fatalf("seed model_profiles: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO smith_findings (check_id, severity, summary, evidence, sweep_kind, created_at) VALUES (?, ?, ?, ?, 'manual', ?)`,
		"zzquerytoken_check", "warn", "zzquerytoken finding summary", "{}", 1000); err != nil {
		t.Fatalf("seed smith_findings: %v", err)
	}

	s := New(Deps{Store: db})
	results, err := s.KBSearch(ctx, "zzquerytoken", 20)
	if err != nil {
		t.Fatalf("KBSearch: %v", err)
	}

	seenKinds := map[string]bool{}
	var auditBody string
	for _, r := range results {
		seenKinds[r.Kind] = true
		if r.Kind == "audit" {
			auditBody = r.Body
		}
		if r.TS == nil && r.Kind != "doc" {
			t.Errorf("expected a non-nil TS on a %s-kind DB result", r.Kind)
		}
	}
	for _, wantKind := range []string{"notification", "mode_history", "audit", "profile", "finding"} {
		if !seenKinds[wantKind] {
			t.Errorf("expected a %q result among %d results, got kinds %v", wantKind, len(results), seenKinds)
		}
	}
	if strings.Contains(auditBody, "sk-forge-should-not-leak") {
		t.Errorf("audit_log result leaked the raw api_key value: %q", auditBody)
	}
}

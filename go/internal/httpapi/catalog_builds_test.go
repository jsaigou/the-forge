// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

type buildsFixture struct {
	s       *Server
	db      *store.DB
	pre     catalogPrereqs
	root    string
	engine  int64
	probes  int
	loadVal bool
}

func newBuildsFixture(t *testing.T) *buildsFixture {
	t.Helper()
	s, db := newCatalogTestServer(t)
	f := &buildsFixture{s: s, db: db, pre: seedCatalogPrereqs(t, db), root: t.TempDir()}
	f.engine = f.pre.engineID
	s.deps.Settings = db.Settings()
	s.deps.Audit = db.Audit()
	roots, _ := json.Marshal([]string{f.root})
	if err := db.Settings().Set(context.Background(), SettingCatalogBuildRoots, roots); err != nil {
		t.Fatal(err)
	}
	old := probeBuildLoadMode
	probeBuildLoadMode = func(ctx context.Context, path string) (bool, bool) { f.probes++; return f.loadVal, true }
	t.Cleanup(func() { probeBuildLoadMode = old })
	return f
}

// binary writes an executable file under the allowed root and returns its path.
func (f *buildsFixture) binary(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *buildsFixture) send(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := do(t, f.s, authedRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *buildsFixture) create(t *testing.T, name, path, backend string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"engine_id": f.engine, "name": name, "binary_path": path, "backend": backend})
	code, out := f.send(t, "POST", "/api/v1/catalog/builds", string(b))
	if code != 201 {
		t.Fatalf("create %s: %d %v", name, code, out)
	}
	return out
}

func idOf(m map[string]any) string { return strconv.FormatInt(int64(m["id"].(float64)), 10) }

func TestBuildCreateGuards(t *testing.T) {
	f := newBuildsFixture(t)
	good := f.binary(t, "tree/bin/llama-server", 0o755)
	f.loadVal = true

	out := f.create(t, "tree-rocm", good, "rocm")
	if out["retired"] != false || out["load_mode_supported"] != true || f.probes != 1 {
		t.Errorf("create response = %v probes=%d, want live with the probe result", out, f.probes)
	}

	outside := filepath.Join(t.TempDir(), "llama-server")
	_ = os.WriteFile(outside, []byte("x"), 0o755)
	link := filepath.Join(f.root, "escape")
	_ = os.Symlink(outside, link)
	noexec := f.binary(t, "noexec/llama-server", 0o644)
	cases := []struct{ name, path, backend, field string }{
		{"outside the roots", outside, "rocm", "binary_path"},
		{"relative", "bin/llama-server", "rocm", "binary_path"},
		{"shell metacharacter", good + ";rm", "rocm", "binary_path"},
		{"missing file", filepath.Join(f.root, "nope"), "rocm", "binary_path"},
		{"a directory", f.root, "rocm", "binary_path"},
		{"not executable", noexec, "rocm", "binary_path"},
		{"symlink out of the roots", link, "rocm", "binary_path"},
		{"no path for a non-vllm build", "", "rocm", "binary_path"},
		{"bad backend", good, "cuda", "backend"},
	}
	for i, c := range cases {
		b, _ := json.Marshal(map[string]any{"engine_id": f.engine, "name": "case-" + strconv.Itoa(i), "binary_path": c.path, "backend": c.backend})
		code, body := f.send(t, "POST", "/api/v1/catalog/builds", string(b))
		fields, _ := body["fields"].(map[string]any)
		if code != 422 || fields[c.field] == nil {
			t.Errorf("%s: %d %v, want 422 on %s", c.name, code, body, c.field)
		}
	}
	// A duplicate live name is refused.
	b, _ := json.Marshal(map[string]any{"engine_id": f.engine, "name": "tree-rocm", "binary_path": good, "backend": "rocm"})
	if code, body := f.send(t, "POST", "/api/v1/catalog/builds", string(b)); code != 422 || body["fields"].(map[string]any)["name"] == nil {
		t.Errorf("duplicate name: %d %v", code, body)
	}
	// vLLM is a service, not a file: no path needed.
	f.create(t, "svc-vllm", "", "vllm")
}

func TestBuildRetireIsRefusedWhileAConfigUsesIt(t *testing.T) {
	f := newBuildsFixture(t)
	build := f.create(t, "tree-rocm", f.binary(t, "tree/bin/llama-server", 0o755), "rocm")
	other := f.create(t, "other-rocm", f.binary(t, "other/bin/llama-server", 0o755), "rocm")
	bid := int64(build["id"].(float64))
	cfgID, err := f.db.Catalog().CreateConfig(context.Background(), store.Config{
		Name: "uses-tree", VariantID: f.pre.variantID, WeightArtifactID: f.pre.artifactID, EngineID: f.engine, BuildID: bid,
	})
	if err != nil {
		t.Fatal(err)
	}

	code, body := f.send(t, "PATCH", "/api/v1/catalog/builds/"+idOf(build), `{"retired":true}`)
	if code != 409 || body["code"] != "build_in_use" || !strings.Contains(body["error"].(string), "uses-tree") {
		t.Fatalf("retire while referenced: %d %v, want 409 build_in_use naming the config", code, body)
	}
	got, _ := f.db.Catalog().GetBuild(context.Background(), bid)
	if got.Retired || got.BinaryPath == "" || got.Name != "tree-rocm" {
		t.Errorf("a refused retire still changed the row: %+v", got)
	}

	// Repoint the config, then retiring works and writes both the column and the legacy markers.
	cfg, _ := f.db.Catalog().GetConfig(context.Background(), cfgID)
	cfg.BuildID = int64(other["id"].(float64))
	if err := f.db.Catalog().UpdateConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	code, body = f.send(t, "PATCH", "/api/v1/catalog/builds/"+idOf(build), `{"retired":true}`)
	if code != 200 || body["retired"] != true || body["binary_path"] != "" || body["name"] != "tree-rocm" {
		t.Fatalf("retire: %d %v", code, body)
	}
	if _, has := body["load_mode_supported"]; has {
		t.Errorf("a retired build was probed: %v", body)
	}
	rows, _ := f.db.Audit().List(context.Background(), "catalog_build_retire", idOf(build), 5)
	if len(rows) != 1 {
		t.Errorf("audit rows for the retire = %d, want 1", len(rows))
	}

	// Un-retiring needs a real binary again, and drops the legacy prefix.
	if code, _ := f.send(t, "PATCH", "/api/v1/catalog/builds/"+idOf(build), `{"retired":false}`); code != 422 {
		t.Errorf("un-retire without a binary = %d, want 422", code)
	}
	live := f.binary(t, "tree/bin/llama-server", 0o755)
	code, body = f.send(t, "PATCH", "/api/v1/catalog/builds/"+idOf(build), `{"retired":false,"binary_path":"`+live+`"}`)
	if code != 200 || body["retired"] != false || body["name"] != "tree-rocm" || body["binary_path"] != live {
		t.Errorf("un-retire: %d %v", code, body)
	}
}

func TestBuildGetBodyRoundTripsAndETagGuards(t *testing.T) {
	f := newBuildsFixture(t)
	build := f.create(t, "tree-rocm", f.binary(t, "tree/bin/llama-server", 0o755), "rocm")
	w := do(t, f.s, authedRequest("GET", "/api/v1/catalog/builds/"+idOf(build), nil))
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" {
		t.Fatalf("GET: %d etag=%q", w.Code, etag)
	}
	// The write response (which carries load_mode_supported) is a valid PUT body, and the GET body is too.
	if code, body := f.send(t, "PUT", "/api/v1/catalog/builds/"+idOf(build), w.Body.String()); code != 200 {
		t.Fatalf("PUT of the GET body: %d %v", code, body)
	}
	b, _ := json.Marshal(build)
	if code, body := f.send(t, "PUT", "/api/v1/catalog/builds/"+idOf(build), string(b)); code != 200 {
		t.Fatalf("PUT of the create response: %d %v", code, body)
	}
	// A stale If-Match is refused with 412 and changes nothing.
	req := authedRequest("PATCH", "/api/v1/catalog/builds/"+idOf(build), strings.NewReader(`{"reason":"x"}`))
	req.Header.Set("If-Match", `W/"stale"`)
	if rec := do(t, f.s, req); rec.Code != http.StatusPreconditionFailed {
		t.Errorf("stale If-Match = %d, want 412", rec.Code)
	}
	if code, _ := f.send(t, "GET", "/api/v1/catalog/builds/9999", ""); code != 404 {
		t.Errorf("GET unknown build = %d, want 404", code)
	}
}

func TestBuildValidateExcludesItself(t *testing.T) {
	f := newBuildsFixture(t)
	path := f.binary(t, "tree/bin/llama-server", 0o755)
	build := f.create(t, "tree-rocm", path, "rocm")
	body, _ := json.Marshal(map[string]any{"engine_id": f.engine, "name": "tree-rocm", "binary_path": path, "backend": "rocm"})
	if code, out := f.send(t, "POST", "/api/v1/catalog/builds/"+idOf(build)+"/validate", string(body)); code != 200 || out["valid"] != true {
		t.Errorf("validate-an-update of itself: %d %v", code, out)
	}
	if code, _ := f.send(t, "POST", "/api/v1/catalog/builds/validate", string(body)); code != 422 {
		t.Errorf("validate as a create of an existing name = %d, want 422", code)
	}
}

// ADR-0020 F10: config.parallel is gone from the API. An older client's body that still carries it is accepted and
// the value is ignored; no response returns it.
func TestConfigParallelIsAcceptedAndIgnored(t *testing.T) {
	f := newBuildsFixture(t)
	b, _ := json.Marshal(map[string]any{"name": "old-client", "variant_id": f.pre.variantID, "weight_artifact_id": f.pre.artifactID,
		"engine_id": f.engine, "build_id": f.pre.buildID, "n_ctx": 4096, "parallel": 7, "extra_args": []string{"--parallel", "1"}})
	w := do(t, f.s, authedRequest("POST", "/api/v1/catalog/configs", strings.NewReader(string(b))))
	if w.Code != 201 {
		t.Fatalf("a body carrying parallel was refused: %d %s", w.Code, w.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if _, has := created["parallel"]; has {
		t.Errorf("the response still returns parallel: %s", w.Body.String())
	}
	g := do(t, f.s, authedRequest("GET", "/api/v1/catalog/configs", nil))
	if strings.Contains(g.Body.String(), `"parallel"`) {
		t.Errorf("the config list still returns parallel")
	}
}

// A build can be deleted only after it is retired and only when no config uses it; the file on disk is never touched.
func TestBuildDeleteGuards(t *testing.T) {
	f := newBuildsFixture(t)
	path := f.binary(t, "tree/bin/llama-server", 0o755)
	build := f.create(t, "tree-rocm", path, "rocm")

	if code, body := f.send(t, "DELETE", "/api/v1/catalog/builds/"+idOf(build), ""); code != 409 || body["code"] != "build_not_retired" {
		t.Fatalf("delete of a live build: %d %v, want 409 build_not_retired", code, body)
	}
	bid := int64(build["id"].(float64))
	cfgID, err := f.db.Catalog().CreateConfig(context.Background(), store.Config{
		Name: "uses-tree", VariantID: f.pre.variantID, WeightArtifactID: f.pre.artifactID, EngineID: f.engine, BuildID: bid,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retire by writing the row directly (the API refuses while a config uses it), then try to delete.
	b, _ := f.db.Catalog().GetBuild(context.Background(), bid)
	b.Retired, b.BinaryPath = true, ""
	if err := f.db.Catalog().UpdateBuild(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if code, body := f.send(t, "DELETE", "/api/v1/catalog/builds/"+idOf(build), ""); code != 409 || body["code"] != "build_in_use" {
		t.Fatalf("delete while a config uses it: %d %v, want 409 build_in_use", code, body)
	}
	if err := f.db.Catalog().DeleteConfig(context.Background(), cfgID); err != nil {
		t.Fatal(err)
	}
	if code, body := f.send(t, "DELETE", "/api/v1/catalog/builds/"+idOf(build), ""); code != 200 {
		t.Fatalf("delete of a retired, unused build: %d %v", code, body)
	}
	if _, err := f.db.Catalog().GetBuild(context.Background(), bid); err == nil {
		t.Error("the row is still there")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the binary on disk was touched: %v", err)
	}
	if code, _ := f.send(t, "DELETE", "/api/v1/catalog/builds/"+idOf(build), ""); code != 404 {
		t.Errorf("second delete = %d, want 404", code)
	}
	rows, _ := f.db.Audit().List(context.Background(), "catalog_build_delete", idOf(build), 5)
	if len(rows) != 1 {
		t.Errorf("audit rows for the delete = %d, want 1", len(rows))
	}
}

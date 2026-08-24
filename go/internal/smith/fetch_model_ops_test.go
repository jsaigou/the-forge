// SPDX-License-Identifier: Apache-2.0

package smith

// fetch_model_ops_test.go — P3smith. Exercises fetch_model's native ops
// against a fake HTTP server (httptest) and a real in-memory store: resume,
// checksum mismatch, GGUF magic/header handling, atomic finalize, and the
// catalog-link write through the applyCatalogChange dispatch seam. No
// network beyond loopback.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

func newFetchTestSmith(t *testing.T, modelsDir string) *Smith {
	t.Helper()
	return New(Deps{
		Cfg:  func() *config.Config { return &config.Config{Paths: config.Paths{ModelsDir: modelsDir}} },
		Logf: func(string, ...any) {},
	})
}

func withFakeHF(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := fetchModelBaseURL
	fetchModelBaseURL = srv.URL
	t.Cleanup(func() { fetchModelBaseURL = old })
}

func baseFetchParams() map[string]string {
	return map[string]string{
		"hf_repo":  "testorg/testmodel",
		"filename": "model-q4_k_m.gguf",
	}
}

// ── step 0: download ────────────────────────────────────────────────────────

func TestOpFetchDownload_HappyPath(t *testing.T) {
	payload := strings.Repeat("forge-fetch-model-payload\n", 400)
	withFakeHF(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	})
	dir := t.TempDir()
	s := newFetchTestSmith(t, dir)

	res, err := s.opFetchDownload(context.Background(), baseFetchParams())
	if err != nil {
		t.Fatalf("opFetchDownload: %v", err)
	}
	part := filepath.Join(dir, "model-q4_k_m.gguf.part")
	got, rerr := os.ReadFile(part)
	if rerr != nil {
		t.Fatalf("read .part: %v", rerr)
	}
	if string(got) != payload {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(payload))
	}
	if !strings.Contains(res.Stdout, part) {
		t.Errorf("stdout %q should name the .part path", res.Stdout)
	}
}

func TestOpFetchDownload_ResumesFromPartial(t *testing.T) {
	payload := []byte(strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 40))
	half := len(payload) / 2
	var rangeSeen string
	withFakeHF(t, func(w http.ResponseWriter, r *http.Request) {
		rangeSeen = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[half:])
	})
	dir := t.TempDir()
	part := filepath.Join(dir, "model-q4_k_m.gguf.part")
	if err := os.WriteFile(part, payload[:half], 0o644); err != nil {
		t.Fatalf("seed .part: %v", err)
	}
	s := newFetchTestSmith(t, dir)

	if _, err := s.opFetchDownload(context.Background(), baseFetchParams()); err != nil {
		t.Fatalf("opFetchDownload (resume): %v", err)
	}
	if rangeSeen == "" || !strings.HasPrefix(rangeSeen, "bytes=") {
		t.Errorf("Range header = %q, want a bytes= resume request", rangeSeen)
	}
	got, rerr := os.ReadFile(part)
	if rerr != nil {
		t.Fatalf("read .part: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Errorf("resumed file is %d bytes, want full %d", len(got), len(payload))
	}
}

func TestOpFetchDownload_ServerIgnoresRange_RestartsClean(t *testing.T) {
	payload := []byte("full-content-from-a-server-that-ignored-range")
	withFakeHF(t, func(w http.ResponseWriter, _ *http.Request) {
		// No 206 — a plain 200 means "start over"; the op must truncate.
		_, _ = w.Write(payload)
	})
	dir := t.TempDir()
	part := filepath.Join(dir, "model-q4_k_m.gguf.part")
	stalePrefix := append([]byte("STALE-STALE-STALE"), payload[:10]...)
	if err := os.WriteFile(part, stalePrefix, 0o644); err != nil {
		t.Fatalf("seed .part: %v", err)
	}
	s := newFetchTestSmith(t, dir)

	if _, err := s.opFetchDownload(context.Background(), baseFetchParams()); err != nil {
		t.Fatalf("opFetchDownload: %v", err)
	}
	got, _ := os.ReadFile(part)
	if string(got) != string(payload) {
		t.Errorf("file = %q, want a clean restart to %q", got, payload)
	}
}

func TestOpFetchDownload_RefusesExistingFinal(t *testing.T) {
	withFakeHF(t, func(w http.ResponseWriter, _ *http.Request) {})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model-q4_k_m.gguf"), []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFetchTestSmith(t, dir)
	if _, err := s.opFetchDownload(context.Background(), baseFetchParams()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an already-exists refusal", err)
	}
}

func TestOpFetchDownload_HTTPErrorFailsAfterRetries(t *testing.T) {
	calls := 0
	withFakeHF(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	s := newFetchTestSmith(t, t.TempDir())
	_, err := s.opFetchDownload(context.Background(), baseFetchParams())
	if err == nil {
		t.Fatal("expected failure on persistent HTTP 500")
	}
	if calls != fetchModelMaxAttempts {
		t.Errorf("server saw %d calls, want exactly %d bounded attempts", calls, fetchModelMaxAttempts)
	}
}

// ── step 1: verify ────────────────────────────────────────────────────────

func TestOpFetchVerify_ChecksumMismatch(t *testing.T) {
	payload := []byte("some model bytes")
	withFakeHF(t, func(http.ResponseWriter, *http.Request) {})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model-q4_k_m.gguf.part"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	params := baseFetchParams()
	params["sha256"] = strings.Repeat("ab", 32) // valid hex, wrong value
	s := newFetchTestSmith(t, dir)

	_, err := s.opFetchVerify(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
}

func TestOpFetchVerify_GGUFMagicFailure(t *testing.T) {
	withFakeHF(t, func(http.ResponseWriter, *http.Request) {})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model-q4_k_m.gguf.part"), []byte("definitely not a gguf file"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFetchTestSmith(t, dir)
	_, err := s.opFetchVerify(context.Background(), baseFetchParams())
	if err == nil || !strings.Contains(err.Error(), "GGUF") {
		t.Fatalf("err = %v, want a GGUF magic/header failure", err)
	}
}

func TestOpFetchVerify_GGUFHeaderParsedIntoResult(t *testing.T) {
	withFakeHF(t, func(http.ResponseWriter, *http.Request) {})
	dir := t.TempDir()
	ggufBytes := testGGUF(t)
	if err := os.WriteFile(filepath.Join(dir, "model-q4_k_m.gguf.part"), ggufBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFetchTestSmith(t, dir)
	res, err := s.opFetchVerify(context.Background(), baseFetchParams())
	if err != nil {
		t.Fatalf("opFetchVerify: %v", err)
	}
	for _, want := range []string{"trained_ctx=4096", "arch=testarch"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", res.Stdout, want)
		}
	}
	if res.CheckpointNote == "" {
		t.Error("verify must set a CheckpointNote — that's the evidence the checkpoint shows")
	}
}

func TestOpFetchVerify_NonGGUFSkipsHeaderCheck(t *testing.T) {
	withFakeHF(t, func(http.ResponseWriter, *http.Request) {})
	dir := t.TempDir()
	params := map[string]string{"hf_repo": "testorg/testmodel", "filename": "tokenizer.json"}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json.part"), []byte("{\"ok\":true}"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFetchTestSmith(t, dir)
	if _, err := s.opFetchVerify(context.Background(), params); err != nil {
		t.Fatalf("non-gguf file must verify without the header check: %v", err)
	}
}

// ── step 2: finalize ────────────────────────────────────────────────────────

func TestOpFetchFinalize_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "model-q4_k_m.gguf")
	part := final + ".part"
	if err := os.WriteFile(part, []byte("complete"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newFetchTestSmith(t, dir)
	if _, err := s.opFetchFinalize(context.Background(), baseFetchParams()); err != nil {
		t.Fatalf("opFetchFinalize: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("final missing after finalize: %v", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf(".part still exists (stat err = %v)", err)
	}
}

func TestOpFetchFinalize_MissingPartFailsClosed(t *testing.T) {
	s := newFetchTestSmith(t, t.TempDir())
	if _, err := s.opFetchFinalize(context.Background(), baseFetchParams()); err == nil {
		t.Fatal("finalize without a downloaded .part must fail")
	}
}

// ── step 3: catalog link through the dispatch seam ───────────────────────

func seedFetchLinkCatalog(t *testing.T, db *store.DB, modelsDir string) {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()
	mdlID, err := cat.CreateModel(ctx, store.Model{Name: "testmodel"})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	varID, err := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "8b"})
	if err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	ggufFormat, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	artID, err := cat.CreateArtifact(ctx, store.Artifact{
		VariantID: varID, FormatID: ggufFormat.ID, ArtifactType: "weight",
		FilePath: "old-file.gguf", FileSizeBytes: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	cfgID, err := cat.CreateConfig(ctx, store.Config{
		Name: "testmodel-8b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID,
		Status: "unverified", Visibility: "visible",
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	_ = cfgID
	_ = modelsDir
}

func TestOpFetchCatalogLink_UpdatesArtifactViaDispatchSeam(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t)
	seedFetchLinkCatalog(t, db, dir)
	// The finalized file the link re-reads for GGUF facts.
	if err := os.WriteFile(filepath.Join(dir, "new-model.gguf"), testGGUF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := db.Catalog()
	cfgRow, err := cat.ConfigByName(context.Background(), "testmodel-8b")
	if err != nil {
		t.Fatalf("ConfigByName: %v", err)
	}

	params := map[string]string{
		"hf_repo":       "testorg/testmodel",
		"filename":      "model-q4_k_m.gguf",
		"dest_rel_path": "new-model.gguf",
		"config_name":   "testmodel-8b",
	}
	// resolveFetchModelTarget reads filename for the .gguf check but writes
	// dest_rel_path as the artifact path; rename the fixture accordingly by
	// pointing dest at the same file we wrote.
	s := New(Deps{
		Store:    db,
		Settings: db.Settings(),
		Catalog:  cat,
		Cfg:      func() *config.Config { return &config.Config{Paths: config.Paths{ModelsDir: dir}} },
		Logf:     func(string, ...any) {},
	})

	res, err := s.opFetchCatalogLink(context.Background(), params)
	if err != nil {
		t.Fatalf("opFetchCatalogLink: %v", err)
	}
	if !strings.Contains(res.Stdout, "dispatch seam") {
		t.Errorf("stdout = %q, want it to name the dispatch seam", res.Stdout)
	}
	art, gerr := cat.GetArtifact(context.Background(), cfgRow.WeightArtifactID)
	if gerr != nil {
		t.Fatalf("GetArtifact: %v", gerr)
	}
	if art.FilePath != "new-model.gguf" {
		t.Errorf("artifact FilePath = %q, want new-model.gguf", art.FilePath)
	}
	if art.GGUFTrainedCtx != 4096 || art.GGUFArch != "testarch" {
		t.Errorf("artifact GGUF fields = arch %q ctx %d, want testarch/4096 (parsed from the finalized file)", art.GGUFArch, art.GGUFTrainedCtx)
	}
}

func TestOpFetchCatalogLink_NoConfigNameIsHonestNoOp(t *testing.T) {
	s := newFetchTestSmith(t, t.TempDir())
	res, err := s.opFetchCatalogLink(context.Background(), baseFetchParams())
	if err != nil {
		t.Fatalf("no config_name must be a no-op success, got: %v", err)
	}
	if !strings.Contains(res.Stdout, "skipped") {
		t.Errorf("stdout = %q, want it to say linking was skipped", res.Stdout)
	}
}

// TestApplyCatalogChange_ArtifactOnly pins the minimal implementation
// boundary: artifact rows work behind the same seam KindCatalogChange uses;
// every other table still reports not-implemented rather than pretending.
func TestApplyCatalogChange_ArtifactOnly(t *testing.T) {
	db := openDB(t)
	seedFetchLinkCatalog(t, db, t.TempDir())
	ctx := context.Background()

	rowJSON, _ := json.Marshal(store.Artifact{
		ID: 999999, VariantID: 1, FormatID: 1, ArtifactType: "weight",
		FilePath: "x.gguf",
	})
	s := New(Deps{Catalog: db.Catalog(), Logf: func(string, ...any) {}})
	err := s.applyCatalogChange(ctx, catalogChangeDetail{Op: "update", Table: "variant", Row: rowJSON})
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("variant-table change = %v, want not-yet-implemented", err)
	}
	err = s.applyCatalogChange(ctx, catalogChangeDetail{Op: "delete", Table: "artifact", Row: rowJSON})
	if err == nil || !strings.Contains(err.Error(), "op") {
		t.Fatalf("delete op = %v, want unsupported-op error", err)
	}
}

// ── param validation (dispatch-time re-checks + optionals) ────────────────

func TestResolveFetchModelTarget_RejectsTraversalAndBadParams(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
	}{
		{"missing repo", map[string]string{"filename": "m.gguf"}},
		{"traversal dest", map[string]string{"hf_repo": "o/m", "filename": "m.gguf", "dest_rel_path": "../../etc/x"}},
		{"bad repo", map[string]string{"hf_repo": "../evil", "filename": "m.gguf"}},
		{"filename separator", map[string]string{"hf_repo": "o/m", "filename": "sub/dir.gguf"}},
		{"bad sha", map[string]string{"hf_repo": "o/m", "filename": "m.gguf", "sha256": "zz"}},
	}
	for _, tc := range cases {
		if _, err := resolveFetchModelTarget(tc.params); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
	tgt, err := resolveFetchModelTarget(map[string]string{"hf_repo": "o/m", "filename": "m.gguf"})
	if err != nil || tgt.DestRelPath != "m.gguf" {
		t.Errorf("defaults: tgt=%+v err=%v, want DestRelPath defaulting to the filename", tgt, err)
	}
}

func TestValidateParams_FetchModelOptionalsAccepted(t *testing.T) {
	proc, ok := procedures.Get("fetch_model")
	if !ok {
		t.Fatal("fetch_model not registered")
	}
	full := map[string]string{
		"hf_repo": "o/m", "filename": "m.gguf",
		"dest_rel_path": "d/m.gguf", "sha256": strings.Repeat("ab", 32),
		"config_name": "cfg",
	}
	minimal := map[string]string{"hf_repo": "o/m", "filename": "m.gguf"}
	for name, params := range map[string]map[string]string{"full": full, "minimal": minimal} {
		if err := procedures.ValidateParams(proc, params); err != nil {
			t.Errorf("%s param set rejected: %v", name, err)
		}
	}
	if err := procedures.ValidateParams(proc, map[string]string{"hf_repo": "o/m"}); err == nil {
		t.Error("missing required filename must fail validation")
	}
	if err := procedures.ValidateParams(proc, map[string]string{"hf_repo": "o/m", "filename": "m.gguf", "extra": "x"}); err == nil {
		t.Error("unknown param must fail validation even when optionals are absent")
	}
}

// ── GGUF fixture ────────────────────────────────────────────────────────────

// testGGUF builds a minimal but genuinely parseable GGUF header:
// general.architecture="testarch", testarch.context_length=4096 (uint32),
// general.parameter_count=7e9 (uint64). Header + KV only, zero tensors.
func testGGUF(t *testing.T) []byte {
	t.Helper()
	var buf []byte
	appendLE := func(v any) {
		var tmp [8]byte
		n := 0
		switch x := v.(type) {
		case uint32:
			binary.LittleEndian.PutUint32(tmp[:], x)
			n = 4
		case uint64:
			binary.LittleEndian.PutUint64(tmp[:], x)
			n = 8
		default:
			t.Fatalf("unsupported LE type %T", v)
		}
		buf = append(buf, tmp[:n]...)
	}
	str := func(s string) {
		appendLE(uint64(len(s)))
		buf = append(buf, s...)
	}
	kvString := func(k, v string) {
		str(k)
		appendLE(uint32(8)) // typeString
		str(v)
	}
	kvUint := func(k string, vt uint32, v any) {
		str(k)
		appendLE(vt)
		appendLE(v)
	}

	appendLE(uint32(0x46554747)) // "GGUF"
	appendLE(uint32(3))          // version
	appendLE(uint64(0))          // tensor count
	appendLE(uint64(3))          // kv count
	kvString("general.architecture", "testarch")
	kvUint("testarch.context_length", 4, uint32(4096))
	kvUint("general.parameter_count", 10, uint64(7_000_000_000))
	return buf
}

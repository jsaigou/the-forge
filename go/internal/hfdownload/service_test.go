// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/store"
)

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, *store.DB, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dir := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{ModelsDir: dir}}

	svc := New(Deps{
		Store: db,
		HF:    &hf.Client{HTTP: srv.Client(), BaseURL: srv.URL},
		Cfg:   func() *config.Config { return cfg },
		Logf:  t.Logf,
	})
	return svc, db, dir
}

func waitForState(t *testing.T, svc *Service, jobID int64, want string, timeout time.Duration) store.ModelDownloadRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last store.ModelDownloadRow
	for time.Now().Before(deadline) {
		job, err := svc.Get(context.Background(), jobID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		last = job
		if job.State == want {
			return job
		}
		if job.State == "failed" && want != "failed" {
			t.Fatalf("job %d failed early: %s", jobID, job.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d never reached state %q, last state %q (error=%q)", jobID, want, last.State, last.Error)
	return last
}

// testGGUF builds a minimal but genuinely parseable GGUF header — the
// exact fixture smith/fetch_model_ops_test.go uses, reproduced here since
// that test helper is unexported in a different package.
func testGGUF(t *testing.T, arch string, ctxLen uint32) []byte {
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
	appendLE(uint64(2))          // kv count
	kvString("general.architecture", arch)
	kvUint(arch+".context_length", 4, ctxLen)
	return buf
}

func TestPreflightBlocksOnExistingFile(t *testing.T) {
	svc, _, dir := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-q4_k_m.gguf"), []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Preflight(context.Background(), "org/model", []PreflightFile{
		{Filename: "model-q4_k_m.gguf", SizeBytes: 100},
	}, "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !report.Blocked {
		t.Fatal("expected Preflight to block on an existing destination file")
	}
}

func TestPreflightFlagsBackendAboveVulkanCeiling(t *testing.T) {
	svc, _, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	report, err := svc.Preflight(context.Background(), "org/big-model", []PreflightFile{
		{Filename: "model.gguf", SizeBytes: 80 << 30}, // 80 GB, over the ~63GB ceiling
	}, "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.RequiresBackend != "rocm" {
		t.Errorf("RequiresBackend = %q, want rocm for an 80GB model", report.RequiresBackend)
	}
	if report.Blocked {
		t.Error("a large model must not be BLOCKED — only flagged for the right backend")
	}
}

func TestStartJobDownloadsVerifiesAndRegisters(t *testing.T) {
	payload := testGGUF(t, "testarch", 8192)
	svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})

	jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
		Repo: "testorg/testmodel", Files: []PreflightFile{
			{Filename: "testmodel-Q4_K_M.gguf", SizeBytes: int64(len(payload))},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	job := waitForState(t, svc, jobID, "done", 5*time.Second)
	if job.CreatedConfigID == 0 {
		t.Fatal("done job must have a created_config_id")
	}

	cat := db.Catalog()
	cfg, err := cat.GetConfig(context.Background(), job.CreatedConfigID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.Status != "unverified" || cfg.Visibility != "hidden" {
		t.Errorf("Config = status=%q visibility=%q, want unverified/hidden", cfg.Status, cfg.Visibility)
	}
	if cfg.Parallel != 1 {
		t.Errorf("Config.Parallel = %d, want 1 (the hard rule — --parallel N splits context)", cfg.Parallel)
	}
	if cfg.NCtx != 8192 {
		t.Errorf("Config.NCtx = %d, want 8192 (from the GGUF trained_ctx)", cfg.NCtx)
	}
	art, err := cat.GetArtifact(context.Background(), cfg.WeightArtifactID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.GGUFArch != "testarch" {
		t.Errorf("Artifact.GGUFArch = %q, want testarch", art.GGUFArch)
	}
	vt, err := cat.GetVariant(context.Background(), cfg.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	model, err := cat.GetModel(context.Background(), vt.ModelID)
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if model.HFRepo != "testorg/testmodel" {
		t.Errorf("Model.HFRepo = %q, want testorg/testmodel", model.HFRepo)
	}
}

// waitForFileProgress polls until jobID's file has written at least
// minBytes to disk (per the store's own bookkeeping) — the only reliable
// signal that the CLIENT side has actually persisted bytes, as opposed to
// the server having merely flushed them onto the wire (a real race: the
// server flushing does not mean io.Copy on the client has processed that
// data into the .part file yet).
func waitForFileProgress(t *testing.T, svc *Service, jobID int64, minBytes int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		files, err := svc.ListFiles(context.Background(), jobID)
		if err == nil && len(files) > 0 && files[0].BytesDone >= minBytes {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %d bytes written", jobID, minBytes)
}

func TestPauseThenResumeCompletesViaRangeRequest(t *testing.T) {
	payload := testGGUF(t, "testarch", 4096)
	// Pad so the transfer takes long enough to pause mid-flight.
	payload = append(payload, strings.Repeat("X", 200000)...)
	half := len(payload) / 2

	var sawRangeResume int32

	svc, _, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			// The concurrently-running asset-enrichment goroutine's
			// Card() call (GET /api/models/{repo}) — answer immediately.
			// Routing this into the same hang-until-cancelled branch
			// below would leave an uncancellable (context.Background())
			// request outstanding forever, and httptest.Server.Close()
			// (t.Cleanup) blocks until every outstanding request
			// finishes — a real deadlock, not a flaky timing issue.
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.Header.Get("Range") != "" {
			atomic.StoreInt32(&sawRangeResume, 1)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[half:])
			return
		}
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(payload[:half])
		if flusher != nil {
			flusher.Flush()
		}
		// Hang until the client cancels (Pause) rather than closing
		// cleanly — a clean 200 completion would look like "server
		// ignored Range", which is a different code path.
		<-r.Context().Done()
	})

	jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
		Repo: "testorg/pausable", Files: []PreflightFile{
			{Filename: "pausable-Q4_K_M.gguf", SizeBytes: int64(len(payload))},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	// Wait for real, client-persisted progress (not just "the server
	// flushed") before pausing, or Pause can race a cancellation through
	// before any bytes landed on disk — which would send a second,
	// still-Range-less request into the same hanging branch.
	waitForFileProgress(t, svc, jobID, 1, 3*time.Second)
	if !svc.Pause(jobID) {
		t.Fatal("Pause reported the job wasn't running")
	}
	waitForState(t, svc, jobID, "paused", 3*time.Second)

	svc.Start(jobID)
	job := waitForState(t, svc, jobID, "done", 5*time.Second)
	if atomic.LoadInt32(&sawRangeResume) != 1 {
		t.Error("resume never sent a Range request — pause/resume must continue from the .part offset, not restart")
	}
	if job.BytesDone != int64(len(payload)) {
		t.Errorf("BytesDone = %d, want the full %d bytes", job.BytesDone, len(payload))
	}
}

func TestCancelDeletesPartialFile(t *testing.T) {
	svc, _, dir := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			_, _ = w.Write([]byte(`{}`)) // enrichment's Card() call — see the pause test's comment
			return
		}
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(strings.Repeat("Y", 1000)))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	})

	jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
		Repo: "testorg/cancelme", Files: []PreflightFile{
			{Filename: "cancelme.gguf", SizeBytes: 500000},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	waitForFileProgress(t, svc, jobID, 1, 3*time.Second)

	if err := svc.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForState(t, svc, jobID, "cancelled", 3*time.Second)

	part := filepath.Join(dir, "cancelme.gguf.part")
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("cancel must delete the .part file, stat err = %v", err)
	}
}

func TestDeleteRefusesRunningJob(t *testing.T) {
	svc, _, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(strings.Repeat("Y", 1000)))
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	})

	jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
		Repo: "testorg/stillrunning", Files: []PreflightFile{
			{Filename: "stillrunning.gguf", SizeBytes: 500000},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	waitForFileProgress(t, svc, jobID, 1, 3*time.Second)

	if err := svc.Delete(context.Background(), jobID); err == nil {
		t.Fatal("Delete on a running job must be refused")
	}
	if _, err := svc.Get(context.Background(), jobID); err != nil {
		t.Errorf("job must still exist after a refused delete, Get: %v", err)
	}

	if err := svc.Cancel(context.Background(), jobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForState(t, svc, jobID, "cancelled", 3*time.Second)
}

func TestDeleteRemovesTerminalJob(t *testing.T) {
	svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	dl := db.ModelDownloads()
	id, err := dl.Create(context.Background(), store.ModelDownloadRow{Repo: "org/done", State: "failed"}, []store.ModelDownloadFileRow{
		{Filename: "m.gguf", DestRelPath: "m.gguf"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if files, err := svc.ListFiles(context.Background(), id); err != nil || len(files) != 0 {
		t.Errorf("ListFiles after delete = (%v, %v), want (0 files, nil) — cascade must remove per-file rows too", files, err)
	}
}

func TestDeleteUnknownJobIsNotFound(t *testing.T) {
	svc, _, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := svc.Delete(context.Background(), 999999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete(unknown) = %v, want ErrNotFound", err)
	}
}

func TestBootReconcileFlipsRunningToPaused(t *testing.T) {
	svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	dl := db.ModelDownloads()
	id, err := dl.Create(context.Background(), store.ModelDownloadRow{Repo: "org/orphaned"}, []store.ModelDownloadFileRow{
		{Filename: "m.gguf", DestRelPath: "m.gguf"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := dl.UpdateState(context.Background(), id, "running", ""); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}

	if err := svc.BootReconcile(context.Background()); err != nil {
		t.Fatalf("BootReconcile: %v", err)
	}
	job, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != "paused" {
		t.Errorf("state = %q, want paused after boot reconcile", job.State)
	}
}

func TestShardedDownloadRegistersOneArtifactPerShard(t *testing.T) {
	// Real HF split GGUFs carry a valid magic+header in EVERY shard (just
	// fewer metadata KVs past the first) — downloadAndVerifyFile requires
	// a readable GGUF header on every .gguf-named file, so the fixture
	// must be a real (if minimal) GGUF for both shards, not junk bytes.
	shard1 := testGGUF(t, "sharded-arch", 16384)
	shard2 := testGGUF(t, "sharded-arch", 16384)

	svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "00001") {
			_, _ = w.Write(shard1)
			return
		}
		_, _ = w.Write(shard2)
	})

	jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
		Repo: "testorg/sharded", Files: []PreflightFile{
			{Filename: "model-00001-of-00002.gguf", SizeBytes: int64(len(shard1))},
			{Filename: "model-00002-of-00002.gguf", SizeBytes: int64(len(shard2))},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	job := waitForState(t, svc, jobID, "done", 5*time.Second)

	cat := db.Catalog()
	cfg, err := cat.GetConfig(context.Background(), job.CreatedConfigID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	art, err := cat.GetArtifact(context.Background(), cfg.WeightArtifactID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !strings.Contains(art.FilePath, "00001-of-00002") {
		t.Errorf("weight artifact FilePath = %q, want the first shard", art.FilePath)
	}
	if art.ShardSetID == "" {
		t.Error("a multi-file download must set ShardSetID")
	}
	files, err := svc.ListFiles(context.Background(), jobID)
	if err != nil || len(files) != 2 {
		t.Fatalf("ListFiles = %+v, err=%v, want 2 files", files, err)
	}
	for _, f := range files {
		if f.State != "verified" {
			t.Errorf("file %q state = %q, want verified", f.Filename, f.State)
		}
	}

	// The second shard must ALSO get its own Artifact row (same
	// ShardSetID, un-flagged sibling — nothing points a FK at it).
	variantArtifacts, err := cat.ListArtifactsForVariant(context.Background(), art.VariantID)
	if err != nil {
		t.Fatalf("ListArtifactsForVariant: %v", err)
	}
	if len(variantArtifacts) != 2 {
		t.Fatalf("variant artifacts = %d, want 2 (one per shard)", len(variantArtifacts))
	}
	var second store.Artifact
	found := false
	for _, a := range variantArtifacts {
		if a.ID != art.ID {
			second = a
			found = true
		}
	}
	if !found {
		t.Fatal("second shard's artifact row not found")
	}
	if !strings.Contains(second.FilePath, "00002-of-00002") {
		t.Errorf("second artifact FilePath = %q, want the second shard", second.FilePath)
	}
	if second.ShardSetID != art.ShardSetID {
		t.Errorf("second artifact ShardSetID = %q, want %q (matching the primary)", second.ShardSetID, art.ShardSetID)
	}
	if second.ArtifactType != "weight" {
		t.Errorf("second artifact ArtifactType = %q, want weight", second.ArtifactType)
	}
}

func TestConfigNameRepointsExistingArtifactInsteadOfRegisteringNewModel(t *testing.T) {
	payload := testGGUF(t, "repoint-arch", 2048)
	svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})

	// Seed an existing Config the way the old fetch_model tests did.
	ctx := context.Background()
	cat := db.Catalog()
	mdlID, _ := cat.CreateModel(ctx, store.Model{Name: "existing"})
	varID, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdlID, Name: "8b"})
	ggufFormat, _ := cat.FormatByName(ctx, "GGUF")
	artID, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: varID, FormatID: ggufFormat.ID, ArtifactType: "weight", FilePath: "old.gguf", FileSizeBytes: 1})
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	cfgID, err := cat.CreateConfig(ctx, store.Config{Name: "existing-8b", VariantID: varID, WeightArtifactID: artID, EngineID: eng.ID, Status: "unverified", Visibility: "visible"})
	if err != nil {
		t.Fatalf("seed CreateConfig: %v", err)
	}

	jobID, err := svc.StartJob(ctx, CreateJobRequest{
		Repo: "testorg/repoint", ConfigName: "existing-8b", Files: []PreflightFile{
			{Filename: "new-model.gguf", SizeBytes: int64(len(payload))},
		},
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	job := waitForState(t, svc, jobID, "done", 5*time.Second)
	if job.CreatedConfigID != 0 {
		t.Errorf("CreatedConfigID = %d, want 0 — repoint mode must not auto-register a new Config", job.CreatedConfigID)
	}

	art, err := cat.GetArtifact(ctx, artID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.FilePath != "new-model.gguf" || art.GGUFArch != "repoint-arch" {
		t.Errorf("artifact = %+v, want it repointed at the new file", art)
	}
	// Confirm no NEW model was created under the same name.
	models, _ := cat.ListModels(ctx)
	count := 0
	for _, m := range models {
		if m.Name == "existing" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d models named 'existing', want exactly 1 (repoint must not duplicate)", count)
	}
	_ = cfgID
}

// SPDX-License-Identifier: Apache-2.0

package smith

// build_refresh_ops_test.go — autonomous-remediation Sprint 6. Exercises
// the judgment-critical paths directly (the precheck's dirty-tree/
// mid-rebase refusal and its deliberate detached-HEAD tolerance, fork
// resolution's live-setting cross-checks, and the catalog discovery
// helpers) rather than the full 12-step run, which needs real git/cmake
// binaries and is covered by the live evaluation runs instead
// (docs/v5-smith.md §13.4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── opBuildGitPrecheck ───────────────────────────────────────────────────

// fakeGitRunStep answers "git status --porcelain" with a fixed string and
// fails any other argv it doesn't recognize — narrow by design, so a test
// using it is explicit about exactly which commands it expects.
type fakeGitRunStep struct{ porcelain string }

func (f fakeGitRunStep) run(_ context.Context, spec procedures.StepSpec) (procedures.StepResult, error) {
	// argv is ["git", "-C", <path>, "status", "--porcelain"] — "status" is
	// index 3.
	if len(spec.Argv) >= 4 && spec.Argv[0] == "git" && spec.Argv[3] == "status" {
		return procedures.StepResult{Stdout: f.porcelain}, nil
	}
	return procedures.StepResult{}, nil
}

func newTestGitRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	return root
}

func TestOpBuildGitPrecheck_CleanTreeNoRebasePasses(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "test-clean", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), RunStep: fakeGitRunStep{porcelain: ""}.run, Logf: func(string, ...any) {}})

	if _, err := s.opBuildGitPrecheck(context.Background(), map[string]string{"binary": "test-clean"}); err != nil {
		t.Fatalf("opBuildGitPrecheck: %v, want nil (clean tree, no rebase in progress)", err)
	}
}

func TestOpBuildGitPrecheck_DirtyTreeRefuses(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "test-dirty", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	// Real shape of `git status --porcelain` on a tree with hand-edited,
	// uncommitted patches — exactly poolside's live state on ForgeHost this
	// sprint (M common/speculative.cpp, M ggml/src/ggml-cuda/common.cuh,
	// ?? common.cuh.bak.gfx1151).
	dirty := " M common/speculative.cpp\n M ggml/src/ggml-cuda/common.cuh\n?? common.cuh.bak.gfx1151\n"
	s := New(Deps{Store: db, Settings: db.Settings(), RunStep: fakeGitRunStep{porcelain: dirty}.run, Logf: func(string, ...any) {}})

	_, err := s.opBuildGitPrecheck(context.Background(), map[string]string{"binary": "test-dirty"})
	if err == nil {
		t.Fatal("expected opBuildGitPrecheck to refuse a dirty tree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error = %q, want it to mention uncommitted changes", err.Error())
	}
}

func TestOpBuildGitPrecheck_MidRebaseRefuses(t *testing.T) {
	root := newTestGitRoot(t)
	if err := os.MkdirAll(filepath.Join(root, ".git", "rebase-merge"), 0o755); err != nil {
		t.Fatalf("mkdir rebase-merge: %v", err)
	}
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "test-midrebase", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), RunStep: fakeGitRunStep{porcelain: ""}.run, Logf: func(string, ...any) {}})

	_, err := s.opBuildGitPrecheck(context.Background(), map[string]string{"binary": "test-midrebase"})
	if err == nil {
		t.Fatal("expected opBuildGitPrecheck to refuse a tree with a rebase already in progress")
	}
	if !strings.Contains(err.Error(), "rebase in progress") {
		t.Errorf("error = %q, want it to mention a rebase already in progress", err.Error())
	}
}

// TestOpBuildGitPrecheck_DetachedHEADIsTolerated pins the deliberate
// scope correction found live on ForgeHost this sprint: every real tracked
// fork runs pinned to a specific tested commit (detached HEAD), which is
// normal operational practice, not a sign of trouble — the precheck must
// not refuse it. There is nothing HEAD-shaped to fake here (the op never
// reads HEAD at all); this test exists so a future change that adds a
// branch check gets caught immediately by this test's clean pass.
func TestOpBuildGitPrecheck_DetachedHEADIsTolerated(t *testing.T) {
	root := newTestGitRoot(t)
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("8bf3c1130abcdef1234567890abcdef12345678\n"), 0o644); err != nil {
		t.Fatalf("write detached HEAD: %v", err)
	}
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "test-detached", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), RunStep: fakeGitRunStep{porcelain: ""}.run, Logf: func(string, ...any) {}})

	if _, err := s.opBuildGitPrecheck(context.Background(), map[string]string{"binary": "test-detached"}); err != nil {
		t.Fatalf("opBuildGitPrecheck refused a detached-but-clean tree: %v", err)
	}
}

// ── resolveBuildRefreshFork ───────────────────────────────────────────────

func TestResolveBuildRefreshFork_NotTracked(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	_, _, err := s.resolveBuildRefreshFork(context.Background(), "nope, not tracked")
	if err == nil || !strings.Contains(err.Error(), "not in smith.binaries.tracked") {
		t.Fatalf("err = %v, want an unmapped-binary error", err)
	}
}

func TestResolveBuildRefreshFork_NoRegistryEntry(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "untracked-tree", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	_, _, err := s.resolveBuildRefreshFork(context.Background(), "untracked-tree")
	if err == nil || !strings.Contains(err.Error(), "no reviewed fork registered") {
		t.Fatalf("err = %v, want a no-registry-entry error", err)
	}
}

func TestResolveBuildRefreshFork_UpstreamRefDisagreesFailsClosed(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "mismatched", Path: mustExecutable(t), SourceKind: "git", SourceRef: root,
		UpstreamRef: "some-other-remote/main", // disagrees with the registry entry above
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	_, _, err := s.resolveBuildRefreshFork(context.Background(), "mismatched")
	if err == nil || !strings.Contains(err.Error(), "disagrees with the reviewed fork registry") {
		t.Fatalf("err = %v, want an upstream_ref-disagreement error", err)
	}
}

func TestResolveBuildRefreshFork_HappyPath(t *testing.T) {
	root := newTestGitRoot(t)
	fork := buildRefreshFork{
		SourceRef: root, Remote: "upstream", UpstreamRef: "upstream/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	}
	db := openDB(t)
	seedBuildRefreshForks(t, db, fork)
	tb := TrackedBinary{Name: "happy-path", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "upstream/master"}
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, tb))
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})

	gotTB, gotFork, err := s.resolveBuildRefreshFork(context.Background(), "happy-path")
	if err != nil {
		t.Fatalf("resolveBuildRefreshFork: %v", err)
	}
	if gotTB.Path != tb.Path || gotFork.Remote != "upstream" {
		t.Fatalf("got (%+v, %+v), want tracked binary + the registered fork", gotTB, gotFork)
	}
}

// ── buildsForSourceTree / configsForBuild / findCandidateBuildID ────────

func seedBuildRefreshCatalog(t *testing.T, db *store.DB, sourceRef string) (oldBuild, candidateBuild store.Build) {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	oldID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "test-old", BinaryPath: filepath.Join(sourceRef, "build-rocm", "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild old: %v", err)
	}
	candID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "test-old" + buildRefreshCandidateSuffix,
		BinaryPath: filepath.Join(sourceRef, buildRefreshDirName("rocm"), "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild candidate: %v", err)
	}
	// A build under a DIFFERENT tree must never show up in this tree's
	// discovery — the real regression this test guards against would be a
	// path-prefix bug (e.g. "/opt/forge/llama.cpp" matching
	// "/opt/forge/llama.cpp-poolside" as a prefix).
	if _, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "unrelated", BinaryPath: "/opt/forge/llama.cpp-poolside/build/bin/llama-server", Backend: "rocm",
	}); err != nil {
		t.Fatalf("CreateBuild unrelated: %v", err)
	}
	oldBuild.ID, candidateBuild.ID = oldID, candID
	oldBuild.EngineID, candidateBuild.EngineID = eng.ID, eng.ID
	oldBuild.Backend, candidateBuild.Backend = "rocm", "rocm"
	oldBuild.Name, candidateBuild.Name = "test-old", "test-old"+buildRefreshCandidateSuffix
	oldBuild.BinaryPath = filepath.Join(sourceRef, "build-rocm", "bin", "llama-server")
	candidateBuild.BinaryPath = filepath.Join(sourceRef, buildRefreshDirName("rocm"), "bin", "llama-server")
	return oldBuild, candidateBuild
}

func TestBuildsForSourceTree_ExactTreeOnlyNoPrefixLeak(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Logf: func(string, ...any) {}})
	root := "/opt/forge/llama.cpp"
	seedBuildRefreshCatalog(t, db, root)

	got, err := s.buildsForSourceTree(context.Background(), root)
	if err != nil {
		t.Fatalf("buildsForSourceTree: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d builds, want 2 (old + candidate, excluding the unrelated prefix-similar tree)", len(got))
	}
}

func TestFindCandidateBuildID_DistinguishesCandidateFromOld(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Logf: func(string, ...any) {}})
	root := "/opt/forge/llama.cpp-puzzle"
	_, candidate := seedBuildRefreshCatalog(t, db, root)

	builds, err := s.buildsForSourceTree(context.Background(), root)
	if err != nil {
		t.Fatalf("buildsForSourceTree: %v", err)
	}
	id, err := s.findCandidateBuildID(context.Background(), builds, "rocm")
	if err != nil {
		t.Fatalf("findCandidateBuildID: %v", err)
	}
	if id != candidate.ID {
		t.Fatalf("findCandidateBuildID = %d, want the candidate build id %d", id, candidate.ID)
	}
}

func TestConfigsForBuild_OnlyMatchingBuildID(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	cat := db.Catalog()
	seedBrainCatalog(t, db) // gives us variant/artifact/engine plumbing to reuse
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	buildA, _ := cat.CreateBuild(ctx, store.Build{EngineID: eng.ID, Name: "a", BinaryPath: "/x/a/bin/llama-server", Backend: "rocm"})
	buildB, _ := cat.CreateBuild(ctx, store.Build{EngineID: eng.ID, Name: "b", BinaryPath: "/x/b/bin/llama-server", Backend: "rocm"})
	cfg, err := cat.ConfigByName(ctx, "ornith-35b")
	if err != nil {
		t.Fatalf("ConfigByName: %v", err)
	}
	cfg.BuildID = buildA
	if err := cat.UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: cat, Logf: func(string, ...any) {}})
	got, err := s.configsForBuild(ctx, buildA)
	if err != nil {
		t.Fatalf("configsForBuild: %v", err)
	}
	if len(got) != 1 || got[0].Name != "ornith-35b" {
		t.Fatalf("configsForBuild(buildA) = %+v, want just ornith-35b", got)
	}
	got, err = s.configsForBuild(ctx, buildB)
	if err != nil {
		t.Fatalf("configsForBuild: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("configsForBuild(buildB) = %+v, want none", got)
	}
}

// ── deviceLostPatterns ────────────────────────────────────────────────────

func TestScanJournalForDeviceLost(t *testing.T) {
	clean := "Aug 20 12:00:00 forgehost forge-a1[123]: model loaded, serving requests\n"
	if hits := scanJournalForDeviceLost(clean); len(hits) != 0 {
		t.Errorf("clean journal flagged hits: %v", hits)
	}
	dirty := "Aug 20 12:00:00 forgehost forge-a1[123]: vk::Queue::submit: ErrorDeviceLost\n"
	if hits := scanJournalForDeviceLost(dirty); len(hits) == 0 {
		t.Error("expected the known device-lost signature to be flagged")
	}
}

// ── test helpers ──────────────────────────────────────────────────────────

// mustExecutable returns a real, absolute path to an existing regular
// file — resolveBuildRefreshFork's happy path doesn't stat the tracked
// binary's own Path itself (only opBuildRecordInstalled does, via
// BinaryPathAllowed), but TrackedBinary.Path still needs to decode/round-
// trip through settings JSON, so any real path will do; the test binary
// itself is a convenient one guaranteed to exist.
func mustExecutable(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return p
}

func mustBinariesJSON(t *testing.T, tbs ...TrackedBinary) string {
	t.Helper()
	raw, err := json.Marshal(tbs)
	if err != nil {
		t.Fatalf("marshal tracked binaries: %v", err)
	}
	return string(raw)
}

// seedBuildRefreshForks writes the fork registry into settings — the seam
// resolveBuildRefreshFork reads live since the registry moved out of Go
// data (open-source-readiness finding 4; migration 0061 seeds the reviewed
// production entries, tests seed their own synthetic trees).
func seedBuildRefreshForks(t *testing.T, db *store.DB, forks ...buildRefreshFork) {
	t.Helper()
	raw, err := json.Marshal(forks)
	if err != nil {
		t.Fatalf("marshal fork registry: %v", err)
	}
	setSetting(t, db, SettingBuildRefreshForks, string(raw))
}

// ── opBuildCatalogPromote: transactional revert ──────────────────────────

// TestOpBuildCatalogPromote_RevertsAllOnPartialFailure pins a real bug
// found and fixed in this sprint: `return out, revert(err)` (and the
// equivalent revertAll pattern) read the StepResult BEFORE the revert
// closure's own mutation of it was guaranteed to have run, so a reverted
// run's returned Stdout could silently come back empty — losing the "which
// configs got reverted" log trail exactly when it mattered most (a partial
// failure). This drives a real two-config promote where the second
// config's reload fails, and asserts BOTH configs end up back on the
// original build id AND the returned log actually contains both revert
// lines.
func TestOpBuildCatalogPromote_RevertsAllOnPartialFailure(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends:             map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
		RepresentativeConfig: map[string]string{"rocm": "ornith-35b"},
	})
	ctx := context.Background()
	cat := db.Catalog()
	seedBrainCatalog(t, db) // "ornith-35b" config + variant/artifact/engine plumbing
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	oldID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "old", BinaryPath: filepath.Join(root, "build-rocm", "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild old: %v", err)
	}
	candID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "old" + buildRefreshCandidateSuffix,
		BinaryPath: filepath.Join(root, buildRefreshDirName("rocm"), "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild candidate: %v", err)
	}

	// Two OTHER configs on the old build (ornith-35b is the representative
	// — already "migrated" by a prior reliability-test step in the real
	// flow, so it's deliberately left off the old build here). configA is
	// not currently resident (no reload attempted); configB IS resident
	// and its reload will fail, forcing a revert of both.
	mkConfig := func(name string) {
		v, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mustModelID(t, cat), Name: name})
		gguf, _ := cat.FormatByName(ctx, "GGUF")
		art, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: v, FormatID: gguf.ID, ArtifactType: "weight", FilePath: name + ".gguf", FileSizeBytes: 1})
		if _, err := cat.CreateConfig(ctx, store.Config{
			Name: name, VariantID: v, WeightArtifactID: art, EngineID: eng.ID, BuildID: oldID,
			NCtx: 4096, Parallel: 1, Status: "unverified", Visibility: "visible",
		}); err != nil {
			t.Fatalf("CreateConfig %s: %v", name, err)
		}
	}
	mkConfig("configA")
	mkConfig("configB")

	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "promote-test", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	placer := &stubPlacer{unloadResult: &engine.Result{Success: false, Message: "stub unload failure"}}
	sched := newStubSched(map[string]string{"a2": "configB"}) // only configB is resident
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: cat, Placer: placer, Sched: sched, Logf: func(string, ...any) {}})

	res, err := s.opBuildCatalogPromote(ctx, map[string]string{"binary": "promote-test"})
	if err == nil {
		t.Fatal("expected opBuildCatalogPromote to fail (configB's reload was rigged to fail)")
	}
	if !strings.Contains(err.Error(), "reload resident config configB") {
		t.Errorf("error = %v, want it to name configB's reload failure", err)
	}
	if !strings.Contains(res.Stdout, "reverted config configA") || !strings.Contains(res.Stdout, "reverted config configB") {
		t.Fatalf("res.Stdout = %q, want it to contain revert log lines for BOTH configA and configB — this is the exact bug this test pins", res.Stdout)
	}

	gotA, err := cat.ConfigByName(ctx, "configA")
	if err != nil {
		t.Fatalf("ConfigByName configA: %v", err)
	}
	if gotA.BuildID != oldID {
		t.Errorf("configA.BuildID = %d, want reverted to the original %d", gotA.BuildID, oldID)
	}
	gotB, err := cat.ConfigByName(ctx, "configB")
	if err != nil {
		t.Fatalf("ConfigByName configB: %v", err)
	}
	if gotB.BuildID != oldID {
		t.Errorf("configB.BuildID = %d, want reverted to the original %d", gotB.BuildID, oldID)
	}
	_ = candID
}

func mustModelID(t *testing.T, cat store.Catalog) int64 {
	t.Helper()
	id, err := cat.CreateModel(context.Background(), store.Model{Name: t.Name()})
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	return id
}

// TestOpBuildCatalogPromote_NilPlacerFailsClosed pins a real bug found and
// fixed this sprint: opBuildCatalogPromote could reach a direct
// s.d.Placer.Unload/Load call (when a repointed config happens to be
// resident) with no nil check anywhere above it — every other dispatcher
// in this codebase nil-checks Placer before use; a nil Placer here would
// have panicked, and executeAction's spawning goroutine has no recover.
func TestOpBuildCatalogPromote_NilPlacerFailsClosed(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "nil-placer-test", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Logf: func(string, ...any) {}}) // no Placer wired

	_, err := s.opBuildCatalogPromote(context.Background(), map[string]string{"binary": "nil-placer-test"})
	if !errors.Is(err, ErrPlacerUnwired) {
		t.Fatalf("err = %v, want ErrPlacerUnwired", err)
	}
}

// TestBuildRefreshForkRegistryShape pins the shape rules every fork
// registry entry must satisfy, exercised against the SHIPPED SYNTHETIC
// example seed (docs/examples/smith-local-seed.example.json via
// ImportLocalSeed) — the same path a real deployment's recipes take.
// Migrations seed smith.build_refresh.forks EMPTY (two-layer knowledge
// architecture: recipes are deployment data, never shipped), so the
// example file IS the reference every deployment starts from, and this
// test guards it: internally coherent topology (remote matches its
// upstream ref), backends carry flags and a representative, keys agree.
// It exists because the Sprint 6 eval found, live, two registry classes
// of drift: a tracked entry naming a remote that did not exist, and
// recipes carrying flags the tree's upstream had removed. Operator edits
// to a deployment's own registry are the operator's review, under the
// procedure's unchanged fail-closed cross-checks.
func TestBuildRefreshForkRegistryShape(t *testing.T) {
	db := openDB(t)
	importExampleSeed(t, db)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	forks := s.BuildRefreshForks(context.Background())
	if len(forks) == 0 {
		t.Fatal("the shipped example seed imported no forks — docs/examples/smith-local-seed.example.json must parse through the live decoder")
	}
	seen := map[string]bool{}
	for _, fork := range forks {
		sourceRef := fork.SourceRef
		if seen[sourceRef] {
			t.Errorf("fork %q: duplicate source_ref in the registry", sourceRef)
		}
		seen[sourceRef] = true
		if sourceRef == "" {
			t.Error("fork with empty SourceRef — entries key on the tree's filesystem root")
		}
		if fork.Remote == "" || fork.UpstreamRef == "" {
			t.Errorf("fork %q: Remote/UpstreamRef must both be set", sourceRef)
		}
		if !strings.HasPrefix(fork.UpstreamRef, fork.Remote+"/") {
			t.Errorf("fork %q: UpstreamRef %q must live on its own remote %q", sourceRef, fork.UpstreamRef, fork.Remote)
		}
		if len(fork.Backends) == 0 {
			t.Errorf("fork %q: Backends must not be empty", sourceRef)
		}
		for backend, flags := range fork.Backends {
			if flags.Backend != backend {
				t.Errorf("fork %q backend %q: flags.Backend = %q, must equal the map key", sourceRef, backend, flags.Backend)
			}
			if len(flags.ConfigureFlags) == 0 {
				t.Errorf("fork %q backend %q: ConfigureFlags must not be empty", sourceRef, backend)
			}
			if flags.LibDirSubstring != "" && !strings.Contains(flags.LibDirSubstring, "/") {
				t.Errorf("fork %q backend %q: LibDirSubstring %q looks like a typo", sourceRef, backend, flags.LibDirSubstring)
			}
			if _, ok := fork.RepresentativeConfig[backend]; !ok {
				t.Errorf("fork %q backend %q: no RepresentativeConfig — the reliability test would fail closed at run time", sourceRef, backend)
			}
		}
		for backend := range fork.RepresentativeConfig {
			if _, ok := fork.Backends[backend]; !ok {
				t.Errorf("fork %q: representative named for backend %q, which is not in Backends", sourceRef, backend)
			}
		}
	}
	if !seen["/opt/example/llama.cpp"] {
		t.Error("example fork entry missing from the shipped seed — the reference every deployment starts from")
	}
}

// TestBuildRefreshExampleSeedCarriesROriginPattern pins the one
// trap-carrying detail the SHIPPED example demonstrates (build-refresh.md
// §2): the $ORIGIN RPATH literal, carried as ONE argv element because the
// procedure has no shell layer to quote it. Concrete deployment recipes
// carry their own reviewed flags; the $ORIGIN pattern is the mechanism
// knowledge that travels with the product.
func TestBuildRefreshExampleSeedCarriesROriginPattern(t *testing.T) {
	db := openDB(t)
	importExampleSeed(t, db)
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	var example *buildRefreshFork
	for _, f := range s.BuildRefreshForks(context.Background()) {
		if f.SourceRef == "/opt/example/llama.cpp" {
			example = &f
			break
		}
	}
	if example == nil {
		t.Fatal("example fork missing from the shipped seed")
	}
	hasROrigin := false
	for _, flag := range example.Backends["vulkan"].ConfigureFlags {
		if flag == "-DCMAKE_INSTALL_RPATH=$ORIGIN" {
			hasROrigin = true
		}
	}
	if !hasROrigin {
		t.Error("example vulkan flags lost the $ORIGIN RPATH literal — the documented single-argv-element trap (no shell layer to quote it)")
	}
}

// TestGiantPrefillCharsForSizesToCanaryContext pins eval run 72's failure:
// the fixed ~120K-token scenario got an HTTP 400 from a 131072-context
// canary that physically cannot hold it. The prompt must scale to the
// canary's own context — and stay at the full documented scenario for
// large-context canaries and unknown (nCtx <= 0) configs alike.
func TestGiantPrefillCharsForSizesToCanaryContext(t *testing.T) {
	cases := []struct {
		nCtx int
		want int
	}{
		{0, buildRefreshGiantPrefillChars},       // unknown: full scenario, fail loudly on a real mismatch
		{-1, buildRefreshGiantPrefillChars},      // malformed: same
		{40960, 107520},                          // swallow-32b-class: 75% × 3.5 chars/token
		{131072, 344064},                         // the run-72 canary class
		{262144, buildRefreshGiantPrefillChars},  // fits the full scenario — no shrink
		{1048576, buildRefreshGiantPrefillChars}, // nemotron-class: full scenario
	}
	for _, tc := range cases {
		if got := giantPrefillCharsFor(tc.nCtx); got != tc.want {
			t.Errorf("giantPrefillCharsFor(%d) = %d, want %d", tc.nCtx, got, tc.want)
		}
	}
	if got := buildGiantPrefillPrompt(1000); len(got) < 1000 {
		t.Errorf("buildGiantPrefillPrompt(1000) len = %d, want ≥ 1000", len(got))
	}
}

// TestReliabilityRevertTarget pins eval run 75's mid-reliability-test
// interruption finding: when an attempt is killed between its repoint and
// its revert, the next attempt captures the CANDIDATE as its "original",
// and a naive revert would restore production onto the unvetted build.
func TestReliabilityRevertTarget(t *testing.T) {
	if got := reliabilityRevertTarget(5, 12, 5); got != 5 {
		t.Errorf("clean anchor = %d, want 5 (the attempt's own original)", got)
	}
	if got := reliabilityRevertTarget(12, 12, 5); got != 5 {
		t.Errorf("polluted anchor (== candidate) = %d, want 5 (the backend's original build, never the candidate)", got)
	}
	if got := reliabilityRevertTarget(7, 12, 5); got != 7 {
		t.Errorf("operator-moved anchor = %d, want 7 (a real non-candidate original is honored as-is)", got)
	}
}

// TestIsBuildRefreshCandidatePath pins the shared predicate every
// discovered-build consumer must agree on (reliability loop, repoint plan,
// promote, cleanup): rows whose binary sits in a build-smith-<backend>
// dir are candidates, full stop — and the check must not leak across
// backends or trees.
func TestIsBuildRefreshCandidatePath(t *testing.T) {
	cases := []struct {
		path    string
		backend string
		want    bool
	}{
		{"/x/tree/build-smith-vulkan/bin/llama-server", "vulkan", true},
		{"/x/tree/build-smith-rocm/bin/llama-server", "rocm", true},
		{"/x/tree/build/bin/llama-server", "rocm", false},
		{"/x/tree/build-vulkan-new/bin/llama-server", "vulkan", false},
		// different backend's candidate dir is not THIS backend's candidate
		{"/x/tree/build-smith-vulkan/bin/llama-server", "rocm", false},
	}
	for _, tc := range cases {
		if got := isBuildRefreshCandidatePath(store.Build{BinaryPath: tc.path, Backend: tc.backend}); got != tc.want {
			t.Errorf("isBuildRefreshCandidatePath(%q, %q) = %v, want %v", tc.path, tc.backend, got, tc.want)
		}
	}
}

// TestFindOrCreateCandidateBuild_AdoptsRowFromInterruptedRun pins the fix
// for eval run 62's orphan: its reliability test created the vulkan
// candidate row, then failed on the double-load bug — leaving the row
// behind. Without adoption, every failed retry would add another orphan;
// with it, retries converge on the one row.
func TestFindOrCreateCandidateBuild_AdoptsRowFromInterruptedRun(t *testing.T) {
	db := openDB(t)
	root := "/opt/forge/llama.cpp"
	oldBuild, candidate := seedBuildRefreshCatalog(t, db, root)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Logf: func(string, ...any) {}})

	newBin := filepath.Join(root, buildRefreshDirName("rocm"), "bin", "llama-server")
	id, adopted, err := s.findOrCreateCandidateBuild(context.Background(), oldBuild, newBin)
	if err != nil {
		t.Fatalf("findOrCreateCandidateBuild: %v", err)
	}
	if !adopted || id != candidate.ID {
		t.Fatalf("id/adopted = %d/%v, want adoption of the existing candidate %d/true", id, adopted, candidate.ID)
	}

	// A different old build with no candidate yet gets a fresh row…
	other := store.Build{EngineID: oldBuild.EngineID, Name: "other-build", Backend: "rocm"}
	otherBin := filepath.Join(root, "build-smith-other", "bin", "llama-server")
	id2, adopted2, err := s.findOrCreateCandidateBuild(context.Background(), other, otherBin)
	if err != nil {
		t.Fatalf("findOrCreateCandidateBuild (create): %v", err)
	}
	if adopted2 {
		t.Fatalf("adopted = true, want a freshly created row for a build with no candidate")
	}
	// …and a repeat call adopts exactly that row.
	id3, adopted3, err := s.findOrCreateCandidateBuild(context.Background(), other, otherBin)
	if err != nil || !adopted3 || id3 != id2 {
		t.Fatalf("repeat call = %d/%v/%v, want adoption of the just-created row %d", id3, adopted3, err, id2)
	}
}

// TestLoadConfigViaPlacer_UnloadsResidentBeforeLoading pins eval run 62's
// live failure: a real consumer loaded the canary config mid-run (before
// the maintenance window opened at the reliability step), and the test's
// bare Load was refused by the engine's double-load guard. The resident
// instance was also running the OLD binary — unloading it first is the
// only reconciliation that actually tests the candidate.
func TestLoadConfigViaPlacer_UnloadsResidentBeforeLoading(t *testing.T) {
	db := openDB(t)
	placer := &stubPlacer{plan: engine.Plan{Fits: true, Slot: "a2"}}
	sched := newStubSched(map[string]string{"a1": "canary-config"})
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Placer: placer, Sched: sched, Logf: func(string, ...any) {}})

	slot, err := s.loadConfigViaPlacer(context.Background(), "canary-config")
	if err != nil {
		t.Fatalf("loadConfigViaPlacer: %v", err)
	}
	if slot != "a2" {
		t.Fatalf("slot = %q, want a2 from the stub fit plan", slot)
	}
	if len(placer.unloads) != 1 || placer.unloads[0] != "a1" {
		t.Fatalf("unloads = %v, want exactly one unload of the resident slot a1", placer.unloads)
	}
	if len(placer.loads) != 1 || placer.loads[0].Mode != "canary-config" || placer.loads[0].Slot != "a2" {
		t.Fatalf("loads = %v, want exactly one fresh load of canary-config onto a2", placer.loads)
	}
}

// TestLoadConfigViaPlacer_ResidentUnloadFailureFailsClosed — the other
// half of the same regression: when the resident instance can't be
// evicted, the reliability test must fail closed (the revert path then
// restores the repoint) rather than proceeding against the old binary.
func TestLoadConfigViaPlacer_ResidentUnloadFailureFailsClosed(t *testing.T) {
	db := openDB(t)
	placer := &stubPlacer{
		plan:         engine.Plan{Fits: true, Slot: "a2"},
		unloadResult: &engine.Result{Success: false, Message: "stub unload failure"},
	}
	sched := newStubSched(map[string]string{"a1": "canary-config"})
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Placer: placer, Sched: sched, Logf: func(string, ...any) {}})

	if _, err := s.loadConfigViaPlacer(context.Background(), "canary-config"); err == nil {
		t.Fatal("expected an error when the resident unload fails")
	}
	if len(placer.loads) != 0 {
		t.Fatalf("loads = %v, want NO load attempted after a failed resident unload", placer.loads)
	}
}

// TestPromoteRepointPlan_NamesEveryConfigThePromoteWillTouch pins the
// G8-class disclosure: the promote checkpoint's note must name the exact
// configs (and the candidate build) that promotion will repoint, so a
// mislabeled tracked binary can never hide nemotron-class blast radius
// behind a friendly label.
func TestPromoteRepointPlan_NamesEveryConfigThePromoteWillTouch(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	cat := db.Catalog()
	seedBrainCatalog(t, db)
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}
	root := "/opt/forge/llama.cpp"
	oldID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-rocm", BinaryPath: filepath.Join(root, "build", "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild old: %v", err)
	}
	candID, err := cat.CreateBuild(ctx, store.Build{
		EngineID: eng.ID, Name: "standard-rocm" + buildRefreshCandidateSuffix,
		BinaryPath: filepath.Join(root, buildRefreshDirName("rocm"), "bin", "llama-server"), Backend: "rocm",
	})
	if err != nil {
		t.Fatalf("CreateBuild candidate: %v", err)
	}
	mkConfig := func(name string) {
		v, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mustModelID(t, cat), Name: name})
		gguf, _ := cat.FormatByName(ctx, "GGUF")
		art, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: v, FormatID: gguf.ID, ArtifactType: "weight", FilePath: name + ".gguf", FileSizeBytes: 1})
		if _, err := cat.CreateConfig(ctx, store.Config{
			Name: name, VariantID: v, WeightArtifactID: art, EngineID: eng.ID, BuildID: oldID,
			NCtx: 4096, Parallel: 1, Status: "unverified", Visibility: "visible",
		}); err != nil {
			t.Fatalf("CreateConfig %s: %v", name, err)
		}
	}
	mkConfig("configA")
	mkConfig("configB")

	old := store.Build{ID: oldID, EngineID: eng.ID, Name: "standard-rocm", Backend: "rocm", BinaryPath: filepath.Join(root, "build", "bin", "llama-server")}
	cand := store.Build{ID: candID, EngineID: eng.ID, Name: "standard-rocm" + buildRefreshCandidateSuffix, Backend: "rocm", BinaryPath: filepath.Join(root, buildRefreshDirName("rocm"), "bin", "llama-server")}
	fork := buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"rocm": {Backend: "rocm"}},
	}
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: cat, Logf: func(string, ...any) {}})

	note, err := s.promoteRepointPlan(ctx, fork, []store.Build{old, cand})
	if err != nil {
		t.Fatalf("promoteRepointPlan: %v", err)
	}
	for _, want := range []string{"standard-rocm", "configA", "configB", fmt.Sprintf("candidate build %d", candID)} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to contain %q", note, want)
		}
	}
	if strings.Contains(note, "no configs to repoint") {
		t.Errorf("note = %q, must not claim there is nothing to repoint when two configs sit on the old build", note)
	}
}

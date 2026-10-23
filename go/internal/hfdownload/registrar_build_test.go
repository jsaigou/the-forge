// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// seedBuild adds a live build and (optionally) n configs that use it.
func seedBuild(t *testing.T, db *store.DB, name, path, backend string, nConfigs int) int64 {
	t.Helper()
	ctx := context.Background()
	cat := db.Catalog()
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	bid, err := cat.CreateBuild(ctx, store.Build{EngineID: eng.ID, Name: name, BinaryPath: path, Backend: backend})
	if err != nil {
		t.Fatalf("CreateBuild %s: %v", name, err)
	}
	if nConfigs > 0 {
		mdl, _ := cat.CreateModel(ctx, store.Model{Name: "m-" + name})
		vr, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdl, Name: "v"})
		ff, _ := cat.FormatByName(ctx, "GGUF")
		art, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: vr, FormatID: ff.ID, ArtifactType: "weight", FilePath: name + ".gguf", FileSizeBytes: 1})
		for i := 0; i < nConfigs; i++ {
			if _, err := cat.CreateConfig(ctx, store.Config{
				Name: name + "-cfg" + string(rune('a'+i)), VariantID: vr, WeightArtifactID: art,
				EngineID: eng.ID, BuildID: bid, Status: "unverified", Visibility: "hidden",
			}); err != nil {
				t.Fatalf("CreateConfig: %v", err)
			}
		}
	}
	return bid
}

// The old rule ("first row whose backend matches") picked a retired,
// binary-less vulkan row for every new model; retired rows must never be
// candidates, and among live ones the build already serving the most
// configs wins over a merely newer one.
func TestPickBuild_SkipsRetiredAndPrefersMostUsed(t *testing.T) {
	svc, db, _ := newTestService(t, func(http.ResponseWriter, *http.Request) {})
	seedBuild(t, db, "(retired) standard-vulkan", "", "vulkan", 0)
	seedBuild(t, db, "(retired) with-path", "/x/old", "vulkan", 0)
	workhorse := seedBuild(t, db, "standard-vulkan", "/x/std", "vulkan", 3)
	seedBuild(t, db, "fresh-candidate", "/x/new", "vulkan", 0)
	seedBuild(t, db, "some-rocm", "/x/rocm", "rocm", 1)

	got := svc.pickBuild(context.Background(), db.Catalog(), 1<<30)
	if got.ID != workhorse {
		t.Fatalf("vulkan pick = %q (id %d), want the most-used live build (id %d)", got.Name, got.ID, workhorse)
	}
	got = svc.pickBuild(context.Background(), db.Catalog(), vulkanCeilingBytes+1)
	if got.Name != "some-rocm" {
		t.Fatalf("rocm pick = %q, want some-rocm", got.Name)
	}
}

func TestPickBuild_TieBreaksTowardNewestAndEmptyIsZero(t *testing.T) {
	svc, db, _ := newTestService(t, func(http.ResponseWriter, *http.Request) {})
	if got := svc.pickBuild(context.Background(), db.Catalog(), 1<<30); got.ID != 0 {
		t.Fatalf("no builds: want zero Build, got %+v", got)
	}
	seedBuild(t, db, "a", "/x/a", "vulkan", 0)
	newest := seedBuild(t, db, "b", "/x/b", "vulkan", 0)
	if got := svc.pickBuild(context.Background(), db.Catalog(), 1<<30); got.ID != newest {
		t.Fatalf("tie: got id %d, want newest %d", got.ID, newest)
	}
}

func TestLoadModeArgs_FollowsTheBinary(t *testing.T) {
	probe := map[string][2]bool{ // path -> {supported, known}
		"/x/new":     {true, true},
		"/x/legacy":  {false, true},
		"/x/unknown": {false, false},
	}
	svc, _, _ := newTestService(t, func(http.ResponseWriter, *http.Request) {})
	svc.d.ProbeLoadMode = func(_ context.Context, p string) (bool, bool) { v := probe[p]; return v[0], v[1] }

	cases := []struct {
		build store.Build
		want  []string
	}{
		{store.Build{BinaryPath: "/x/new"}, []string{"--load-mode", "none"}},
		{store.Build{BinaryPath: "/x/legacy"}, []string{"--no-mmap"}},
		{store.Build{BinaryPath: "/x/unknown"}, []string{"--load-mode", "none"}}, // cannot tell -> modern default
		{store.Build{}, []string{"--load-mode", "none"}},                         // no build resolved
	}
	for _, c := range cases {
		if got := svc.loadModeArgs(context.Background(), c.build); !reflect.DeepEqual(got, c.want) {
			t.Errorf("loadModeArgs(%q) = %v, want %v", c.build.BinaryPath, got, c.want)
		}
	}
}

// End to end: a download registered while only a legacy-flag build exists
// must carry --no-mmap, and one registered against a modern build must not.
func TestRegisteredConfigCarriesTheFlagItsBuildUnderstands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supported bool
		wantFlag  string
		notFlag   string
	}{
		{"modern", true, "--load-mode", "--no-mmap"},
		{"legacy", false, "--no-mmap", "--load-mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := testGGUF(t, "testarch", 8192)
			svc, db, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) })
			bid := seedBuild(t, db, "vk-"+tc.name, "/x/vk", "vulkan", 0)
			svc.d.ProbeLoadMode = func(context.Context, string) (bool, bool) { return tc.supported, true }

			jobID, err := svc.StartJob(context.Background(), CreateJobRequest{
				Repo: "testorg/" + tc.name, Files: []PreflightFile{{Filename: tc.name + "-Q4_K_M.gguf", SizeBytes: int64(len(payload))}},
			})
			if err != nil {
				t.Fatalf("StartJob: %v", err)
			}
			job := waitForState(t, svc, jobID, "done", 5*time.Second)
			cfg, err := db.Catalog().GetConfig(context.Background(), job.CreatedConfigID)
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			if cfg.BuildID != bid {
				t.Errorf("BuildID = %d, want %d", cfg.BuildID, bid)
			}
			has := func(f string) bool {
				for _, a := range cfg.ExtraArgs {
					if a == f {
						return true
					}
				}
				return false
			}
			if !has(tc.wantFlag) || has(tc.notFlag) {
				t.Errorf("ExtraArgs = %v, want %s and not %s", cfg.ExtraArgs, tc.wantFlag, tc.notFlag)
			}
		})
	}
}

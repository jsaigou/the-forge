// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/cli"
)

// buildDir lays out what a Radiance build directory holds that the registration checks.
func buildDir(t *testing.T, mode os.FileMode, withKernels bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "radiance"), []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "radiance_home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if withKernels {
		if err := os.MkdirAll(filepath.Join(home, "kernels"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidateRadianceBuildDir(t *testing.T) {
	good := buildDir(t, 0o755, true)
	bin, home, err := validateRadianceBuildDir(good)
	if err != nil {
		t.Fatalf("a complete build directory must validate: %v", err)
	}
	if bin != filepath.Join(good, "bin", "radiance") || home != filepath.Join(good, "radiance_home") {
		t.Fatalf("unexpected paths %q %q", bin, home)
	}
	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{"not executable", buildDir(t, 0o644, true), "not executable"},
		{"no kernels", buildDir(t, 0o755, false), "no kernels directory"},
		{"empty dir", t.TempDir(), "no engine binary"},
	} {
		if _, _, err := validateRadianceBuildDir(tc.dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// fakeCatalog serves the three catalog calls registration makes and records the POSTs.
type fakeCatalog struct {
	engines []catalogEngine
	builds  []catalogBuild
	posted  []map[string]any
}

func (f *fakeCatalog) client(t *testing.T) *cli.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/catalog/engines", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(f.engines) })
	mux.HandleFunc("GET /api/v1/catalog/builds", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(f.builds) })
	mux.HandleFunc("POST /api/v1/catalog/builds", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.posted = append(f.posted, b)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(catalogBuild{ID: 26, Name: b["name"].(string)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &cli.Client{BaseURL: srv.URL, Key: "k", HTTP: &http.Client{Timeout: 5 * time.Second}}
}

var testEnv = map[string]string{"RADIANCE_HOME": "/opt/radiance/x/build/radiance_home", "RADIANCE_KERNELS": "libhalo"}

func TestRegisterRadianceBuildCreatesOnce(t *testing.T) {
	f := &fakeCatalog{engines: []catalogEngine{{ID: 1, Kind: "llamacpp"}, {ID: 3, Kind: "radiance"}}}
	c := f.client(t)
	out, err := registerRadianceBuild(c, "radiance", "radiance-unofficial/x", "/opt/radiance/x/build/bin/radiance", "rocm", testEnv)
	if err != nil || !out.Created || out.ID != 26 {
		t.Fatalf("first registration: %+v %v", out, err)
	}
	if len(f.posted) != 1 || f.posted[0]["engine_id"].(float64) != 3 {
		t.Fatalf("must post once, to the Radiance engine: %v", f.posted)
	}
	// The same registration again is a no-op: the row now exists with identical contents.
	f.builds = []catalogBuild{{ID: 26, EngineID: 3, Name: "radiance-unofficial/x", BinaryPath: "/opt/radiance/x/build/bin/radiance", Backend: "rocm", LaunchEnv: testEnv}}
	out, err = registerRadianceBuild(c, "radiance", "radiance-unofficial/x", "/opt/radiance/x/build/bin/radiance", "rocm", testEnv)
	if err != nil || out.Created || out.ID != 26 {
		t.Fatalf("repeat must report the existing build: %+v %v", out, err)
	}
	if len(f.posted) != 1 {
		t.Fatalf("a repeat must not post again: %v", f.posted)
	}
}

func TestRegisterRadianceBuildNeverOverwrites(t *testing.T) {
	f := &fakeCatalog{
		engines: []catalogEngine{{ID: 3, Kind: "radiance"}},
		builds:  []catalogBuild{{ID: 25, EngineID: 3, Name: "radiance-unofficial/x", BinaryPath: "/opt/other/bin/radiance", Backend: "rocm", LaunchEnv: testEnv}},
	}
	_, err := registerRadianceBuild(f.client(t), "radiance", "radiance-unofficial/x", "/opt/radiance/x/build/bin/radiance", "rocm", testEnv)
	if err == nil || !strings.Contains(err.Error(), "different contents") {
		t.Fatalf("a same-name build with other contents must be refused, got %v", err)
	}
	if len(f.posted) != 0 {
		t.Fatalf("nothing may be written: %v", f.posted)
	}
}

func TestRegisterRadianceBuildNeedsTheEngine(t *testing.T) {
	f := &fakeCatalog{engines: []catalogEngine{{ID: 1, Kind: "llamacpp"}}}
	if _, err := registerRadianceBuild(f.client(t), "radiance", "n", "/b", "rocm", testEnv); err == nil || !strings.Contains(err.Error(), `no "radiance" engine`) {
		t.Fatalf("want a no-engine error, got %v", err)
	}
}

func TestRadianceRegisterBuildDryRunContactsNothing(t *testing.T) {
	dir := buildDir(t, 0o755, true)
	var out, errb bytes.Buffer
	t.Setenv("FORGE_API_URL", "http://127.0.0.1:1") // unreachable: a dry run must not try it
	t.Setenv("FORGE_API_KEY", "")
	if err := runRadianceCLI([]string{"register-build", "-path", dir, "-name", "n", "-dry-run"}, &out, &errb); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"dry run", dir + "/bin/radiance", "RADIANCE_KERNELS=libhalo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q: %s", want, out.String())
		}
	}
}

func TestRadianceCLIUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runRadianceCLI(nil, &out, &errb); err == nil {
		t.Fatal("no subcommand must be a usage error")
	}
	if err := runRadianceCLI([]string{"register-build", "-name", "n"}, &out, &errb); err == nil || !strings.Contains(err.Error(), "-path and -name") {
		t.Fatalf("missing -path: %v", err)
	}
	if err := runRadianceCLI([]string{"register-build", "-path", "/x", "-name", "n", "-backend", "cuda"}, &out, &errb); err == nil || !strings.Contains(err.Error(), "-backend") {
		t.Fatalf("bad backend: %v", err)
	}
}

// A 2.x build is registered on the radiance2 engine row, and never on the 1.17 one even when both exist.
func TestRegisterRadianceBuildKind2UsesItsOwnEngine(t *testing.T) {
	f := &fakeCatalog{engines: []catalogEngine{{ID: 3, Kind: "radiance"}, {ID: 4, Kind: "radiance2"}}}
	out, err := registerRadianceBuild(f.client(t), "radiance2", "radiance-upstream/y", "/opt/radiance/y/build/bin/radiance", "rocm", testEnv)
	if err != nil || !out.Created {
		t.Fatalf("registration: %+v %v", out, err)
	}
	if len(f.posted) != 1 || f.posted[0]["engine_id"].(float64) != 4 {
		t.Fatalf("a radiance2 build must post to engine 4, got %v", f.posted)
	}
	// Without the radiance2 engine row (an older daemon) it is refused, never silently put on the 1.17 engine.
	g := &fakeCatalog{engines: []catalogEngine{{ID: 3, Kind: "radiance"}}}
	if _, err := registerRadianceBuild(g.client(t), "radiance2", "n", "/b", "rocm", testEnv); err == nil || !strings.Contains(err.Error(), `no "radiance2" engine`) {
		t.Fatalf("want a no-engine error for radiance2, got %v", err)
	}
	if len(g.posted) != 0 {
		t.Fatalf("nothing may be written: %v", g.posted)
	}
}

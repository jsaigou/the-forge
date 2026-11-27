// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeRadianceCatalog answers only the three listings radiance_host_prereqs reads; any other Catalog method panics (nil embedded interface).
type fakeRadianceCatalog struct {
	store.Catalog
	engines []store.Engine
	builds  []store.Build
	configs []store.Config
	err     error
}

func (f fakeRadianceCatalog) ListEngines(context.Context) ([]store.Engine, error) {
	return f.engines, f.err
}
func (f fakeRadianceCatalog) ListBuilds(context.Context) ([]store.Build, error) { return f.builds, nil }
func (f fakeRadianceCatalog) ListConfigs(context.Context) ([]store.Config, error) {
	return f.configs, nil
}

type fakeLaunchEnv map[int64]map[string]string

func (f fakeLaunchEnv) BuildLaunchEnv(id int64) (map[string]string, error) { return f[id], nil }

type errLaunchEnv struct{}

func (errLaunchEnv) BuildLaunchEnv(int64) (map[string]string, error) {
	return nil, errors.New("no launch_env column")
}

func radianceCat(retired bool) fakeRadianceCatalog {
	return fakeRadianceCatalog{
		engines: []store.Engine{{ID: 1, Name: "llama", Kind: "llamacpp"}, {ID: 2, Name: "radiance", Kind: "radiance"}},
		builds:  []store.Build{{ID: 10, EngineID: 1, Name: "rocm"}, {ID: 20, EngineID: 2, Name: "rad", Retired: retired}},
	}
}

func cwsrFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cwsr_enable")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mkdir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, mode); err != nil {
		t.Fatal(err)
	}
	return d
}

type noEnvHost struct{ osHostFS }

func (noEnvHost) Getenv(string) string { return "" }

func TestRadianceHostPrereqs_GatedOnCatalog(t *testing.T) {
	ctx := context.Background()
	// No Radiance build or config: the check is suppressed entirely.
	if !radianceNotInUse(&CheckEnv{Catalog: fakeRadianceCatalog{engines: []store.Engine{{ID: 1, Kind: "llamacpp"}}, builds: []store.Build{{EngineID: 1}}}}) {
		t.Error("llama.cpp-only catalog must not run the check")
	}
	if !radianceNotInUse(&CheckEnv{Catalog: radianceCat(true)}) {
		t.Error("a retired Radiance build alone must not run the check")
	}
	if radianceNotInUse(&CheckEnv{Catalog: radianceCat(false)}) {
		t.Error("an active Radiance build must run the check")
	}
	cfgOnly := fakeRadianceCatalog{engines: []store.Engine{{ID: 2, Kind: "radiance"}}, configs: []store.Config{{EngineID: 2}}}
	if radianceNotInUse(&CheckEnv{Catalog: cfgOnly}) {
		t.Error("a Radiance config must run the check")
	}
	if f := runRadianceHostPrereqs(ctx, &CheckEnv{Catalog: radianceCat(true)}); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("retired only: %q", f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, &CheckEnv{}); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("no catalog: %q", f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, &CheckEnv{Catalog: fakeRadianceCatalog{err: errors.New("db closed")}}); f.Severity != SeverityWarn {
		t.Errorf("catalog error: %s %q", f.Severity, f.Summary)
	}
}

func TestRadianceHostPrereqs_Cwsr(t *testing.T) {
	ctx := context.Background()
	base := func(cwsr string) *CheckEnv {
		return &CheckEnv{Catalog: radianceCat(false), Host: noEnvHost{}, CwsrPath: cwsr}
	}
	if f := runRadianceHostPrereqs(ctx, base(cwsrFile(t, "1\n"))); f.Severity != SeverityOK || f.SummaryKey != "checks.radiance_host_prereqs.ok" {
		t.Errorf("cwsr=1: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, base(cwsrFile(t, "0\n"))); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "cwsr_enable") {
		t.Errorf("cwsr=0: %s %q", f.Severity, f.Summary)
	}
	// A missing parameter file is "cannot verify" (the path is unverified), never a failure.
	f := runRadianceHostPrereqs(ctx, base(filepath.Join(t.TempDir(), "absent")))
	if f.Severity != SeverityOK || f.SummaryKey != "checks.radiance_host_prereqs.unverified" {
		t.Errorf("missing cwsr: %s %q", f.Severity, f.Summary)
	}
}

func TestRadianceHostPrereqs_LaunchEnvPathsAndPlugins(t *testing.T) {
	ctx := context.Background()
	good := mkdir(t, 0o755)
	writable := mkdir(t, 0o775)
	worldWritable := mkdir(t, 0o777)
	env := func(le BuildLaunchEnvReader) *CheckEnv {
		return &CheckEnv{Catalog: radianceCat(false), Host: noEnvHost{}, CwsrPath: cwsrFile(t, "1"), RadianceBuildEnv: le}
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"ROCM_PATH": good, "FLAG": "not-a-path"}})); f.Severity != SeverityOK {
		t.Errorf("good dir: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"ROCM_PATH": filepath.Join(good, "missing")}})); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "does not exist") {
		t.Errorf("missing dir: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"LD_LIBRARY_PATH": good + ":" + writable}})); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "writable") {
		t.Errorf("group-writable in a path list: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"X": worldWritable}})); f.Severity != SeverityWarn {
		t.Errorf("world-writable: %s %q", f.Severity, f.Summary)
	}
	// nil reader (launch_env column not there yet) and a reader error both mean "no env", the latter reported as cannot verify.
	if f := runRadianceHostPrereqs(ctx, env(nil)); f.Severity != SeverityOK {
		t.Errorf("nil reader: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, env(errLaunchEnv{})); f.Severity != SeverityOK || f.SummaryKey != "checks.radiance_host_prereqs.unverified" {
		t.Errorf("reader error: %s %q", f.Severity, f.Summary)
	}

	// RADIANCE_HOME from launch_env: the directory and its libhalo* entries.
	home := mkdir(t, 0o755)
	plugin := filepath.Join(home, "libhalo_plugin.so")
	if err := os.WriteFile(plugin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"RADIANCE_HOME": home}})); f.Severity != SeverityOK {
		t.Errorf("good home: %s %q", f.Severity, f.Summary)
	}
	if err := os.Chmod(plugin, 0o666); err != nil {
		t.Fatal(err)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"RADIANCE_HOME": home}})); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "libhalo_plugin.so") {
		t.Errorf("writable plugin: %s %q", f.Severity, f.Summary)
	}
	if f := runRadianceHostPrereqs(ctx, env(fakeLaunchEnv{20: {"RADIANCE_HOME": worldWritable}})); f.Severity != SeverityWarn {
		t.Errorf("writable home: %s %q", f.Severity, f.Summary)
	}
}

type homeHost struct {
	osHostFS
	home string
}

func (h homeHost) Getenv(k string) string {
	if k == "RADIANCE_HOME" {
		return h.home
	}
	return ""
}

func TestRadianceHostPrereqs_HomeFromProcessEnvironment(t *testing.T) {
	ctx := context.Background()
	e := &CheckEnv{Catalog: radianceCat(false), Host: homeHost{home: mkdir(t, 0o777)}, CwsrPath: cwsrFile(t, "1")}
	if f := runRadianceHostPrereqs(ctx, e); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "RADIANCE_HOME") {
		t.Errorf("env RADIANCE_HOME writable: %s %q", f.Severity, f.Summary)
	}
}

func TestIsRadianceKind(t *testing.T) {
	for _, tc := range []struct {
		kind, backend string
		want          bool
	}{{"radiance", "rocm", true}, {"llamacpp", "rocm", false}, {"", "vllm", false}, {"", "", false}} {
		if got := isRadianceKind(tc.kind, tc.backend); got != tc.want {
			t.Errorf("isRadianceKind(%q,%q) = %v, want %v", tc.kind, tc.backend, got, tc.want)
		}
	}
}

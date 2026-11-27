// SPDX-License-Identifier: Apache-2.0

package smith

// checks_radiance.go — radiance_host_prereqs (Radiance phase 1b step f7). It runs only when the catalog holds at least one non-retired Radiance build
// or a config on a Radiance engine; a host without Radiance gets no finding at all (Check.Skip), so it adds no sweep noise.
//
// What it checks (each unverifiable item is reported as "cannot verify", never as a failure):
//   - /sys/module/amdgpu/parameters/cwsr_enable reads 1. The exact path is UNVERIFIED on the host; a missing file means "cannot verify".
//   - every absolute path named by a launch_env value of an active Radiance build exists and is not group/world-writable.
//   - RADIANCE_HOME (from a build's launch_env, else the process environment) and its libhalo* plugin entries are not group/world-writable.
//
// The build's launch_env column is added on another branch (phase-1b-f-core). Until it lands, BuildLaunchEnvReader is nil, the env map is empty and
// only the cwsr and process-environment RADIANCE_HOME checks run; once it lands, wiring a reader into CheckEnv.RadianceBuildEnv activates the rest.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/store"
)

// radianceKind is the stored engine kind of Radiance (enginekind will carry the constant; until it does this is the one literal).
const radianceKind = "radiance"

// defaultCwsrPath is where the amdgpu module parameter is read. UNVERIFIED on the host (see the file comment).
const defaultCwsrPath = "/sys/module/amdgpu/parameters/cwsr_enable"

// BuildLaunchEnvReader returns the launch_env map of a catalog build (variables the engine launch adds to its environment).
type BuildLaunchEnvReader interface {
	BuildLaunchEnv(buildID int64) (map[string]string, error)
}

// HostFS is the small slice of the filesystem radiance_host_prereqs reads, injectable for tests.
type HostFS interface {
	Stat(path string) (fs.FileInfo, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	Getenv(key string) string
}

type osHostFS struct{}

func (osHostFS) Stat(p string) (fs.FileInfo, error)      { return os.Stat(p) }
func (osHostFS) ReadDir(p string) ([]fs.DirEntry, error) { return os.ReadDir(p) }
func (osHostFS) Getenv(k string) string                  { return os.Getenv(k) }

// noBuildLaunchEnv is the nil-safe default: no build carries a launch environment.
type noBuildLaunchEnv struct{}

func (noBuildLaunchEnv) BuildLaunchEnv(int64) (map[string]string, error) { return nil, nil }

func (e *CheckEnv) hostFS() HostFS {
	if e.Host != nil {
		return e.Host
	}
	return osHostFS{}
}

// isRadianceKind resolves a stored engine kind (with the build's backend) through enginekind; a kind the table does not know yet is compared raw, so
// the check also works on a binary built before Radiance joined the table.
func isRadianceKind(kind, backend string) bool {
	spec, err := enginekind.Resolve(kind, backend)
	if err != nil {
		return kind == radianceKind || kind == enginekind.Radiance2
	}
	return enginekind.IsRadiance(spec.Name)
}

// radianceCatalog returns the non-retired Radiance builds and whether any Radiance config exists. The error is a catalog read error.
func radianceCatalog(ctx context.Context, cat store.Catalog) (builds []store.Build, hasConfig bool, err error) {
	engines, err := cat.ListEngines(ctx)
	if err != nil {
		return nil, false, err
	}
	kindOf := map[int64]string{}
	for _, e := range engines {
		kindOf[e.ID] = e.Kind
	}
	all, err := cat.ListBuilds(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, b := range all {
		if !b.Retired && isRadianceKind(kindOf[b.EngineID], b.Backend) {
			builds = append(builds, b)
		}
	}
	cfgs, err := cat.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, c := range cfgs {
		if isRadianceKind(kindOf[c.EngineID], "") {
			hasConfig = true
			break
		}
	}
	return builds, hasConfig, nil
}

// radianceNotInUse is Check.Skip: true (suppress) unless the catalog has a Radiance build or config. A catalog read error does not suppress (the check
// then reports it).
func radianceNotInUse(env *CheckEnv) bool {
	if env.Catalog == nil {
		return true
	}
	builds, hasConfig, err := radianceCatalog(context.Background(), env.Catalog)
	if err != nil {
		return false
	}
	return len(builds) == 0 && !hasConfig
}

// groupWorldWritable reports whether mode grants write to group or others.
func groupWorldWritable(m fs.FileMode) bool { return m.Perm()&0o022 != 0 }

func runRadianceHostPrereqs(ctx context.Context, env *CheckEnv) Finding {
	const id = "radiance_host_prereqs"
	if env.Catalog == nil {
		return skipFinding(id, "no catalog source")
	}
	builds, hasConfig, err := radianceCatalog(ctx, env.Catalog)
	if err != nil {
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: fmt.Sprintf("could not read the catalog to find Radiance builds: %v", err),
			SummaryKey: "checks.radiance_host_prereqs.catalog_error", Params: map[string]any{"error": err.Error()}}
	}
	if len(builds) == 0 && !hasConfig {
		return skipFinding(id, "no non-retired Radiance build or config in the catalog")
	}
	host := env.hostFS()
	var problems []string
	var unverified []string

	// 1. cwsr_enable
	cwsr := env.CwsrPath
	if cwsr == "" {
		cwsr = defaultCwsrPath
	}
	readParam := env.ReadFile
	if readParam == nil {
		readParam = os.ReadFile
	}
	switch raw, err := readParam(cwsr); {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		unverified = append(unverified, "amdgpu cwsr_enable (parameter file not present)")
	case err != nil:
		unverified = append(unverified, "amdgpu cwsr_enable (unreadable)")
	case strings.TrimSpace(string(raw)) != "1":
		problems = append(problems, fmt.Sprintf("amdgpu cwsr_enable is %q, Radiance needs 1", strings.TrimSpace(string(raw))))
	}

	// 2. launch_env paths of the active Radiance builds, and RADIANCE_HOME.
	reader := env.RadianceBuildEnv
	if reader == nil {
		reader = noBuildLaunchEnv{}
	}
	checkedPaths := map[string]bool{}
	home := ""
	for _, b := range builds {
		lenv, err := reader.BuildLaunchEnv(b.ID)
		if err != nil {
			unverified = append(unverified, fmt.Sprintf("launch_env of build %q (%v)", b.Name, err))
			continue
		}
		keys := make([]string, 0, len(lenv))
		for k := range lenv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "RADIANCE_HOME" && home == "" {
				home = lenv[k]
			}
			for _, p := range strings.Split(lenv[k], ":") {
				if !filepath.IsAbs(p) || checkedPaths[p] {
					continue
				}
				checkedPaths[p] = true
				info, err := host.Stat(p)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					problems = append(problems, fmt.Sprintf("build %q launch_env %s names %s, which does not exist", b.Name, k, p))
				case err != nil:
					unverified = append(unverified, fmt.Sprintf("%s (%v)", p, err))
				case groupWorldWritable(info.Mode()):
					problems = append(problems, fmt.Sprintf("%s (launch_env %s of build %q) is group- or world-writable", p, k, b.Name))
				}
			}
		}
	}
	if home == "" {
		home = host.Getenv("RADIANCE_HOME")
	}
	if home != "" {
		if info, err := host.Stat(home); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				problems = append(problems, fmt.Sprintf("RADIANCE_HOME %s does not exist", home))
			} else {
				unverified = append(unverified, fmt.Sprintf("RADIANCE_HOME %s (%v)", home, err))
			}
		} else {
			if groupWorldWritable(info.Mode()) {
				problems = append(problems, fmt.Sprintf("RADIANCE_HOME %s is group- or world-writable", home))
			}
			entries, err := host.ReadDir(home)
			if err != nil {
				unverified = append(unverified, fmt.Sprintf("RADIANCE_HOME %s plugin entries (%v)", home, err))
			}
			for _, de := range entries {
				if !strings.HasPrefix(de.Name(), "libhalo") {
					continue
				}
				p := filepath.Join(home, de.Name())
				pi, err := host.Stat(p)
				switch {
				case err != nil:
					unverified = append(unverified, fmt.Sprintf("%s (%v)", p, err))
				case groupWorldWritable(pi.Mode()):
					problems = append(problems, fmt.Sprintf("plugin %s is group- or world-writable", p))
				}
			}
		}
	}

	sort.Strings(problems)
	sort.Strings(unverified)
	ev := map[string]any{"radiance_builds": len(builds), "problems": problems, "cannot_verify": unverified, "radiance_home_set": home != ""}
	if len(problems) > 0 {
		summary := "Radiance host prerequisite not met: " + problems[0]
		if len(problems) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(problems)-1)
		}
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: summary,
			SummaryKey: "checks.radiance_host_prereqs.problem",
			Params:     map[string]any{"first": problems[0], "more_count": len(problems) - 1},
			Evidence:   ev}
	}
	if len(unverified) > 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:    fmt.Sprintf("no Radiance host prerequisite failed; %d could not be verified: %s", len(unverified), strings.Join(unverified, "; ")),
			SummaryKey: "checks.radiance_host_prereqs.unverified",
			Params:     map[string]any{"count": len(unverified), "items": strings.Join(unverified, "; ")},
			Evidence:   ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:    "Radiance host prerequisites hold (cwsr_enable, launch environment paths, plugin directory permissions)",
		SummaryKey: "checks.radiance_host_prereqs.ok",
		Evidence:   ev}
}

// SPDX-License-Identifier: Apache-2.0

package main

// forge radiance register-build: registers a built Radiance engine (the unofficial fork,
// docs/radiance-installer-and-catalog-plan.md) as a catalog build through the same guarded
// API the Settings page uses (POST /api/v1/catalog/builds: build-root guard, executable
// check, launch-environment validation), so the installer does not hand-write HTTP calls.
//
// It is idempotent: a build with the same name and the same binary and launch environment
// is reported as already registered; the same name with different contents is an error
// (a registration is never silently overwritten). With -dry-run it validates the build
// directory and prints the request, and touches neither the daemon nor the database.
//
// Authentication: FORGE_API_KEY (an admin or catalog-scope key) if set; otherwise it mints
// a catalog-scope key (10 minutes) in the state database it is pointed at, uses it, and
// revokes it before returning, so no long-lived credential is created by an install.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/cli"
	"github.com/jsaigou/the-forge/internal/store"
)

// radiancePluginKernels is the kernel library the Radiance fork serves on gfx1151 and the value
// the running host's build 25 carries; the engine needs it named to find the device kernels.
const radiancePluginKernels = "libhalo"

// catalogAPI is the part of cli.Client the registration uses, so the logic is testable against
// an httptest server without a daemon.
type catalogAPI interface {
	GetJSON(path string, v any) error
	PostJSON(path string, body, v any) error
}

type registerOutcome struct {
	ID      int64
	Name    string
	Created bool // false: an identical registration already existed
}

func runRadianceCLI(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "register-build" {
		return errors.New("usage: forge radiance register-build -path DIR -name NAME [-backend rocm] [-kind radiance|radiance2] [-db FILE] [-dry-run]")
	}
	return runRadianceRegisterBuild(args[1:], stdout, stderr)
}

func runRadianceRegisterBuild(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("radiance register-build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		path    = fs.String("path", "", "the Radiance build directory (the one holding bin/radiance and radiance_home/)")
		name    = fs.String("name", "", "the catalog build name, e.g. radiance-unofficial/strix-halo-r2-rocm-10.1")
		backend = fs.String("backend", "rocm", "the build's backend: rocm or vulkan")
		kind    = fs.String("kind", enginekind.Radiance, "the engine kind: radiance (1.17, takes flags) or radiance2 (2.x, takes a YAML file; forge slot-exec writes it at launch)")
		dbPath  = fs.String("db", "/var/lib/forge/forge.db", "state database, used only to mint the short-lived catalog key when FORGE_API_KEY is not set")
		dryRun  = fs.Bool("dry-run", false, "validate the directory and print the request; contact nothing")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *path == "" {
		return errors.New("-path and -name are required")
	}
	if !enginekind.IsRadiance(*kind) {
		return fmt.Errorf("-kind must be %s or %s (got %q)", enginekind.Radiance, enginekind.Radiance2, *kind)
	}
	if *backend != "rocm" && *backend != "vulkan" {
		return fmt.Errorf("-backend must be rocm or vulkan (got %q)", *backend)
	}
	binary, home, err := validateRadianceBuildDir(*path)
	if err != nil {
		return err
	}
	env := map[string]string{"RADIANCE_HOME": home, "RADIANCE_KERNELS": radiancePluginKernels}
	if *dryRun {
		fmt.Fprintf(stdout, "dry run: would register build %q\n  binary_path %s\n  backend     %s\n  launch_env  RADIANCE_HOME=%s RADIANCE_KERNELS=%s\n",
			*name, binary, *backend, home, radiancePluginKernels)
		return nil
	}

	client, revoke, err := scopedClient(*dbPath, authz.ScopeCatalog, "radiance-register")
	if err != nil {
		return err
	}
	defer revoke()
	out, err := registerRadianceBuild(client, *kind, *name, binary, *backend, env)
	if err != nil {
		return err
	}
	if out.Created {
		fmt.Fprintf(stdout, "registered build %q (id %d)\n", out.Name, out.ID)
	} else {
		fmt.Fprintf(stdout, "build %q is already registered (id %d); nothing changed\n", out.Name, out.ID)
	}
	return nil
}

// validateRadianceBuildDir checks what the registration depends on, before any request: the engine
// binary is a regular executable file and the plugin directory exists. It returns the absolute
// binary path and plugin directory. The API's build guard re-checks both against the build roots.
func validateRadianceBuildDir(dir string) (binary, home string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	binary = filepath.Join(abs, "bin", "radiance")
	fi, err := os.Stat(binary)
	switch {
	case err != nil:
		return "", "", fmt.Errorf("no engine binary at %s: %w", binary, err)
	case !fi.Mode().IsRegular():
		return "", "", fmt.Errorf("%s is not a regular file", binary)
	case fi.Mode().Perm()&0o111 == 0:
		return "", "", fmt.Errorf("%s is not executable", binary)
	}
	home = filepath.Join(abs, "radiance_home")
	if hi, err := os.Stat(home); err != nil || !hi.IsDir() {
		return "", "", fmt.Errorf("no plugin directory at %s (is this the build directory?)", home)
	}
	if st, err := os.Stat(filepath.Join(home, "kernels")); err != nil || !st.IsDir() {
		return "", "", fmt.Errorf("%s has no kernels directory: the build did not produce device kernels (check RAD_GPU_TARGETS)", home)
	}
	return binary, home, nil
}

type catalogEngine struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
}

type catalogBuild struct {
	ID         int64             `json:"id"`
	EngineID   int64             `json:"engine_id"`
	Name       string            `json:"name"`
	BinaryPath string            `json:"binary_path"`
	Backend    string            `json:"backend"`
	LaunchEnv  map[string]string `json:"launch_env"`
}

// registerRadianceBuild creates the catalog build, or reports the identical one that exists.
func registerRadianceBuild(api catalogAPI, kind, name, binary, backend string, env map[string]string) (registerOutcome, error) {
	var engines []catalogEngine
	if err := api.GetJSON("/api/v1/catalog/engines", &engines); err != nil {
		return registerOutcome{}, fmt.Errorf("list engines: %w", err)
	}
	var engineID int64
	for _, e := range engines {
		if e.Kind == kind {
			engineID = e.ID
		}
	}
	if engineID == 0 {
		return registerOutcome{}, fmt.Errorf("the catalog has no %q engine; this daemon is too old to register such a build", kind)
	}
	var builds []catalogBuild
	if err := api.GetJSON("/api/v1/catalog/builds", &builds); err != nil {
		return registerOutcome{}, fmt.Errorf("list builds: %w", err)
	}
	for _, b := range builds {
		if b.Name != name {
			continue
		}
		if b.EngineID == engineID && b.BinaryPath == binary && b.Backend == backend && sameEnv(b.LaunchEnv, env) {
			return registerOutcome{ID: b.ID, Name: b.Name}, nil
		}
		return registerOutcome{}, fmt.Errorf("a build named %q (id %d) is already registered with different contents (binary %s); pick another -name or retire it in Settings → Catalog → Builds", name, b.ID, b.BinaryPath)
	}
	var created catalogBuild
	body := map[string]any{
		"engine_id": engineID, "name": name, "binary_path": binary, "backend": backend,
		"reason": "registered from the command line", "retired": false, "launch_env": env,
	}
	if err := api.PostJSON("/api/v1/catalog/builds", body, &created); err != nil {
		return registerOutcome{}, fmt.Errorf("register build: %w", err)
	}
	return registerOutcome{ID: created.ID, Name: name, Created: true}, nil
}

func sameEnv(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// scopedClient returns a client for the local daemon and a function that undoes any credential it created: FORGE_API_KEY when set, otherwise a
// ten-minute key of the given scope minted in the state database and revoked afterwards.
func scopedClient(dbPath, scope, namePrefix string) (*cli.Client, func(), error) {
	base := strings.TrimRight(os.Getenv("FORGE_API_URL"), "/")
	if base == "" {
		base = cli.DefaultBaseURL
	}
	if key := os.Getenv("FORGE_API_KEY"); key != "" {
		return &cli.Client{BaseURL: base, Key: key, HTTP: &http.Client{Timeout: 30 * time.Second}}, func() {}, nil
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("no FORGE_API_KEY and the state database cannot be opened to mint a short-lived key: %w", err)
	}
	auth := authz.New(db)
	ctx := context.Background()
	token, err := auth.MintScopedKey(ctx, fmt.Sprintf("%s-%d", namePrefix, time.Now().Unix()), "", authz.RoleAdmin, "", time.Now().Add(10*time.Minute), scope)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("mint the short-lived %s key: %w", scope, err)
	}
	_, keyid, _, err := authz.ParseToken(token)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("parse the minted key: %w", err)
	}
	revoke := func() {
		if err := auth.RevokeKey(ctx, keyid); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not revoke the temporary key %s (it expires in 10 minutes): %v\n", keyid, err)
		}
		db.Close()
	}
	return &cli.Client{BaseURL: base, Key: token, HTTP: &http.Client{Timeout: 30 * time.Second}}, revoke, nil
}

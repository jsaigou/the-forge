// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
)

// baseCandidate returns a systemSettingsResponse with every field filled to
// something structurally valid, so a test can override just the field it
// cares about without every other check firing spuriously.
func baseCandidate(t *testing.T) systemSettingsResponse {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return systemSettingsResponse{
		Listen: ":15000", RouterListen: ":15001", MCPListen: ":15002",
		DBPath: filepath.Join(dir, "forge.db"), TTSUnit: "forge-tts",
		ModelsDir: dir, SysconfigDir: dir, StateDir: dir, IconsDir: dir,
		VulkanBin: bin, RocmBin: bin,
		Ports:    map[string]int{},
		Hostname: "forgehost.example.ts.net",
	}
}

func checkLevel(checks []preflightCheck, field string) string {
	for _, c := range checks {
		if c.Field == field {
			return c.Level
		}
	}
	return "<missing>"
}

func TestPreflightSystemDirsAndBinaries(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	cand := baseCandidate(t)
	checks, fail := s.preflightSystem(context.Background(), cand)
	if len(fail) != 0 {
		t.Fatalf("expected an all-valid candidate to pass, got failures: %v (checks=%+v)", fail, checks)
	}
	for _, field := range []string{"models_dir", "sysconfig_dir", "state_dir", "vulkan_bin", "rocm_bin"} {
		if lvl := checkLevel(checks, field); lvl != "ok" {
			t.Errorf("%s level = %q, want ok", field, lvl)
		}
	}

	// Nonexistent directory.
	bad := cand
	bad.ModelsDir = "/nonexistent/definitely-not-real-path"
	_, fail = s.preflightSystem(context.Background(), bad)
	if fail["models_dir"] == "" {
		t.Error("expected models_dir failure for a nonexistent path")
	}

	// A file where a directory is expected.
	bad = cand
	bad.SysconfigDir = cand.VulkanBin // a real file, not a dir
	_, fail = s.preflightSystem(context.Background(), bad)
	if fail["sysconfig_dir"] == "" {
		t.Error("expected sysconfig_dir failure when the path is a file, not a dir")
	}

	// A non-executable "binary".
	notExec := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad = cand
	bad.VulkanBin = notExec
	_, fail = s.preflightSystem(context.Background(), bad)
	if fail["vulkan_bin"] == "" {
		t.Error("expected vulkan_bin failure for a non-executable file")
	}

	// Empty icons_dir produces no check at all — it's a known-legitimate
	// unset state (confirmed empty on ForgeHost itself), not a warn or error.
	optional := cand
	optional.IconsDir = ""
	checks, fail = s.preflightSystem(context.Background(), optional)
	if fail["icons_dir"] != "" {
		t.Error("empty icons_dir must not fail")
	}
	if lvl := checkLevel(checks, "icons_dir"); lvl != "<missing>" {
		t.Errorf("icons_dir level = %q, want no check row at all for an unset optional field", lvl)
	}
}

func TestPreflightSystemStateDirWriteProbeIsSelfCleaning(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	cand := baseCandidate(t)

	_, fail := s.preflightSystem(context.Background(), cand)
	if fail["state_dir"] != "" {
		t.Fatalf("expected a writable state_dir to pass, got: %v", fail)
	}
	// The probe file must not survive the check.
	entries, err := os.ReadDir(cand.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forge-preflight") {
			t.Errorf("preflight left a stray file behind: %s", e.Name())
		}
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root — permission-denied probe is not meaningful")
	}
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	bad := cand
	bad.StateDir = roDir
	_, fail = s.preflightSystem(context.Background(), bad)
	if fail["state_dir"] == "" {
		t.Error("expected state_dir failure for a read-only directory")
	}
}

func TestPreflightSystemListenAddr(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	cand := baseCandidate(t)

	// Unchanged from current (both default-zero, so "" == "" — resolvedSystemSettings
	// applies defaults, so compare against the real default instead).
	current := s.resolvedSystemSettings(context.Background())
	same := cand
	same.Listen = current.Listen
	checks, fail := s.preflightSystem(context.Background(), same)
	if fail["listen"] != "" {
		t.Errorf("unchanged listen address should not be probed/fail: %v", fail)
	}
	if lvl := checkLevel(checks, "listen"); lvl != "ok" {
		t.Errorf("unchanged listen level = %q, want ok", lvl)
	}

	// A changed address to something free should pass with a real bind probe.
	free := cand
	free.RouterListen = ":0" // ":0" always binds to an ephemeral free port
	_, fail = s.preflightSystem(context.Background(), free)
	if fail["router_listen"] != "" {
		t.Errorf("a free changed address should pass: %v", fail)
	}

	// A changed address already held by a real listener should fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := cand
	held.MCPListen = ln.Addr().String()
	_, fail = s.preflightSystem(context.Background(), held)
	if fail["mcp_listen"] == "" {
		t.Error("expected mcp_listen failure for an address already bound elsewhere")
	}

	// Empty address is always an error, regardless of "unchanged" logic.
	empty := cand
	empty.Listen = ""
	_, fail = s.preflightSystem(context.Background(), empty)
	if fail["listen"] == "" {
		t.Error("expected listen failure for an empty address")
	}
}

// TestIsSelfWildcardNarrowing covers the sprint-4 fix directly against the
// decision function: narrowing an already-bound wildcard host to a specific
// one on the SAME port is expected self-collision (the daemon's own running
// wildcard socket already occupies it, even though restarting would free it
// cleanly) and must warn, not error/422 — while a genuine conflict (a
// different port, a non-wildcard current bind, or a non-EADDRINUSE failure)
// must not be reclassified.
//
// A live-socket reproduction was tried first and dropped: whether binding a
// specific address alongside an already-bound wildcard on the same port
// actually collides is platform-dependent — it does on this repo's real
// Linux deployment (verified live on ForgeHost, sprint 4) but does not on
// macOS's BSD-derived stack (verified directly: net.Listen("tcp4", ":0")
// followed by net.Listen("tcp", "127.0.0.1:<same port>") succeeds on
// darwin). Testing the pure decision function is deterministic and
// platform-independent; the actual wiring is proven live against the real
// target OS.
func TestIsSelfWildcardNarrowing(t *testing.T) {
	addrInUse := &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}}
	otherErr := &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EACCES}}

	cases := []struct {
		name     string
		cur, cnd string
		err      error
		want     bool
	}{
		{"narrowing wildcard on same port", ":5000", "127.0.0.1:5000", addrInUse, true},
		{"narrowing 0.0.0.0 form on same port", "0.0.0.0:5000", "127.0.0.1:5000", addrInUse, true},
		{"different port is a real conflict", ":5000", "127.0.0.1:5001", addrInUse, false},
		{"current already specific is a real conflict", "127.0.0.1:5000", "127.0.0.2:5000", addrInUse, false},
		{"candidate also wildcard is not narrowing", ":5000", "0.0.0.0:5000", addrInUse, false},
		{"non-EADDRINUSE error is never reclassified", ":5000", "127.0.0.1:5000", otherErr, false},
		{"unparseable addresses fall through", ":5000", "not-an-addr", addrInUse, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSelfWildcardNarrowing(c.cur, c.cnd, c.err); got != c.want {
				t.Errorf("isSelfWildcardNarrowing(%q, %q, %v) = %v, want %v", c.cur, c.cnd, c.err, got, c.want)
			}
		})
	}
}

func TestPreflightSystemPortCollision(t *testing.T) {
	// serverWithSettings' fixture config has slot "a1" on port 8080.
	s := serverWithSettings(t, newFakeSettings())
	cand := baseCandidate(t)

	collide := cand
	collide.Ports = map[string]int{"embedding": 8080} // collides with slot a1
	_, fail := s.preflightSystem(context.Background(), collide)
	if fail["ports"] == "" {
		t.Error("expected a ports failure for a port colliding with a real slot")
	}

	clean := cand
	clean.Ports = map[string]int{"embedding": 18083, "stt": 18084}
	checks, fail := s.preflightSystem(context.Background(), clean)
	if fail["ports"] != "" {
		t.Errorf("expected non-colliding ports to pass, got: %v", fail)
	}
	if lvl := checkLevel(checks, "ports"); lvl != "ok" {
		t.Errorf("ports level = %q, want ok", lvl)
	}
}

func TestPreflightSystemDBPath(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	cand := baseCandidate(t)

	missing := cand
	missing.DBPath = "/nonexistent/nowhere/forge.db"
	_, fail := s.preflightSystem(context.Background(), missing)
	if fail["db_path"] == "" {
		t.Error("expected db_path failure when the parent directory doesn't exist")
	}

	// A non-sqlite file at the target path is a warn, not an error.
	dir := t.TempDir()
	fakeDB := filepath.Join(dir, "forge.db")
	if err := os.WriteFile(fakeDB, []byte("not a real sqlite file"), 0o644); err != nil {
		t.Fatal(err)
	}
	warn := cand
	warn.DBPath = fakeDB
	checks, fail := s.preflightSystem(context.Background(), warn)
	if fail["db_path"] != "" {
		t.Errorf("a non-sqlite existing file should warn, not fail: %v", fail)
	}
	if lvl := checkLevel(checks, "db_path"); lvl != "warn" {
		t.Errorf("db_path level = %q, want warn", lvl)
	}
}

func TestCheckTrustedCIDRLockout(t *testing.T) {
	check := checkTrustedCIDRLockout("10.0.0.5:54321", "10.0.0.0/24, 192.168.1.0/24")
	if check == nil || check.Level != "ok" {
		t.Errorf("caller's address is covered, expected ok, got %+v", check)
	}

	check = checkTrustedCIDRLockout("172.16.0.5:54321", "10.0.0.0/24, 192.168.1.0/24")
	if check == nil || check.Level != "error" {
		t.Errorf("caller's address is NOT covered, expected error, got %+v", check)
	}

	// Malformed remote addr (no parseable IP) — don't block, nothing to check.
	if check := checkTrustedCIDRLockout("not-an-address", "10.0.0.0/24"); check != nil {
		t.Errorf("unparseable remote addr should return nil (skip), got %+v", check)
	}
}

func TestAuthConfigPutRejectsLockoutCIDR(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	// Set forward_auth_header as the active provider first.
	w := do(t, s, authedRequest("PUT", "/api/v1/auth/config", strings.NewReader(`{"network_provider":"forward_auth_header"}`)))
	if w.Code != 200 {
		t.Fatalf("set network_provider = %d, body=%s", w.Code, w.Body)
	}

	// authedRequest's httptest.NewRequest gives a synthetic RemoteAddr
	// (192.0.2.1:1234, TEST-NET-1 — see net/http/httptest docs) that a
	// trusted_cidrs value excluding it must be rejected for.
	w = do(t, s, authedRequest("PUT", "/api/v1/auth/config",
		strings.NewReader(`{"provider_config":{"trusted_cidrs":"10.0.0.0/8"}}`)))
	if w.Code != 422 {
		t.Fatalf("lockout CIDR = %d, want 422, body=%s", w.Code, w.Body)
	}

	// A CIDR that covers the test's own synthetic address should be accepted.
	w = do(t, s, authedRequest("PUT", "/api/v1/auth/config",
		strings.NewReader(`{"provider_config":{"trusted_cidrs":"192.0.2.0/24"}}`)))
	if w.Code != 200 {
		t.Fatalf("non-lockout CIDR = %d, want 200, body=%s", w.Code, w.Body)
	}
}

func TestSystemPreflightPostDryRunWritesNothing(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("POST", "/api/v1/system/preflight", strings.NewReader(`{"models_dir":"/nonexistent/nowhere"}`)))
	if w.Code != 200 {
		t.Fatalf("POST system/preflight = %d, body=%s", w.Code, w.Body)
	}
	var resp struct {
		OK     bool             `json:"ok"`
		Checks []preflightCheck `json:"checks"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.OK {
		t.Error("expected ok=false for a nonexistent models_dir")
	}
	if _, err := set.Get(context.Background(), "infra.paths"); err == nil {
		t.Error("POST /system/preflight must not write infra.paths — it's a dry run")
	}
}

func TestSystemSettingsPutRejectsFailingPreflight(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("PUT", "/api/v1/system/settings", strings.NewReader(`{"models_dir":"/nonexistent/nowhere-real"}`)))
	if w.Code != 422 {
		t.Fatalf("PUT with a failing preflight = %d, want 422, body=%s", w.Code, w.Body)
	}
	var resp struct {
		Error  string            `json:"error"`
		Fields map[string]string `json:"fields"`
		Checks []preflightCheck  `json:"checks"`
	}
	decodeJSON(t, w.Body, &resp)
	if resp.Error != "preflight_failed" {
		t.Errorf("error = %q, want preflight_failed", resp.Error)
	}
	if resp.Fields["models_dir"] == "" {
		t.Error("expected models_dir in the failure fields")
	}
	// Nothing should have been written to the store.
	if _, err := set.Get(context.Background(), "infra.paths"); err == nil {
		t.Error("a failed preflight must not persist infra.paths")
	}
}

// newServerWithRestart builds a Server with Deps.SystemRestart wired to
// restartFn — serverWithSettings (httpapi_test.go) doesn't expose that
// field, so this mirrors its construction with the one extra Deps value
// this file's restart tests need.
func newServerWithRestart(t *testing.T, restartFn func(context.Context) error) *Server {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{
		Server: config.Server{Listen: ":0"},
		Slots: map[string]config.Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
		},
	})
	s := New(Deps{
		Snapshots:     collector.NewStatic(nil),
		Engine:        &engine.Stub{},
		Sched:         &sched.Stub{},
		Auth:          &stubAuth{identity: authz.Identity{Name: "operator", Role: authz.RoleAdmin}},
		Events:        events,
		Publish:       events,
		Config:        func() *config.Config { return cfg },
		Settings:      newFakeSettings(),
		Hostname:      "test-host",
		SystemRestart: restartFn,
	})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHandleSystemRestart(t *testing.T) {
	called := make(chan struct{}, 1)
	restartFn := func(ctx context.Context) error {
		called <- struct{}{}
		return nil
	}
	s := newServerWithRestart(t, restartFn)

	w := do(t, s, authedRequest("POST", "/api/v1/system/restart", nil))
	if w.Code != 202 {
		t.Fatalf("POST system/restart = %d, body=%s", w.Code, w.Body)
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if resp["unit"] != "forge-daemon.service" {
		t.Errorf("unit = %v, want forge-daemon.service", resp["unit"])
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("SystemRestart was not called within 2s of the 202 response")
	}
}

func TestHandleSystemRestartUnavailable(t *testing.T) {
	s := newTestServer(t) // no SystemRestart wired
	w := do(t, s, authedRequest("POST", "/api/v1/system/restart", nil))
	if w.Code != 503 {
		t.Fatalf("POST system/restart with no SystemRestart wired = %d, want 503", w.Code)
	}
}

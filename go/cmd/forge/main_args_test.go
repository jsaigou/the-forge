// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain_UnknownSubcommandDoesNotBoot guards the 2026-08-18 regression: an
// unrecognized positional argument (e.g. a `version` typo before it became a
// real subcommand) used to fall straight through flag.Parse() — which never
// errors on unrecognized non-flag args — into main() booting a full daemon
// against whatever DB/config it found, live-reconciling and restarting real
// systemd units on an already-running host. Run as a real subprocess (the
// GO_WANT_FORGE_MAIN env-var re-exec trick) so this exercises the actual
// main() exit path, not a unit stub of it. The target argument is passed via
// FORGE_TEST_ARG rather than appended to the test binary's own argv,
// since `go test`'s generated main already parses os.Args itself and a
// second positional arg there would collide with its own flags.
func TestMain_UnknownSubcommandDoesNotBoot(t *testing.T) {
	if os.Getenv("GO_WANT_FORGE_MAIN") == "1" {
		os.Args = []string{"forge", os.Getenv("FORGE_TEST_ARG")}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMain_UnknownSubcommandDoesNotBoot$")
	cmd.Env = append(os.Environ(), "GO_WANT_FORGE_MAIN=1", "FORGE_TEST_ARG=frobnicate")
	out, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("forge frobnicate: err=%v out=%s, want exit code 2", err, out)
	}
	if !strings.Contains(string(out), "unrecognized argument") {
		t.Errorf("output = %q, want a message about the unrecognized argument", out)
	}
}

// TestMain_VersionSubcommandExitsCleanly confirms `forge version` (the
// alias added alongside the fix above) prints the version and exits 0
// without falling through to boot.
func TestMain_VersionSubcommandExitsCleanly(t *testing.T) {
	if os.Getenv("GO_WANT_FORGE_MAIN") == "1" {
		os.Args = []string{"forge", os.Getenv("FORGE_TEST_ARG")}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMain_VersionSubcommandExitsCleanly$")
	cmd.Env = append(os.Environ(), "GO_WANT_FORGE_MAIN=1", "FORGE_TEST_ARG=version")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Fatalf("forge version: err=%v out=%s, want exit code 0", err, out)
	}
	if !strings.Contains(string(out), "forge") {
		t.Errorf("output = %q, want it to include the binary name + version", out)
	}
}

// SPDX-License-Identifier: Apache-2.0

package slotexec_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

const canaryValue = "INTEGRATION-CANARY-5521"

func sumFiles(t *testing.T, paths ...string) [][32]byte {
	t.Helper()
	var out [][32]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out = append(out, sha256.Sum256(b))
	}
	return out
}

// TestRealBinaryExecsStub builds the real forge binary, writes a row pointing at a stub script, runs `forge slot-exec` as a subprocess and checks that
// the stub ran in the same process (exec, not fork), saw exactly the row's argv and variables, did not see the inherited FORGE_*/FOUNDRY_* ones, and
// that the database was not modified.
func TestRealBinaryExecsStub(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the forge binary")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "forge")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/forge")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	buildRoot := filepath.Join(dir, "builds")
	if err := os.MkdirAll(buildRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report")
	stub := filepath.Join(buildRoot, "engine.sh")
	script := "#!/bin/sh\n" +
		"{ echo \"pid=$$\"; echo \"arg0=$0\"; for a in \"$@\"; do echo \"arg=$a\"; done;\n" +
		"  echo \"row=$ROW_VAR\"; echo \"forge=${FORGE_API_KEY-unset}\"; echo \"foundry=${FOUNDRY_X-unset}\"; echo \"path=$PATH\"; pwd; } > " + report + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, "forge.db")
	ctx := context.Background()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Settings().Set(ctx, "catalog.build_roots", []byte(`["`+buildRoot+`"]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SlotLaunches().Put(ctx, store.SlotLaunch{
		Slot: "a1", ConfigName: "cfg", EngineKind: "llamacpp", Exe: stub, Cwd: buildRoot,
		Argv: []string{"--port", "8081", "two words", "--x=y"}, Env: map[string]string{"ROW_VAR": canaryValue},
		Port: 8081, ModelPath: "/m", WrittenBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	// Close checkpoints the WAL on the last connection; hash afterwards so the comparison covers the settled state.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := sumFiles(t, dbPath, dbPath+"-wal")

	run := func(args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		c := exec.Command(bin, args...)
		c.Env = []string{"PATH=/usr/bin:/bin", "FORGE_API_KEY=" + canaryValue + "-inherited", "FOUNDRY_X=1"}
		var so, se bytes.Buffer
		c.Stdout, c.Stderr = &so, &se
		return c, &so, &se
	}

	t.Run("dry run", func(t *testing.T) {
		c, so, se := run("slot-exec", "a1", "--dry-run", "-db", dbPath)
		if err := c.Run(); err != nil {
			t.Fatalf("%v\nstderr: %s", err, se.String())
		}
		if strings.Contains(so.String()+se.String(), canaryValue) {
			t.Errorf("a value leaked:\n%s%s", so.String(), se.String())
		}
		if !strings.Contains(so.String(), "ROW_VAR") {
			t.Errorf("dry-run lacks the variable name:\n%s", so.String())
		}
		if _, err := os.Stat(report); err == nil {
			t.Error("dry run executed the stub")
		}
	})

	t.Run("exec", func(t *testing.T) {
		c, so, se := run("slot-exec", "a1", "-db", dbPath)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		pid := c.Process.Pid
		if err := c.Wait(); err != nil {
			t.Fatalf("%v\nstderr: %s", err, se.String())
		}
		if strings.Contains(so.String()+se.String(), canaryValue) {
			t.Errorf("a value leaked into output:\n%s%s", so.String(), se.String())
		}
		raw, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		got := string(raw)
		for _, want := range []string{
			"pid=" + strconv.Itoa(pid) + "\n", // exec, not fork: the stub is the process slot-exec started as
			"arg0=" + stub + "\n",
			"arg=--port\narg=8081\narg=two words\narg=--x=y\n",
			"row=" + canaryValue + "\n",
			"forge=unset\n",
			"foundry=unset\n",
			"path=/usr/bin:/bin\n",
			buildRoot + "\n", // pwd
		} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q:\n%s", want, got)
			}
		}
	})

	t.Run("refused rows exit with their code and never run the stub", func(t *testing.T) {
		_ = os.Remove(report)
		c, _, se := run("slot-exec", "a2", "-db", dbPath)
		err := c.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 66 {
			t.Fatalf("err = %v, want exit 66\n%s", err, se.String())
		}
		c, _, _ = run("slot-exec", "a1", "-db", filepath.Join(dir, "absent.db"))
		err = c.Run()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 69 {
			t.Fatalf("missing db: err = %v, want exit 69", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "absent.db")); err == nil {
			t.Error("slot-exec created a database file")
		}
		if _, err := os.Stat(report); err == nil {
			t.Error("stub ran")
		}
	})

	after := sumFiles(t, dbPath, dbPath+"-wal")
	if before[0] != after[0] || before[1] != after[1] {
		t.Error("slot-exec modified the database or its WAL")
	}
}

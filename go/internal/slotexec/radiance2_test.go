// SPDX-License-Identifier: Apache-2.0

package slotexec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/enginekind"
)

// radiance2Row makes the harness row a Radiance 2.x slot: the stored form is the flag list, exactly as the 1.17 builder writes it.
func radiance2Row(h *harness) {
	h.db.row.EngineKind = enginekind.Radiance2
	h.db.row.Argv = []string{"--model", "/m/q.rad", "--served-model-name", "qwen36", "--host", "127.0.0.1", "--port", "8081", "--max-model-len", "262144",
		"--max-num-seqs", "4", "--num-speculative-tokens", "2", "--kv-cache-dtype", "i8", "--tp", "1"}
}

type cfgCall struct {
	dir, name string
	data      string
}

const wantYAML = `server:
  host: 127.0.0.1
  port: 8081
models:
  qwen36:
    model: /m/q.rad
    max-model-len: 262144
    max-num-seqs: 4
    num-speculative-tokens: 2
    kv-cache-dtype: i8
    tp: 1
`

// A 2.x slot is launched as `radiance --config FILE` with the settings file written first; the row's flags never reach the engine.
func TestRadiance2LaunchWritesSettingsAndExecsWithConfig(t *testing.T) {
	h, _ := newHarness(t)
	radiance2Row(h)
	var calls []cfgCall
	d := h.deps()
	d.ConfigDir = "/var/lib/forge/slot-config"
	d.WriteConfig = func(dir, name string, data []byte) (string, error) {
		calls = append(calls, cfgCall{dir, name, string(data)})
		return filepath.Join(dir, name), nil
	}
	if code := Run([]string{"a1"}, d); code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if len(calls) != 1 || calls[0].dir != "/var/lib/forge/slot-config" || calls[0].name != "a1.yaml" || calls[0].data != wantYAML {
		t.Fatalf("settings file calls = %+v", calls)
	}
	if len(h.calls) != 1 {
		t.Fatalf("exec calls = %d", len(h.calls))
	}
	want := []string{h.db.row.Exe, "--config", "/var/lib/forge/slot-config/a1.yaml"}
	if !reflect.DeepEqual(h.calls[0].argv, want) {
		t.Errorf("exec argv = %v, want %v", h.calls[0].argv, want)
	}
	if !strings.Contains(h.stderr.String(), "engine settings file /var/lib/forge/slot-config/a1.yaml written") {
		t.Errorf("the written file is not logged: %s", h.stderr.String())
	}
}

// --dry-run shows what would be written and writes nothing and execs nothing.
func TestRadiance2DryRunPrintsTheSettingsAndWritesNothing(t *testing.T) {
	h, _ := newHarness(t)
	radiance2Row(h)
	d := h.deps()
	d.WriteConfig = func(string, string, []byte) (string, error) { t.Fatal("dry run wrote a file"); return "", nil }
	if code := Run([]string{"a1", "--dry-run"}, d); code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), wantYAML) || !strings.Contains(h.stdout.String(), DefaultConfigDir+"/a1.yaml") {
		t.Errorf("dry run output lacks the settings file or its path:\n%s", h.stdout.String())
	}
	if len(h.calls) != 0 {
		t.Error("dry run exec'd")
	}
}

// A row whose flags cannot be written as a settings file is a bad row (permanent: the unit does not restart on 78).
func TestRadiance2UnconvertibleRowIsABadRow(t *testing.T) {
	h, _ := newHarness(t)
	radiance2Row(h)
	h.db.row.Argv = append(h.db.row.Argv, "--seed=1")
	d := h.deps()
	d.WriteConfig = func(string, string, []byte) (string, error) { t.Fatal("wrote a file for a bad row"); return "", nil }
	if code := Run([]string{"a1"}, d); code != ExitBadRow {
		t.Fatalf("exit %d, want %d; stderr: %s", code, ExitBadRow, h.stderr.String())
	}
	if len(h.calls) != 0 {
		t.Error("exec'd a row that could not be converted")
	}
}

// A settings file that cannot be written is a host problem the unit may retry (69), and the engine is not started without it.
func TestRadiance2WriteFailureIsRetryable(t *testing.T) {
	h, _ := newHarness(t)
	radiance2Row(h)
	d := h.deps()
	d.WriteConfig = func(string, string, []byte) (string, error) { return "", errors.New("no space left on device") }
	if code := Run([]string{"a1"}, d); code != ExitUnavailable {
		t.Fatalf("exit %d, want %d; stderr: %s", code, ExitUnavailable, h.stderr.String())
	}
	if len(h.calls) != 0 {
		t.Error("exec'd without a settings file")
	}
	if !strings.Contains(h.stderr.String(), "no space left on device") {
		t.Errorf("the cause is not reported: %s", h.stderr.String())
	}
}

func TestRadiance2WithoutAWriterIsRetryableNotAPanic(t *testing.T) {
	h, _ := newHarness(t)
	radiance2Row(h)
	if code := Run([]string{"a1"}, h.deps()); code != ExitUnavailable {
		t.Fatalf("exit %d, want %d", code, ExitUnavailable)
	}
}

// Every other kind is launched exactly as before: the flags are exec'd unchanged and no settings file is ever written.
func TestOtherKindsNeverWriteASettingsFile(t *testing.T) {
	for _, kind := range []string{"", enginekind.LlamaCpp, enginekind.VLLM, enginekind.Radiance} {
		h, _ := newHarness(t)
		h.db.row.EngineKind = kind
		d := h.deps()
		d.WriteConfig = func(string, string, []byte) (string, error) {
			t.Fatalf("kind %q wrote a settings file", kind)
			return "", nil
		}
		if code := Run([]string{"a1"}, d); code != ExitOK {
			t.Fatalf("kind %q: exit %d, stderr: %s", kind, code, h.stderr.String())
		}
		if got := h.calls[0].argv[1:]; !reflect.DeepEqual(got, h.db.row.Argv) {
			t.Errorf("kind %q: argv changed: %v", kind, got)
		}
	}
}

// The real writer: owner-only, atomic replace, no stray temporary files, the directory created.
func TestWriteConfigFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "slot-config")
	p, err := writeConfigFile(dir, "a1.yaml", []byte("one\n"))
	if err != nil || p != filepath.Join(dir, "a1.yaml") {
		t.Fatalf("path %q, err %v", p, err)
	}
	if _, err := writeConfigFile(dir, "a1.yaml", []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "two\n" {
		t.Errorf("content = %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("stray files left: %v", ents)
	}
	if _, err := writeConfigFile("/proc/forge-cannot-write-here", "x", []byte("y")); err == nil {
		t.Error("an unwritable directory must be an error")
	}
}

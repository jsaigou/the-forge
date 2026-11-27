package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// Radiance 2.x takes only --config FILE, so its port is in the settings file Forge writes. Without reading it, no 2.x slot process was
// attributed to its slot: its resident memory was missing from the memory budget (2026-11-27: 30 GiB for Flash-Next).
func TestPortArgReadsRadianceSettingsFile(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "a1.yaml")
	if err := os.WriteFile(cfg, []byte("server:\n  host: 127.0.0.1\n  port: 8080\nmodels:\n  m:\n    model: /m.rad\n    port: 9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := func(pid, cmdline string) {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc("1", "/opt/radiance/bin/radiance\x00--config\x00"+cfg+"\x00")
	proc("2", "radiance\x00--config\x00/nonexistent.yaml\x00")
	proc("3", "llama-server\x00--port\x008081\x00")
	p := Proc{Root: root}
	if port, ok := p.PortArg(1); !ok || port != 8080 {
		t.Errorf("settings file: port = %d, %v; want 8080 from the server section", port, ok)
	}
	if port, ok := p.PortArg(2); ok {
		t.Errorf("unreadable settings file: port = %d, want none", port)
	}
	if port, ok := p.PortArg(3); !ok || port != 8081 {
		t.Errorf("--port: port = %d, %v; want 8081", port, ok)
	}
}

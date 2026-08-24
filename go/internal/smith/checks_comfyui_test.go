// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/smith/comfyui"
)

func TestComfyUIHealth_RegisteredFast(t *testing.T) {
	c := findCheck(t, "comfyui_health")
	if !c.Fast {
		t.Error("comfyui_health must be a fast check (cheap unit+port probe, no HTTP)")
	}
}

func TestComfyUIPrune_RegisteredDeepOnly(t *testing.T) {
	c := findCheck(t, "comfyui_prune")
	if c.Fast {
		t.Error("comfyui_prune must be deep-sweep only — BuildMap makes real HTTP calls + a full disk walk")
	}
}

func TestComfyUIHealth_Disabled(t *testing.T) {
	f := runComfyUIHealth(context.Background(), &CheckEnv{ComfyUIEnabled: false})
	if f.Severity != "" || f.CheckID != "" || f.Summary != "" {
		t.Errorf("f = %+v, want a zero-value finding (suppressed when disabled — no skip finding)", f)
	}
}

func TestComfyUIHealth_UnitInactive(t *testing.T) {
	env := &CheckEnv{
		ComfyUIEnabled: true,
		ComfyUIUnit:    "ai-mode-comfyui",
		Snap: &collector.Snapshot{Units: map[string]collector.UnitState{
			"ai-mode-comfyui": {ActiveState: "inactive"},
		}},
	}
	f := runComfyUIHealth(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (unreachable, not urgent)", f.Severity)
	}
}

func TestComfyUIHealth_UnitActiveNoPortConfigured(t *testing.T) {
	env := &CheckEnv{
		ComfyUIEnabled: true,
		ComfyUIUnit:    "ai-mode-comfyui",
		Snap: &collector.Snapshot{Units: map[string]collector.UnitState{
			"ai-mode-comfyui": {ActiveState: "active"},
		}},
	}
	f := runComfyUIHealth(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok", f.Severity)
	}
}

func TestComfyUIHealth_PortDown(t *testing.T) {
	env := &CheckEnv{
		ComfyUIEnabled: true,
		ComfyUIPort:    3001,
		Dial:           func(int) bool { return false },
	}
	f := runComfyUIHealth(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info", f.Severity)
	}
}

// fakeComfyClient is a minimal comfyui.Client double for the smith-level
// check tests — the comfyui package's own tests (map_test.go) cover
// BuildMap's internal logic exhaustively; these only prove runComfyUIPrune
// wires a MapResult into a Finding correctly.
type fakeComfyClient struct {
	objectInfoErr error
}

func (f *fakeComfyClient) Healthy(context.Context) bool { return f.objectInfoErr == nil }
func (f *fakeComfyClient) Queue(context.Context) (comfyui.QueueResponse, error) {
	return comfyui.QueueResponse{}, nil
}
func (f *fakeComfyClient) History(context.Context) (map[string]comfyui.HistoryEntry, error) {
	return nil, nil
}
func (f *fakeComfyClient) ObjectInfo(context.Context) (map[string]comfyui.ObjectInfoEntry, error) {
	if f.objectInfoErr != nil {
		return nil, f.objectInfoErr
	}
	return map[string]comfyui.ObjectInfoEntry{}, nil
}

func TestComfyUIPrune_Disabled(t *testing.T) {
	f := runComfyUIPrune(context.Background(), &CheckEnv{ComfyUIEnabled: false})
	if f.Severity != "" || f.CheckID != "" || f.Summary != "" {
		t.Errorf("f = %+v, want a zero-value finding (suppressed when disabled — no skip finding)", f)
	}
}

func TestComfyUIChecks_SkippedAtDispatchWhenDisabled(t *testing.T) {
	env := &CheckEnv{ComfyUIEnabled: false}
	enabledChecks := []Check{
		{ID: "comfyui_health", Run: runComfyUIHealth, Skip: func(e *CheckEnv) bool { return !e.ComfyUIEnabled }},
		{ID: "comfyui_prune", Run: runComfyUIPrune, Skip: func(e *CheckEnv) bool { return !e.ComfyUIEnabled }},
	}
	findings := runSelected(context.Background(), enabledChecks, env)
	if len(findings) != 0 {
		t.Errorf("findings = %d, want 0 (disabled checks must not produce any finding at dispatch)", len(findings))
	}
}

func TestComfyUIPrune_NilClient(t *testing.T) {
	f := runComfyUIPrune(context.Background(), &CheckEnv{ComfyUIEnabled: true})
	if f.Evidence["skipped"] == nil {
		t.Errorf("f = %+v, want a skip finding (no client wired)", f)
	}
}

func TestComfyUIPrune_NoModelRoots(t *testing.T) {
	env := &CheckEnv{ComfyUIEnabled: true, ComfyUI: &fakeComfyClient{}}
	f := runComfyUIPrune(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info", f.Severity)
	}
}

func TestComfyUIPrune_Unbuildable_SurfacesAsWarn(t *testing.T) {
	env := &CheckEnv{
		ComfyUIEnabled:    true,
		ComfyUI:           &fakeComfyClient{objectInfoErr: errors.New("connection refused")},
		ComfyUIModelRoots: []string{t.TempDir()},
	}
	f := runComfyUIPrune(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %s, want warn (an unbuildable map is worth operator attention)", f.Severity)
	}
	if len(f.KBRefs) == 0 {
		t.Error("expected a kb_ref pointing at the guardrails corpus chunk")
	}
}

func TestComfyUIPrune_CandidatesFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "w.json"),
		[]byte(`{"nodes": [{"type": "CheckpointLoaderSimple", "widgets_values": ["used.safetensors"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mustWriteModelFileForCheckTest(t, root, "checkpoints", "used.safetensors", 10)
	mustWriteModelFileForCheckTest(t, root, "checkpoints", "unused.safetensors", 20)

	env := &CheckEnv{
		ComfyUIEnabled:      true,
		ComfyUI:             &fakeComfyClient{},
		ComfyUIModelRoots:   []string{root},
		ComfyUIWorkflowDirs: []string{dir},
	}
	f := runComfyUIPrune(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (candidates found, not urgent)", f.Severity)
	}
}

func mustWriteModelFileForCheckTest(t *testing.T, root, folderType, name string, size int) {
	t.Helper()
	full := filepath.Join(root, folderType, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

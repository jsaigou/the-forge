// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"encoding/json"
	"testing"

	"github.com/jsaigou/the-forge/internal/smith/comfyui"
)

func TestProposeComfyUIDelete_BundlesAllCandidatesIntoOneAction(t *testing.T) {
	f := Finding{
		CheckID: "comfyui_prune", Severity: SeverityInfo,
		Evidence: map[string]any{"candidates": []comfyui.FileInfo{
			{FolderType: "checkpoints", RelPath: "a.safetensors", FullPath: "/root/checkpoints/a.safetensors", SizeBytes: 100},
			{FolderType: "loras", RelPath: "b.safetensors", FullPath: "/root/loras/b.safetensors", SizeBytes: 200},
		}},
	}
	drafts := proposeComfyUIDelete(&CheckEnv{}, f, BrainResolution{})
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want exactly 1 (bundled)", len(drafts))
	}
	d := drafts[0]
	if d.Kind != KindDeleteFiles {
		t.Errorf("kind = %s, want delete_files", d.Kind)
	}
	if d.Risk != RiskHigh {
		t.Errorf("risk = %s, want high — delete_files must always be high risk", d.Risk)
	}
	var detail deleteFilesDetail
	if err := json.Unmarshal(d.Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if len(detail.Files) != 2 || detail.TotalBytes != 300 {
		t.Errorf("detail = %+v, want 2 files totalling 300 bytes", detail)
	}
	if detail.Guidance == "" {
		t.Error("expected the confirmation card to carry keep-guidance text directly, not just the originating finding")
	}
}

func TestProposeComfyUIDelete_NonInfoSeverityNeverProposes(t *testing.T) {
	// warn = the map was unbuildable — must never propose a deletion.
	f := Finding{CheckID: "comfyui_prune", Severity: SeverityWarn,
		Evidence: map[string]any{"candidates": []comfyui.FileInfo{{FolderType: "x", RelPath: "y", FullPath: "/root/x/y", SizeBytes: 1}}}}
	if drafts := proposeComfyUIDelete(&CheckEnv{}, f, BrainResolution{}); len(drafts) != 0 {
		t.Errorf("got %d drafts for a warn finding, want 0", len(drafts))
	}
}

func TestProposeComfyUIDelete_NoCandidatesNoProposal(t *testing.T) {
	f := Finding{CheckID: "comfyui_prune", Severity: SeverityInfo, Evidence: map[string]any{}}
	if drafts := proposeComfyUIDelete(&CheckEnv{}, f, BrainResolution{}); len(drafts) != 0 {
		t.Errorf("got %d drafts with zero candidates, want 0", len(drafts))
	}
}

// TestProposeComfyUIDelete_KeepFilesExcluded covers S7-followup smith UX
// sprint: an operator-kept file is dropped from the proposal (never
// re-proposed), a non-kept sibling still gets proposed normally.
func TestProposeComfyUIDelete_KeepFilesExcluded(t *testing.T) {
	f := Finding{
		CheckID: "comfyui_prune", Severity: SeverityInfo,
		Evidence: map[string]any{"candidates": []comfyui.FileInfo{
			{FolderType: "checkpoints", RelPath: "a.safetensors", FullPath: "/root/checkpoints/a.safetensors", SizeBytes: 100},
			{FolderType: "loras", RelPath: "b.safetensors", FullPath: "/root/loras/b.safetensors", SizeBytes: 200},
		}},
	}
	env := &CheckEnv{ComfyUIKeepFiles: []string{"/root/checkpoints/a.safetensors"}}
	drafts := proposeComfyUIDelete(env, f, BrainResolution{})
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (the non-kept file still proposed)", len(drafts))
	}
	var detail deleteFilesDetail
	if err := json.Unmarshal(drafts[0].Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if len(detail.Files) != 1 || detail.Files[0].Path != "/root/loras/b.safetensors" {
		t.Errorf("detail.Files = %+v, want only the non-kept b.safetensors", detail.Files)
	}
}

// TestProposeComfyUIDelete_AllCandidatesKeptNoProposal covers the case every
// candidate this sweep is on the keep-list — must produce nothing, not an
// empty-files proposal.
func TestProposeComfyUIDelete_AllCandidatesKeptNoProposal(t *testing.T) {
	f := Finding{
		CheckID: "comfyui_prune", Severity: SeverityInfo,
		Evidence: map[string]any{"candidates": []comfyui.FileInfo{
			{FolderType: "checkpoints", RelPath: "a.safetensors", FullPath: "/root/checkpoints/a.safetensors", SizeBytes: 100},
		}},
	}
	env := &CheckEnv{ComfyUIKeepFiles: []string{"/root/checkpoints/a.safetensors"}}
	if drafts := proposeComfyUIDelete(env, f, BrainResolution{}); len(drafts) != 0 {
		t.Errorf("got %d drafts when every candidate is kept, want 0", len(drafts))
	}
}

// TestComfyUIPrune_ManualOnlyExcludedFromDeepSweep covers the "on-demand
// only" half of the same sprint: comfyui_prune must never run in an
// automatic scheduled deep sweep, only when explicitly selected by ID.
func TestComfyUIPrune_ManualOnlyExcludedFromDeepSweep(t *testing.T) {
	selected, err := selectChecks(ScopeDeep, nil)
	if err != nil {
		t.Fatalf("selectChecks(deep): %v", err)
	}
	for _, c := range selected {
		if c.ID == "comfyui_prune" {
			t.Fatal("comfyui_prune must not be selected by an automatic deep sweep (ManualOnly)")
		}
	}
	// Explicit selection by ID still works — the on-demand "Custom…" picker
	// path.
	explicit, err := selectChecks("", []string{"comfyui_prune"})
	if err != nil {
		t.Fatalf("selectChecks(explicit comfyui_prune): %v", err)
	}
	if len(explicit) != 1 || explicit[0].ID != "comfyui_prune" {
		t.Errorf("explicit selection = %+v, want exactly comfyui_prune", explicit)
	}
}

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

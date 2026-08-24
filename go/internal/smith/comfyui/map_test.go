// SPDX-License-Identifier: Apache-2.0

package comfyui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeClient is a Client test double — BuildMap never talks to a real
// ComfyUI in tests.
type fakeClient struct {
	objectInfo    map[string]ObjectInfoEntry
	objectInfoErr error
	queue         QueueResponse
	queueErr      error
	history       map[string]HistoryEntry
	historyErr    error
}

func (f *fakeClient) Healthy(context.Context) bool { return f.objectInfoErr == nil }
func (f *fakeClient) Queue(context.Context) (QueueResponse, error) {
	return f.queue, f.queueErr
}
func (f *fakeClient) History(context.Context) (map[string]HistoryEntry, error) {
	return f.history, f.historyErr
}
func (f *fakeClient) ObjectInfo(context.Context) (map[string]ObjectInfoEntry, error) {
	return f.objectInfo, f.objectInfoErr
}

// writeModelFile creates dir/relPath under root, with the given size.
func writeModelFile(t *testing.T, root, folderType, relPath string, size int) {
	t.Helper()
	full := filepath.Join(root, folderType, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWorkflow(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildMap_GuardrailA_APIUnreachable(t *testing.T) {
	client := &fakeClient{objectInfoErr: errors.New("connection refused")}
	res := BuildMap(context.Background(), client, nil, nil)
	if res.Buildable {
		t.Fatal("expected an unbuildable map")
	}
	if res.RefusalReason != ReasonUnbuildable {
		t.Errorf("reason = %s, want %s", res.RefusalReason, ReasonUnbuildable)
	}
}

func TestBuildMap_GuardrailA_ZeroWorkflows(t *testing.T) {
	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{}}
	res := BuildMap(context.Background(), client, nil, []string{t.TempDir()}) // empty dir, no *.json
	if res.Buildable {
		t.Fatal("expected an unbuildable map")
	}
	if res.RefusalReason != ReasonUnbuildable {
		t.Errorf("reason = %s, want %s", res.RefusalReason, ReasonUnbuildable)
	}
}

func TestBuildMap_GuardrailB_ZeroLoaderWorkflow(t *testing.T) {
	dir := t.TempDir()
	// Real, non-empty, structurally valid workflow with zero recognized
	// loader refs — exactly fact 2's trap if this guardrail didn't exist.
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "MarkdownNote", "widgets_values": ["hi"]}]}`)

	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "unused.safetensors", 100)

	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if res.Buildable {
		t.Fatal("expected an unbuildable map — a zero-loader workflow must block everything")
	}
	if res.RefusalReason != ReasonZeroLoaderWorkflow {
		t.Errorf("reason = %s, want %s", res.RefusalReason, ReasonZeroLoaderWorkflow)
	}
	if len(res.Candidates) != 0 {
		t.Error("a refused map must never carry candidates")
	}
}

func TestBuildMap_GuardrailB_RealForgeHostShapeWouldHaveTrapped(t *testing.T) {
	// If BuildMap parsed only workflow.go's top-level nodes[] (the bug the
	// original design doc would have shipped), this exact real fixture
	// would trip guardrail (b) — proving the recursive subgraph walk is
	// load-bearing, not cosmetic.
	dir := t.TempDir()
	writeWorkflow(t, dir, "real.json", realForgeHostWorkflowShape)
	root := t.TempDir()
	writeModelFile(t, root, "text_encoders", "qwen_2.5_vl_7b_fp8_scaled.safetensors", 100)
	writeModelFile(t, root, "vae", "qwen_image_vae.safetensors", 100)
	writeModelFile(t, root, "diffusion_models", "qwen_image_2512_fp8_e4m3fn.safetensors", 100)
	writeModelFile(t, root, "loras", "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors", 100)

	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected a buildable map (subgraph loaders correctly found), got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("expected zero candidates — every real file is referenced by the workflow, got %+v", res.Candidates)
	}
}

func TestBuildMap_GuardrailC_UnknownLoaderClass(t *testing.T) {
	dir := t.TempDir()
	// A recognized loader is present too, so this workflow does NOT trip
	// guardrail (b) (zero recognized refs) — isolating guardrail (c).
	writeWorkflow(t, dir, "w.json", `{"nodes": [
		{"type": "SomeNewLoaderNode", "widgets_values": ["model.safetensors"]},
		{"type": "VAELoader", "widgets_values": ["known.safetensors"]}
	]}`)
	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "model.safetensors", 100)
	writeModelFile(t, root, "vae", "known.safetensors", 100)

	// /object_info says SomeNewLoaderNode has a combo field whose options
	// include a real file this package's Loaders table has never heard of.
	objectInfo := map[string]ObjectInfoEntry{
		"SomeNewLoaderNode": {},
	}
	objectInfo["SomeNewLoaderNode"] = ObjectInfoEntry{}
	oi := objectInfo["SomeNewLoaderNode"]
	oi.Input.Required = map[string]json.RawMessage{
		"model_name": json.RawMessage(`[["model.safetensors"], {}]`),
	}
	objectInfo["SomeNewLoaderNode"] = oi

	client := &fakeClient{objectInfo: objectInfo}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if res.Buildable {
		t.Fatal("expected an unbuildable map — an unrecognized model-folder loader class must block")
	}
	if res.RefusalReason != ReasonUnknownLoaderClass {
		t.Errorf("reason = %s, want %s", res.RefusalReason, ReasonUnknownLoaderClass)
	}
}

func TestBuildMap_NoGuardrailC_WhenComboDoesntTouchInventory(t *testing.T) {
	// An unrecognized class_type whose combo options don't overlap with any
	// real file must NOT trip guardrail (c) — e.g. a class_type with no
	// file-like combo at all (KSampler-shaped: numeric/string options only).
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "SomeNewLoaderNode", "widgets_values": []}, {"type": "CheckpointLoaderSimple", "widgets_values": ["model.safetensors"]}]}`)
	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "model.safetensors", 100)

	oi := ObjectInfoEntry{}
	oi.Input.Required = map[string]json.RawMessage{
		"sampler_name": json.RawMessage(`[["euler", "simple"], {}]`), // no overlap with any real file
	}
	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{"SomeNewLoaderNode": oi}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected a buildable map — combo options don't touch real inventory, got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
}

func TestBuildMap_GuardrailD_RootCoverageGap(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "CheckpointLoaderSimple", "widgets_values": ["known.safetensors"]}]}`)
	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "known.safetensors", 100)

	// ComfyUI's own object_info claims a SECOND checkpoint exists
	// ("other_root_only.safetensors") that isn't under our one configured
	// root — simulating fact 1: a second, unconfigured model root.
	oi := ObjectInfoEntry{}
	oi.Input.Required = map[string]json.RawMessage{
		"ckpt_name": json.RawMessage(`[["known.safetensors", "other_root_only.safetensors"], {}]`),
	}
	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{"CheckpointLoaderSimple": oi}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if res.Buildable {
		t.Fatal("expected an unbuildable map — a file ComfyUI sees but we can't locate must block")
	}
	if res.RefusalReason != ReasonRootCoverage {
		t.Errorf("reason = %s, want %s", res.RefusalReason, ReasonRootCoverage)
	}
	if len(res.MissingFromRoots) != 1 || res.MissingFromRoots[0] != "checkpoints/other_root_only.safetensors" {
		t.Errorf("MissingFromRoots = %v", res.MissingFromRoots)
	}
}

func TestBuildMap_SyntheticComboValueNotAGap(t *testing.T) {
	// Live regression, found running this exact check against ForgeHost
	// 2026-08-11: GET /object_info/VAELoader really returns
	// vae_name: ["qwen_image_vae.safetensors", "pixel_space"] — "pixel_space"
	// is VAELoader's own built-in "no VAE / passthrough" sentinel, not a
	// file. Without excluding it, guardrail (d) permanently refuses every
	// real ComfyUI install that has VAELoader registered.
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "VAELoader", "widgets_values": ["known.safetensors"]}]}`)
	root := t.TempDir()
	writeModelFile(t, root, "vae", "known.safetensors", 100)

	oi := ObjectInfoEntry{}
	oi.Input.Required = map[string]json.RawMessage{
		"vae_name": json.RawMessage(`[["known.safetensors", "pixel_space"], {}]`),
	}
	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{"VAELoader": oi}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected buildable — pixel_space must be excluded from the coverage check, got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
}

func TestBuildMap_TwoRootsMergedFact1(t *testing.T) {
	// Fact 1: ComfyUI merges file lists across every configured root for a
	// given folder type. A file that exists ONLY in the second root must
	// still be found — proving BOTH roots get walked, not just the first.
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "VAELoader", "widgets_values": ["in_root2.safetensors"]}]}`)
	root1, root2 := t.TempDir(), t.TempDir()
	writeModelFile(t, root2, "vae", "in_root2.safetensors", 100)

	oi := ObjectInfoEntry{}
	oi.Input.Required = map[string]json.RawMessage{"vae_name": json.RawMessage(`[["in_root2.safetensors"], {}]`)}
	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{"VAELoader": oi}}

	res := BuildMap(context.Background(), client, []string{root1, root2}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected buildable, got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("expected zero candidates (the referenced file lives in root2), got %+v", res.Candidates)
	}
	if res.InventoryCount != 1 {
		t.Errorf("InventoryCount = %d, want 1", res.InventoryCount)
	}
}

func TestBuildMap_UnreferencedFileIsACandidate(t *testing.T) {
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "CheckpointLoaderSimple", "widgets_values": ["used.safetensors"]}]}`)
	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "used.safetensors", 100)
	writeModelFile(t, root, "checkpoints", "unused.safetensors", 999)

	client := &fakeClient{objectInfo: map[string]ObjectInfoEntry{}}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected buildable, got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].RelPath != "unused.safetensors" {
		t.Fatalf("Candidates = %+v, want exactly [unused.safetensors]", res.Candidates)
	}
	if res.Candidates[0].SizeBytes != 999 {
		t.Errorf("SizeBytes = %d, want 999", res.Candidates[0].SizeBytes)
	}
}

func TestBuildMap_ActiveQueueFileNeverACandidate(t *testing.T) {
	// A file with no saved-workflow entry at all but actively queued right
	// now — §4.9's "in use right now" source — must not be offered for
	// deletion.
	dir := t.TempDir()
	writeWorkflow(t, dir, "w.json", `{"nodes": [{"type": "CheckpointLoaderSimple", "widgets_values": ["saved.safetensors"]}]}`)
	root := t.TempDir()
	writeModelFile(t, root, "checkpoints", "saved.safetensors", 100)
	writeModelFile(t, root, "vae", "queued_only.safetensors", 100)

	client := &fakeClient{
		objectInfo: map[string]ObjectInfoEntry{},
		queue: QueueResponse{
			Running: []json.RawMessage{json.RawMessage(`[1, "id", {"1": {"class_type": "VAELoader", "inputs": {"vae_name": "queued_only.safetensors"}}}, {}, []]`)},
		},
	}
	res := BuildMap(context.Background(), client, []string{root}, []string{dir})
	if !res.Buildable {
		t.Fatalf("expected buildable, got refusal: %s / %s", res.RefusalReason, res.RefusalDetail)
	}
	for _, c := range res.Candidates {
		if c.RelPath == "queued_only.safetensors" {
			t.Fatal("a file referenced only by the live queue must never be a deletion candidate")
		}
	}
}

// SPDX-License-Identifier: Apache-2.0

package comfyui

import "testing"

// realForgeHostWorkflowShape is a trimmed-down but structurally faithful copy of
// /opt/comfyui/app/user/default/workflows/image_qwen_Image_2512.json,
// captured live 2026-08-11 — the exact fixture that proves fact 2 (loaders
// live inside definitions.subgraphs[].nodes[], not top-level nodes[]).
const realForgeHostWorkflowShape = `{
  "id": "c3c58f7e-2004-43ae-8b06-a956294bf7f4",
  "nodes": [
    {"type": "MarkdownNote", "widgets_values": ["some readme text, no loaders here"]},
    {"type": "SaveImage", "widgets_values": ["Qwen-Image-2512"]},
    {"type": "c3c58f7e-2004-43ae-8b06-a956294bf7f4", "widgets_values": []}
  ],
  "definitions": {
    "subgraphs": [
      {
        "id": "c3c58f7e-2004-43ae-8b06-a956294bf7f4",
        "name": "Text to Image (Qwen-Image 2512)",
        "nodes": [
          {"type": "CLIPLoader", "widgets_values": ["qwen_2.5_vl_7b_fp8_scaled.safetensors", "qwen_image", "default"]},
          {"type": "VAELoader", "widgets_values": ["qwen_image_vae.safetensors"]},
          {"type": "UNETLoader", "widgets_values": ["qwen_image_2512_fp8_e4m3fn.safetensors", "default"]},
          {"type": "LoraLoaderModelOnly", "widgets_values": ["Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors", 1]},
          {"type": "CLIPTextEncode", "widgets_values": ["retroanime style embedding:my_style with more prompt text"]},
          {"type": "KSampler", "widgets_values": [920717318372886, "randomize", 50, 4, "euler", "simple", 1]}
        ]
      }
    ]
  }
}`

func TestParseWorkflowRefs_RealForgeHostShape(t *testing.T) {
	refs := NewRefSet()
	nodeCount, err := ParseWorkflowRefs(refs, []byte(realForgeHostWorkflowShape))
	if err != nil {
		t.Fatalf("ParseWorkflowRefs: %v", err)
	}
	if nodeCount == 0 {
		t.Fatal("expected nodes to be counted")
	}

	want := []Reference{
		{FolderType: "text_encoders", Name: "qwen_2.5_vl_7b_fp8_scaled.safetensors"},
		{FolderType: "vae", Name: "qwen_image_vae.safetensors"},
		{FolderType: "diffusion_models", Name: "qwen_image_2512_fp8_e4m3fn.safetensors"},
		{FolderType: "loras", Name: "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors"},
	}
	for _, w := range want {
		if !refs.Has(w.FolderType, w.Name) {
			t.Errorf("missing reference %+v — a top-level-only parser would miss ALL of these (fact 2)", w)
		}
	}
	if !refs.Has("embeddings", "my_style") {
		t.Error("expected embedding:my_style to be extracted from the CLIPTextEncode text widget")
	}
	if len(refs.Refs) != 5 {
		t.Errorf("got %d refs, want exactly 5 (4 loaders + 1 embedding)", len(refs.Refs))
	}
}

// TestParseWorkflowRefs_TopLevelOnly_WouldFindNothing documents (not
// asserts a bug — asserts the SHAPE that motivates the zero-loader
// guardrail) that a workflow with real content ONLY at the top level and no
// subgraphs still parses fine and finds real refs — the guardrail exists
// for the OPPOSITE case (loaders exist but this parser can't find them),
// not for "no subgraphs present at all".
func TestParseWorkflowRefs_TopLevelLoaders_NoSubgraphsNeeded(t *testing.T) {
	raw := `{"nodes": [{"type": "CheckpointLoaderSimple", "widgets_values": ["model.safetensors"]}]}`
	refs := NewRefSet()
	if _, err := ParseWorkflowRefs(refs, []byte(raw)); err != nil {
		t.Fatalf("ParseWorkflowRefs: %v", err)
	}
	if !refs.Has("checkpoints", "model.safetensors") {
		t.Error("expected a top-level-only loader (no subgraphs) to still be found")
	}
}

func TestParseWorkflowRefs_ZeroLoaderTrap(t *testing.T) {
	// Structurally valid, has real nodes, but none of them are recognized
	// loaders — the exact shape a naive parser (or this parser hitting an
	// unrecognized node shape) produces. The caller (BuildMap) is
	// responsible for treating nodeCount>0 && len(refs)==0 as "unparsed",
	// not "this workflow references nothing" — this test just proves the
	// raw signal is available to make that call.
	raw := `{"nodes": [{"type": "MarkdownNote", "widgets_values": ["hello"]}, {"type": "Note", "widgets_values": ["world"]}]}`
	refs := NewRefSet()
	nodeCount, err := ParseWorkflowRefs(refs, []byte(raw))
	if err != nil {
		t.Fatalf("ParseWorkflowRefs: %v", err)
	}
	if nodeCount != 2 {
		t.Errorf("nodeCount = %d, want 2", nodeCount)
	}
	if len(refs.Refs) != 0 {
		t.Errorf("expected zero refs, got %d", len(refs.Refs))
	}
}

func TestParseWorkflowRefs_MalformedJSON(t *testing.T) {
	refs := NewRefSet()
	if _, err := ParseWorkflowRefs(refs, []byte("{not json")); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestParseWorkflowRefs_NestedSubgraphs(t *testing.T) {
	// Not observed live, but handled: a subgraph definition that itself
	// carries a further nested definitions.subgraphs block.
	raw := `{
	  "nodes": [],
	  "definitions": {"subgraphs": [
	    {"nodes": [], "definitions": {"subgraphs": [
	      {"nodes": [{"type": "VAELoader", "widgets_values": ["nested.safetensors"]}]}
	    ]}}
	  ]}
	}`
	refs := NewRefSet()
	if _, err := ParseWorkflowRefs(refs, []byte(raw)); err != nil {
		t.Fatalf("ParseWorkflowRefs: %v", err)
	}
	if !refs.Has("vae", "nested.safetensors") {
		t.Error("expected a doubly-nested subgraph loader to still be found")
	}
}

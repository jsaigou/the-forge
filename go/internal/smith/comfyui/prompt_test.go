// SPDX-License-Identifier: Apache-2.0

package comfyui

import (
	"encoding/json"
	"testing"
)

func TestAddPromptRefs_LiteralAndLink(t *testing.T) {
	p := Prompt{
		"1": {ClassType: "CheckpointLoaderSimple", Inputs: map[string]json.RawMessage{
			"ckpt_name": json.RawMessage(`"model.safetensors"`),
		}},
		"2": {ClassType: "VAELoader", Inputs: map[string]json.RawMessage{
			// A link ([node_id, output_index]), not a literal — must be
			// skipped, never mis-parsed as a filename.
			"vae_name": json.RawMessage(`[1, 0]`),
		}},
		"3": {ClassType: "CLIPTextEncode", Inputs: map[string]json.RawMessage{
			"text": json.RawMessage(`"a photo with embedding:portrait_style in it"`),
		}},
		"4": {ClassType: "SomeCustomNode", Inputs: map[string]json.RawMessage{}},
	}
	refs := NewRefSet()
	AddPromptRefs(refs, p)

	if !refs.Has("checkpoints", "model.safetensors") {
		t.Error("expected literal ckpt_name to be captured")
	}
	if refs.Has("vae", "") {
		t.Error("a link value must never resolve to an empty-string reference")
	}
	if len(refs.Refs) != 2 { // checkpoint + embedding
		t.Errorf("got %d refs, want 2 (link must not count)", len(refs.Refs))
	}
	if !refs.Has("embeddings", "portrait_style") {
		t.Error("expected the embedding token inside CLIPTextEncode text to be captured")
	}
	for _, want := range []string{"CheckpointLoaderSimple", "VAELoader", "CLIPTextEncode", "SomeCustomNode"} {
		if !refs.ClassTypesSeen[want] {
			t.Errorf("expected class_type %q to be recorded as seen", want)
		}
	}
}

func TestAddQueueRefs_RunningAndPending(t *testing.T) {
	q := QueueResponse{
		Running: []json.RawMessage{json.RawMessage(`[1, "id1", {"1": {"class_type": "VAELoader", "inputs": {"vae_name": "a.safetensors"}}}, {}, []]`)},
		Pending: []json.RawMessage{
			json.RawMessage(`[2, "id2", {"1": {"class_type": "VAELoader", "inputs": {"vae_name": "b.safetensors"}}}, {}, []]`),
			json.RawMessage(`"not an array"`), // malformed entry must not abort the batch
		},
	}
	refs := NewRefSet()
	parsed, skipped := AddQueueRefs(refs, q)
	if parsed != 2 {
		t.Errorf("parsed = %d, want 2", parsed)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if !refs.Has("vae", "a.safetensors") || !refs.Has("vae", "b.safetensors") {
		t.Error("expected both running and pending entries' refs to be captured")
	}
}

func TestAddHistoryRefs(t *testing.T) {
	h := map[string]HistoryEntry{
		"id1": {Prompt: []json.RawMessage{
			json.RawMessage(`1`), json.RawMessage(`"id1"`),
			json.RawMessage(`{"1": {"class_type": "UNETLoader", "inputs": {"unet_name": "u.safetensors"}}}`),
		}},
		"id2": {Prompt: []json.RawMessage{json.RawMessage(`1`)}}, // too short — must be skipped, not error
	}
	refs := NewRefSet()
	parsed, skipped := AddHistoryRefs(refs, h)
	if parsed != 1 || skipped != 1 {
		t.Errorf("parsed=%d skipped=%d, want 1/1", parsed, skipped)
	}
	if !refs.Has("diffusion_models", "u.safetensors") {
		t.Error("expected the history entry's unet_name to be captured")
	}
}

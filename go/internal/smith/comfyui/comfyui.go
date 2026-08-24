// SPDX-License-Identifier: Apache-2.0

// Package comfyui is smith P6's ComfyUI client + workflow-dependency-map
// builder (docs/v5-smith.md §4.9 FR7). Deliberately separate from
// internal/smith/web: that package's client is SSRF-hardened to reject
// private IPs by design (web/client.go's isPublicIP), so it structurally
// cannot reach ComfyUI's loopback API — this package's client is loopback
// ONLY, the opposite trust boundary.
//
// The dependency map is the pruning gate (docs/v5-smith.md §4.9's "the
// mitigation IS the design: qualification is the deterministic workflow
// dependency map, not access-time"). See map.go's BuildMap doc comment for
// the four refusal guardrails and the two ground-truth facts (found live on
// ForgeHost, 2026-08-11, before any of this was written) that make a naive
// single-root, top-level-nodes-only parse actively unsafe:
//
//  1. ComfyUI has TWO distinct model root directories on ForgeHost
//     (/opt/comfyui/app/models and /opt/comfyui/models, the latter declared
//     as extra_model_paths.yaml's base_path) — a single hardcoded root
//     silently misses half the disk.
//  2. Real saved workflows nest their loader nodes inside
//     definitions.subgraphs[].nodes[], not the top-level nodes[] array (which
//     holds only MarkdownNote/SaveImage/a subgraph-instance stub with empty
//     widgets_values). A top-level-only parse finds zero references in every
//     real workflow.
package comfyui

import "encoding/json"

// PromptNode is one node in the API/prompt-submission format — what
// /history and /queue return (and what a client POSTs to /prompt). Distinct
// from the UI-saved-workflow format (workflow.go's WorkflowFile): filenames
// live in named Inputs fields here, not a positional widgets_values array.
type PromptNode struct {
	ClassType string                     `json:"class_type"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

// Prompt is the {node_id: PromptNode} graph ComfyUI submits/records.
type Prompt map[string]PromptNode

// QueueResponse is GET /queue's body.
type QueueResponse struct {
	Running []json.RawMessage `json:"queue_running"`
	Pending []json.RawMessage `json:"queue_pending"`
}

// HistoryEntry is one GET /history record — Prompt is buried at index 2 of
// the raw 5-element array ComfyUI uses for both queue items and history
// entries ([number, prompt_id, prompt, extra_data, outputs]).
type HistoryEntry struct {
	Prompt []json.RawMessage `json:"prompt"`
}

// ObjectInfoEntry is one class_type's schema, as returned by GET
// /object_info — only the piece this package reads (the input spec; output
// types/category/etc. are irrelevant to dependency mapping).
type ObjectInfoEntry struct {
	Input struct {
		Required map[string]json.RawMessage `json:"required"`
		Optional map[string]json.RawMessage `json:"optional"`
	} `json:"input"`
}

// promptFromQueueItem extracts the Prompt (index 2) from one raw
// queue_running/queue_pending array entry. Returns (nil, false) on any
// shape mismatch — degrade, never panic, on a ComfyUI version's wire
// format drifting.
func promptFromQueueItem(raw json.RawMessage) (Prompt, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) < 3 {
		return nil, false
	}
	var p Prompt
	if err := json.Unmarshal(arr[2], &p); err != nil {
		return nil, false
	}
	return p, true
}

// promptFromHistoryEntry mirrors promptFromQueueItem for one /history value.
func promptFromHistoryEntry(e HistoryEntry) (Prompt, bool) {
	if len(e.Prompt) < 3 {
		return nil, false
	}
	var p Prompt
	if err := json.Unmarshal(e.Prompt[2], &p); err != nil {
		return nil, false
	}
	return p, true
}

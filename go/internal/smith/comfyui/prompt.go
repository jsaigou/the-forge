// SPDX-License-Identifier: Apache-2.0

package comfyui

import "encoding/json"

// AddPromptRefs extracts loader references from one API/prompt-format graph
// (a /queue or /history entry) into dst — the "in use right now" source
// (docs/v5-smith.md §4.9): a file may have no saved workflow entry at all
// and still be actively rendering.
func AddPromptRefs(dst *RefSet, p Prompt) {
	for _, node := range p {
		dst.SeeClass(node.ClassType)
		spec, ok := Loaders[node.ClassType]
		if !ok {
			continue
		}
		raw, ok := node.Inputs[spec.APIField]
		if !ok {
			continue
		}
		// A field can be a literal value or a [node_id, output_index] link
		// to another node's output — only a literal string is a real file
		// reference; a link means the filename isn't known without graph
		// evaluation, and the linked upstream node (if it's itself a
		// recognized loader) is already handled independently.
		var name string
		if err := json.Unmarshal(raw, &name); err == nil {
			dst.Add(spec.FolderType, name)
		}
	}
	// Embedding tokens can appear in any string-valued input, most commonly
	// CLIPTextEncode's "text" field.
	for _, node := range p {
		for _, raw := range node.Inputs {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				for _, name := range embeddingTokens(s) {
					dst.Add("embeddings", name)
				}
			}
		}
	}
}

// AddQueueRefs parses every running+pending entry from GET /queue.
func AddQueueRefs(dst *RefSet, q QueueResponse) (parsed, skipped int) {
	all := append(append([]json.RawMessage{}, q.Running...), q.Pending...)
	for _, raw := range all {
		p, ok := promptFromQueueItem(raw)
		if !ok {
			skipped++
			continue
		}
		AddPromptRefs(dst, p)
		parsed++
	}
	return parsed, skipped
}

// AddHistoryRefs parses every GET /history entry.
func AddHistoryRefs(dst *RefSet, history map[string]HistoryEntry) (parsed, skipped int) {
	for _, entry := range history {
		p, ok := promptFromHistoryEntry(entry)
		if !ok {
			skipped++
			continue
		}
		AddPromptRefs(dst, p)
		parsed++
	}
	return parsed, skipped
}

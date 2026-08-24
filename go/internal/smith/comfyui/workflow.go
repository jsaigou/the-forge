// SPDX-License-Identifier: Apache-2.0

package comfyui

import (
	"encoding/json"
	"fmt"
)

// workflowNode is one node in the saved UI-workflow format. WidgetsValues
// is positional (the shape found live 2026-08-11: filename at a
// per-class-type fixed index, per LoaderSpec.WidgetIndex) — unlike the
// API/prompt format's named Inputs map (prompt.go).
type workflowNode struct {
	Type          string            `json:"type"`
	WidgetsValues []json.RawMessage `json:"widgets_values"`
}

// subgraphDef is one entry in definitions.subgraphs — itself shaped like a
// mini workflow (its own nodes[], and in principle its own nested
// definitions, though that hasn't been observed live). THIS is where every
// real loader node actually lives (fact 2, package doc): the top-level
// nodes[] in both real ForgeHost workflows held only MarkdownNote/SaveImage/one
// subgraph-instance stub with empty widgets_values.
type subgraphDef struct {
	Nodes       []workflowNode `json:"nodes"`
	Definitions *definitions   `json:"definitions"`
}

type definitions struct {
	Subgraphs []subgraphDef `json:"subgraphs"`
}

// workflowFile is one saved ComfyUI workflow JSON's top-level shape (the
// fields this package reads; a real file has many more we ignore).
type workflowFile struct {
	Nodes       []workflowNode `json:"nodes"`
	Definitions *definitions   `json:"definitions"`
}

// ParseWorkflowRefs parses one saved workflow JSON file's content into dst,
// walking the top-level nodes[] AND every definitions.subgraphs[].nodes[]
// (recursively, in case a subgraph nests further — not observed live, but
// cheap to handle correctly rather than assume one level is the ceiling).
// nodeCount is the total node count seen across every level — BuildMap uses
// "structurally valid, nodeCount>0, but zero refs added" to detect guardrail
// (b)'s trap (a workflow whose loaders live in a shape this parser doesn't
// recognize) rather than treating it as an honestly empty workflow.
func ParseWorkflowRefs(dst *RefSet, raw []byte) (nodeCount int, err error) {
	var wf workflowFile
	if err := json.Unmarshal(raw, &wf); err != nil {
		return 0, fmt.Errorf("comfyui: parse workflow json: %w", err)
	}
	nodeCount += walkNodes(dst, wf.Nodes)
	nodeCount += walkDefinitions(dst, wf.Definitions)
	return nodeCount, nil
}

func walkDefinitions(dst *RefSet, d *definitions) int {
	if d == nil {
		return 0
	}
	n := 0
	for _, sg := range d.Subgraphs {
		n += walkNodes(dst, sg.Nodes)
		n += walkDefinitions(dst, sg.Definitions)
	}
	return n
}

func walkNodes(dst *RefSet, nodes []workflowNode) int {
	for _, node := range nodes {
		dst.SeeClass(node.Type)
		if spec, ok := Loaders[node.Type]; ok && spec.WidgetIndex < len(node.WidgetsValues) {
			var name string
			if json.Unmarshal(node.WidgetsValues[spec.WidgetIndex], &name) == nil {
				dst.Add(spec.FolderType, name)
			}
		}
		for _, wv := range node.WidgetsValues {
			var s string
			if json.Unmarshal(wv, &s) == nil {
				for _, name := range embeddingTokens(s) {
					dst.Add("embeddings", name)
				}
			}
		}
	}
	return len(nodes)
}

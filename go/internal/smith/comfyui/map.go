// SPDX-License-Identifier: Apache-2.0

package comfyui

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RefusalReason names which of BuildMap's four guardrails blocked a map —
// docs/v5-smith.md §4.9's requirement that "the confirmation card lists
// every file with its evidence" extends to knowing WHY zero candidates
// (or zero anything) came back, not just that they did.
type RefusalReason string

const (
	ReasonNone RefusalReason = ""

	// ReasonUnbuildable: ComfyUI's API itself is unreachable, or zero
	// workflow files exist to parse at all — §4.9's own guardrail (a). "An
	// unbuildable map is not an empty map."
	ReasonUnbuildable RefusalReason = "unbuildable"

	// ReasonZeroLoaderWorkflow: a workflow file parsed structurally (valid
	// JSON with a nodes shape) but yielded zero recognized loader
	// references — treated as UNPARSED, never as "references nothing". This
	// is fact 2's trap: a naive top-level-only parser finds zero references
	// in every real ForgeHost workflow, which without this guardrail would
	// qualify every in-use model for deletion.
	ReasonZeroLoaderWorkflow RefusalReason = "zero_loader_workflow"

	// ReasonUnknownLoaderClass: a class_type appears in /object_info with a
	// combo input resolving against a real model folder, but this package
	// has no LoaderSpec for it — a future ComfyUI loader node type this
	// mapping table hasn't been taught about yet.
	ReasonUnknownLoaderClass RefusalReason = "unknown_loader_class"

	// ReasonRootCoverage: /object_info's own combo list names a file this
	// package's configured model_roots can't locate — proof a root is
	// missing (fact 1's failure mode) rather than a guess.
	ReasonRootCoverage RefusalReason = "root_coverage"
)

// FileInfo is one real file found under a configured model root.
type FileInfo struct {
	FolderType string    `json:"folder_type"`
	RelPath    string    `json:"rel_path"` // relative to root/<folder_type>, forward-slashed
	FullPath   string    `json:"full_path"`
	SizeBytes  int64     `json:"size_bytes"`
	ModTime    time.Time `json:"mod_time"`
}

// MapResult is BuildMap's full output — evidence-carrying even on refusal,
// per §4.9's "the card always shows the evidence counts".
type MapResult struct {
	Buildable     bool          `json:"buildable"`
	RefusalReason RefusalReason `json:"refusal_reason,omitempty"`
	RefusalDetail string        `json:"refusal_detail,omitempty"`

	Candidates []FileInfo `json:"candidates,omitempty"` // real files, referenced by nothing found

	WorkflowFilesFound  int      `json:"workflow_files_found"`
	WorkflowsParsed     int      `json:"workflows_parsed"`
	ZeroLoaderWorkflows []string `json:"zero_loader_workflows,omitempty"`
	UnknownClasses      []string `json:"unknown_classes,omitempty"`
	MissingFromRoots    []string `json:"missing_from_roots,omitempty"` // "folder_type/name" ComfyUI sees but we can't locate

	QueueParsed     int `json:"queue_parsed,omitempty"`
	QueueSkipped    int `json:"queue_skipped,omitempty"`
	HistoryParsed   int `json:"history_parsed,omitempty"`
	HistorySkipped  int `json:"history_skipped,omitempty"`
	ReferencedCount int `json:"referenced_count"`
	InventoryCount  int `json:"inventory_count"`
}

func refused(reason RefusalReason, detail string, r *MapResult) MapResult {
	r.Buildable = false
	r.RefusalReason = reason
	r.RefusalDetail = detail
	return *r
}

// BuildMap builds the dependency map: the union of every file referenced by
// a saved workflow, a queued prompt, or a history entry, then diffs it
// against every real file under modelRoots to produce prune candidates —
// or refuses (Buildable=false) when any of the four guardrails fire. Every
// path is read-only; BuildMap never deletes anything itself (execute.go's
// dispatchDeleteFiles is the only writer, and only re-checks a fresh map's
// output, never a stale one).
func BuildMap(ctx context.Context, client Client, modelRoots, workflowDirs []string) MapResult {
	result := MapResult{}

	objectInfo, err := client.ObjectInfo(ctx)
	if err != nil {
		return refused(ReasonUnbuildable, "GET /object_info: "+err.Error(), &result)
	}
	queue, err := client.Queue(ctx)
	if err != nil {
		return refused(ReasonUnbuildable, "GET /queue: "+err.Error(), &result)
	}
	history, err := client.History(ctx)
	if err != nil {
		return refused(ReasonUnbuildable, "GET /history: "+err.Error(), &result)
	}

	refs := NewRefSet()

	workflowFiles := listWorkflowFiles(workflowDirs)
	result.WorkflowFilesFound = len(workflowFiles)
	if len(workflowFiles) == 0 {
		return refused(ReasonUnbuildable, "zero parseable workflow files found under the configured workflow_dirs", &result)
	}
	var zeroLoader []string
	for _, path := range workflowFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			zeroLoader = append(zeroLoader, path+" (unreadable: "+err.Error()+")")
			continue
		}
		fileRefs := NewRefSet()
		nodeCount, err := ParseWorkflowRefs(fileRefs, raw)
		if err != nil {
			zeroLoader = append(zeroLoader, path+" (unparseable: "+err.Error()+")")
			continue
		}
		if nodeCount > 0 && len(fileRefs.Refs) == 0 {
			// Structurally valid, non-empty, but zero recognized loader
			// refs — fact 2's exact trap. Treated as unparsed, not empty.
			zeroLoader = append(zeroLoader, path)
			continue
		}
		for ref := range fileRefs.Refs {
			refs.Add(ref.FolderType, ref.Name)
		}
		for ct := range fileRefs.ClassTypesSeen {
			refs.SeeClass(ct)
		}
		result.WorkflowsParsed++
	}
	if len(zeroLoader) > 0 {
		sort.Strings(zeroLoader)
		result.ZeroLoaderWorkflows = zeroLoader
		return refused(ReasonZeroLoaderWorkflow,
			fmt.Sprintf("%d workflow file(s) parsed structurally but yielded no recognized loader reference", len(zeroLoader)), &result)
	}

	result.QueueParsed, result.QueueSkipped = AddQueueRefs(refs, queue)
	result.HistoryParsed, result.HistorySkipped = AddHistoryRefs(refs, history)

	inventory, invErr := walkModelRoots(modelRoots)
	if invErr != nil {
		return refused(ReasonUnbuildable, "walking configured model_roots: "+invErr.Error(), &result)
	}
	result.InventoryCount = len(inventory)

	// Guardrail (c): a class_type this package doesn't recognize, but whose
	// /object_info schema exposes a combo field naming files that actually
	// exist in our inventory — proof it's a real model-folder loader we
	// have no LoaderSpec for.
	var unknown []string
	for ct := range refs.ClassTypesSeen {
		if _, known := Loaders[ct]; known {
			continue
		}
		if entry, ok := objectInfo[ct]; ok && comboTouchesInventory(entry, inventory) {
			unknown = append(unknown, ct)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		result.UnknownClasses = unknown
		return refused(ReasonUnknownLoaderClass,
			fmt.Sprintf("class_type(s) with an unrecognized model-folder loader shape: %v — extend comfyui.Loaders before any deletion proposal", unknown), &result)
	}

	// Guardrail (d): every filename /object_info itself claims exists for a
	// known folder type must be locatable in our inventory — a name ComfyUI
	// sees that we can't find proves a missing configured root.
	missing := rootCoverageGaps(objectInfo, inventory)
	if len(missing) > 0 {
		sort.Strings(missing)
		result.MissingFromRoots = missing
		return refused(ReasonRootCoverage,
			fmt.Sprintf("%d file(s) ComfyUI itself lists are not locatable under the configured model_roots", len(missing)), &result)
	}

	result.ReferencedCount = len(refs.Refs)
	for _, f := range inventory {
		if !refs.Has(f.FolderType, f.RelPath) {
			result.Candidates = append(result.Candidates, f)
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].FullPath < result.Candidates[j].FullPath
	})
	result.Buildable = true
	return result
}

// listWorkflowFiles globs every *.json directly under each workflow dir
// (non-recursive — matches the real ForgeHost layout, one flat directory of
// saved workflows).
func listWorkflowFiles(dirs []string) []string {
	var out []string
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	sort.Strings(out)
	return out
}

// walkModelRoots enumerates every real file under root/<folderType> for
// every configured root × every folder type this package's Loaders table
// knows about (comfyui.KnownFolderTypes) — the union across BOTH of
// ForgeHost's real model roots (fact 1), not just one.
func walkModelRoots(roots []string) ([]FileInfo, error) {
	var out []FileInfo
	for _, root := range roots {
		for _, folderType := range KnownFolderTypes() {
			base := filepath.Join(root, folderType)
			info, err := os.Stat(base)
			if err != nil || !info.IsDir() {
				continue // this root doesn't carry this folder type — not an error
			}
			err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(base, path)
				if err != nil {
					return err
				}
				fi, err := d.Info()
				if err != nil {
					return err
				}
				out = append(out, FileInfo{
					FolderType: folderType,
					RelPath:    filepath.ToSlash(rel),
					FullPath:   path,
					SizeBytes:  fi.Size(),
					ModTime:    fi.ModTime(),
				})
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("walk %s: %w", base, err)
			}
		}
	}
	return out, nil
}

// comboField is /object_info's shape for a combo (dropdown-of-files) input:
// a 2-element array whose first element is the option list.
type comboField [2]json.RawMessage

// comboOptions decodes one input field's raw JSON as a combo option list,
// or (nil, false) when it isn't shaped like one (a plain type name string,
// a non-combo spec, etc).
func comboOptions(raw json.RawMessage) ([]string, bool) {
	var cf comboField
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, false
	}
	var opts []string
	if err := json.Unmarshal(cf[0], &opts); err != nil {
		return nil, false
	}
	return opts, true
}

// comboTouchesInventory reports whether entry has any combo field whose
// options overlap with a real file's RelPath or basename in inventory —
// the guardrail (c) heuristic for "this unrecognized class_type is
// actually a model-folder loader".
func comboTouchesInventory(entry ObjectInfoEntry, inventory []FileInfo) bool {
	basenames := map[string]bool{}
	for _, f := range inventory {
		basenames[f.RelPath] = true
		basenames[filepath.Base(f.RelPath)] = true
	}
	check := func(fields map[string]json.RawMessage) bool {
		for _, raw := range fields {
			opts, ok := comboOptions(raw)
			if !ok {
				continue
			}
			for _, o := range opts {
				if basenames[o] {
					return true
				}
			}
		}
		return false
	}
	return check(entry.Input.Required) || check(entry.Input.Optional)
}

// rootCoverageGaps implements guardrail (d): for every known folder type,
// read every Loaders class_type mapped to it that's actually present in
// objectInfo (an installation may register only some of them — e.g. a
// custom-node-light ComfyUI might have CheckpointLoaderSimple but not
// unCLIPCheckpointLoader), union their combo lists (real ComfyUI reports
// the identical folder_paths.get_filename_list result from every class
// reading the same folder type, so this only widens coverage, never
// conflicts), and report every name ComfyUI lists that isn't in our own
// inventory for that folder type. Checking every mapped class rather than
// one arbitrary pick matters: picking just one non-deterministically (map
// iteration order) could land on a class_type objectInfo doesn't have,
// silently skipping that folder type's coverage check entirely — found via
// a genuinely flaky test (go test -count=5) before this fix, not a static
// review.
func rootCoverageGaps(objectInfo map[string]ObjectInfoEntry, inventory []FileInfo) []string {
	haveByFolder := map[string]map[string]bool{}
	for _, f := range inventory {
		if haveByFolder[f.FolderType] == nil {
			haveByFolder[f.FolderType] = map[string]bool{}
		}
		haveByFolder[f.FolderType][f.RelPath] = true
	}

	classesByFolder := map[string][]string{} // folderType -> every mapped class_type
	for classType, spec := range Loaders {
		classesByFolder[spec.FolderType] = append(classesByFolder[spec.FolderType], classType)
	}

	seenMissing := map[string]bool{}
	var missing []string
	for folderType, classTypes := range classesByFolder {
		have := haveByFolder[folderType]
		for _, classType := range classTypes {
			entry, ok := objectInfo[classType]
			if !ok {
				continue // this loader class isn't installed on this ComfyUI — nothing to cross-check
			}
			spec := Loaders[classType]
			raw, ok := entry.Input.Required[spec.APIField]
			if !ok {
				raw, ok = entry.Input.Optional[spec.APIField]
			}
			if !ok {
				continue
			}
			opts, ok := comboOptions(raw)
			if !ok {
				continue
			}
			for _, name := range opts {
				if syntheticComboValues[name] {
					continue // ComfyUI's own sentinel, e.g. VAELoader's "pixel_space" — never a real file
				}
				if !have[name] {
					key := folderType + "/" + name
					if !seenMissing[key] {
						seenMissing[key] = true
						missing = append(missing, key)
					}
				}
			}
		}
	}
	return missing
}

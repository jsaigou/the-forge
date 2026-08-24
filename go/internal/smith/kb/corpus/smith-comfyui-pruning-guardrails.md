ref: smith:comfyui-pruning-guardrails
doc: smith
slug: comfyui-pruning-guardrails
title: ComfyUI pruning — the four refusal guardrails
category: config
source: docs/v5-smith.md

A file present in the disk inventory but absent from the referenced union is a *candidate*,
  ranked by size. **Four refusal guardrails, checked in order, any one blocks the entire map**
  (`Buildable=false` — no candidates are ever returned alongside a refusal):
  - **(a) unbuildable** — `/object_info`, `/queue`, or `/history` unreachable, or zero workflow
    files found under `workflow_dirs` at all. "An unbuildable map is not an empty map."
  - **(b) zero-loader workflow** — a workflow file parses as structurally valid, non-empty JSON
    but yields **zero** recognized loader references. Treated as *unparsed*, never as "references
    nothing" — this is fact 2's exact trap: without this guardrail, a ComfyUI frontend format
    change (or any parser gap) would silently qualify every in-use file for deletion.
  - **(c) unknown loader class** — a `class_type` appears in `/object_info` with a combo input
    whose option list overlaps real inventory filenames, but `comfyui.Loaders` has no entry for
    it — a loader node type this table hasn't been taught about yet.
  - **(d) root coverage gap** — `/object_info`'s own combo list (ComfyUI's live view of what it
    can see) names a file the configured `model_roots` can't locate — proof of a missing root
    (fact 1's failure mode), not a guess.

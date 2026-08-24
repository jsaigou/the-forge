// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/comfyui"
)

// maxAutoOpenProposals caps how many pending, smith-created proposals may be
// open at once — a flapping check must not generate unbounded rows.
const maxAutoOpenProposals = 20

// proposer maps one check's finding to zero or more proposed actions. Checks
// stay pure Finding-returning functions (checks.go's registry is untouched);
// this is a strictly read-only second pass over a completed sweep's
// findings, using the same CheckEnv.
type proposer func(env *CheckEnv, f Finding, br BrainResolution) []ActionDraft

// proposers maps check ID → proposer (docs/v5-smith.md §4.6's table).
// load_config/settings_change are deliberately absent — nothing in the
// check catalog knows what a mode *ought* to be, and inventing that
// heuristic is exactly what "propose, never do" is meant to avoid.
var proposers = map[string]proposer{
	"always_on_ports":         proposeRestartDownService,
	"compressor_reachability": proposeRestartCompressorProxy,
	"slot_agreement":          proposeReconcileOrphanSlot,
	"kernel_params":           proposeKernelParamsRunbook,
	"gtt_ceiling":             proposeFreeMemoryRunbook,
	"binary_versions":         proposeRebuildRunbook,
	"comfyui_prune":           proposeComfyUIDelete,
}

// evidenceAs reads ev[key] as T. In-process (the common case: proposeFrom
// runs on the same-call findings, before any DB round trip) the concrete Go
// type set by the check function is already T and the direct assertion
// hits. As a fallback — evidence read back after a JSON round trip, or a
// hand-built Finding in a test — it marshals then unmarshals through T's own
// json tags, which works regardless of the original concrete type.
func evidenceAs[T any](ev map[string]any, key string) T {
	var out T
	raw, ok := ev[key]
	if !ok {
		return out
	}
	if v, ok := raw.(T); ok {
		return v
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// ── evidence shapes (json-tag-compatible with each check's Evidence map) ───

type downPort struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
}

type compressorDown struct {
	Service string `json:"service"`
	Unit    string `json:"unit"`
	Port    int    `json:"port"`
	Active  bool   `json:"active"`
	PortUp  bool   `json:"port_up"`
}

type slotMismatch struct {
	Slot          string `json:"slot"`
	SchedulerMode string `json:"scheduler_mode"`
	UnitMode      string `json:"unit_mode"`
}

// servicePortUnit maps runAlwaysOnPorts' cfg.Ports key (the "service" field
// in its "down" evidence) to the real systemd unit name. Any key not in
// this map has no safe unit mapping and is skipped, never guessed.
var servicePortUnit = map[string]string{
	"stt":       "forge-stt",
	"embedding": "forge-embedding",
	"aligner":   "forge-aligner",
}

// ── proposers ────────────────────────────────────────────────────────────

// proposeRestartDownService turns always_on_ports' down list into
// restart_forge_unit proposals, one per resolvable+allowed down service.
func proposeRestartDownService(env *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityWarn {
		return nil
	}
	cfg := env.cfg()
	var out []ActionDraft
	for _, d := range evidenceAs[[]downPort](f.Evidence, "down") {
		unit, ok := servicePortUnit[d.Service]
		if !ok {
			continue // unknown service key — no safe unit mapping, skip
		}
		if allowed, _ := restartAllowed(cfg, unit); !allowed {
			continue
		}
		detail, err := json.Marshal(map[string]any{
			"unit": unit, "reason": fmt.Sprintf("service %q not listening on port %d", d.Service, d.Port),
		})
		if err != nil {
			continue
		}
		out = append(out, ActionDraft{
			Kind:      KindRestartForgeUnit,
			Title:     "Restart " + unit + " (not listening)",
			Risk:      RiskLow,
			Detail:    detail,
			DedupeKey: KindRestartForgeUnit + ":" + unit,
		})
	}
	return out
}

// proposeRestartCompressorProxy turns headroom_health's down list into
// restart_forge_unit proposals, one per unhealthy proxy.
func proposeRestartCompressorProxy(env *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityWarn && f.Severity != SeverityCrit {
		return nil
	}
	cfg := env.cfg()
	var out []ActionDraft
	for _, d := range evidenceAs[[]compressorDown](f.Evidence, "down") {
		if d.Unit == "" {
			continue
		}
		if allowed, _ := restartAllowed(cfg, d.Unit); !allowed {
			continue
		}
		detail, err := json.Marshal(map[string]any{
			"unit": d.Unit,
			"reason": fmt.Sprintf("compressor proxy %q unhealthy (active=%v, port_up=%v)",
				d.Service, d.Active, d.PortUp),
		})
		if err != nil {
			continue
		}
		out = append(out, ActionDraft{
			Kind:      KindRestartForgeUnit,
			Title:     "Restart compressor proxy " + d.Service,
			Risk:      RiskLow,
			Detail:    detail,
			DedupeKey: KindRestartForgeUnit + ":" + d.Unit,
		})
	}
	return out
}

// proposeReconcileOrphanSlot turns slot_agreement's mismatch list into
// unload_slot proposals, ORPHAN DIRECTION ONLY: the scheduler thinks a slot
// is empty but the unit still reports a loaded mode — a stray process smith
// can safely ask the engine to unload. The opposite direction (scheduler
// thinks occupied, unit says empty) means the scheduler's own view is
// stale, not a stray process — there is no smith-executable fix for that,
// so no proposal.
//
// Guardrail 2 (docs/v5-smith.md §4.5): smith never proposes unloading its
// own brain's slot as a means to free memory or reconcile drift. Checked
// here at the proposer, not only in the handoff/approval gate that would
// still catch a human-created equivalent — the two are deliberately
// different: smith itself must never SUGGEST evicting its own brain, but a
// human retains the right to, gated exactly like any other self-evicting
// action.
func proposeReconcileOrphanSlot(env *CheckEnv, f Finding, br BrainResolution) []ActionDraft {
	if f.Severity != SeverityWarn {
		return nil
	}
	var out []ActionDraft
	for _, m := range evidenceAs[[]slotMismatch](f.Evidence, "mismatches") {
		if m.SchedulerMode != "" || m.UnitMode == "" {
			continue // not the orphan direction
		}
		if br.Resolution == BrainLocalSlot && m.Slot == br.Slot {
			continue // guardrail 2
		}
		detail, err := json.Marshal(map[string]any{
			"slot":   m.Slot,
			"reason": fmt.Sprintf("unit still reports mode %q though the scheduler has cleared this slot", m.UnitMode),
		})
		if err != nil {
			continue
		}
		out = append(out, ActionDraft{
			Kind:      KindUnloadSlot,
			Title:     fmt.Sprintf("Unload orphaned slot %s (%s)", strings.ToUpper(m.Slot), m.UnitMode),
			Risk:      RiskHigh,
			Detail:    detail,
			DedupeKey: KindUnloadSlot + ":" + m.Slot,
		})
	}
	return out
}

// proposeKernelParamsRunbook turns kernel_params' missing-param list into a
// single informational runbook action (never something smith executes — the
// fix needs a bootloader edit + reboot).
func proposeKernelParamsRunbook(_ *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityWarn {
		return nil
	}
	missing := evidenceAs[[]string](f.Evidence, "missing")
	if len(missing) == 0 {
		return nil
	}
	steps := []RunbookStep{
		{
			Title:         "Edit the bootloader kernel command line",
			Command:       `sudo grubby --update-kernel=ALL --args="` + strings.Join(missing, " ") + `"`,
			Why:           "these params aren't set on the running kernel — kernel_params found them missing from /proc/cmdline",
			Verify:        "shows " + strings.Join(missing, " "),
			VerifyCommand: "cat /proc/cmdline",
		},
		{
			Title:         "Reboot to apply",
			Command:       "sudo systemctl reboot",
			Why:           "grubby only edits the bootloader config — the running kernel doesn't see new cmdline args until reboot",
			Verify:        "carries the new params after the reboot completes",
			VerifyCommand: "cat /proc/cmdline",
		},
	}
	detail, err := json.Marshal(map[string]any{"check_id": f.CheckID, "missing": missing, "steps": steps})
	if err != nil {
		return nil
	}
	return []ActionDraft{{
		Kind:      KindRunbook,
		Title:     "Apply missing kernel boot parameters: " + strings.Join(missing, ", "),
		Risk:      RiskInfo,
		Detail:    detail,
		DedupeKey: KindRunbook + ":kernel_params",
	}}
}

// proposeFreeMemoryRunbook turns a crit gtt_ceiling breach into a single
// informational runbook — deliberately NOT an auto unload_slot: guardrail
// 2's spirit is that the scheduler's own eviction policy frees memory and
// smith only reports, it doesn't act on the operator's behalf here.
func proposeFreeMemoryRunbook(env *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityCrit {
		return nil
	}
	var occupancy []map[string]any
	if env.Sched != nil {
		st := env.Sched.Status()
		slots := make([]string, 0, len(st.Slots))
		for slot := range st.Slots {
			slots = append(slots, slot)
		}
		sort.Strings(slots)
		for _, slot := range slots {
			mode := st.Slots[slot]
			if mode == "" {
				continue
			}
			occupancy = append(occupancy, map[string]any{
				"slot": slot, "mode": mode, "memory_bytes": st.SlotMemoryBytes[slot],
			})
		}
	}
	steps := []RunbookStep{
		{
			Title:   "Review current slot occupancy",
			Command: "curl -s localhost:5000/api/v1/status | jq .slots",
			Why:     "smith won't pick which slot to evict for you — that judgment call, and the eviction itself, stays with the scheduler's own policy or the operator (guardrail 2, §4.5)",
			Verify:  "identify the least-needed loaded slot",
		},
		{
			Title:         "Unload it",
			Command:       `curl -X POST localhost:5000/api/v1/unload -d '{"slot":"<slot to free>"}'`,
			Verify:        "the gtt_ceiling check reports ok on the next sweep",
			VerifyCommand: `curl -s -X POST localhost:5000/api/v1/smith/checks/run -d '{"check_ids":["gtt_ceiling"]}'`,
		},
	}
	detail, err := json.Marshal(map[string]any{
		"check_id": f.CheckID, "occupancy": occupancy, "steps": steps, "gtt_pct": f.Evidence["gtt_pct"],
	})
	if err != nil {
		return nil
	}
	return []ActionDraft{{
		Kind:      KindRunbook,
		Title:     "GTT ceiling breached — free memory manually",
		Risk:      RiskInfo,
		Detail:    detail,
		DedupeKey: KindRunbook + ":gtt_ceiling",
	}}
}

// proposeRebuildRunbook turns binary_versions' stale-binary list into one
// runbook proposal per stale binary — real cmake invocations rendered from
// research.md's proven recipe (§4.9's "rebuild recipes"), never something
// smith executes: recompiling a binary that a live slot may currently be
// running is exactly the kind of operation that stays behind a human.
// rebuildBehindThreshold resolves S6's drift threshold, defending against a
// zero-valued CheckEnv (tests, unwired callers): 0 would make the >=-gate
// fire on NO drift at all, proposing rebuilds for everything.
func rebuildBehindThreshold(env *CheckEnv) int {
	if env.Thresholds.BuildRefreshBehindN > 0 {
		return env.Thresholds.BuildRefreshBehindN
	}
	return DefaultThresholds().BuildRefreshBehindN
}

func proposeRebuildRunbook(env *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityInfo {
		return nil
	}
	var out []ActionDraft
	for _, b := range evidenceAs[[]binaryStatus](f.Evidence, "binaries") {
		// Upstream drift (the 2026-08-17 build-refresh addition): the
		// installed build lags upstream by N commits — a "rebuild should be
		// considered" signal. Reference the full build-refresh runbook (the
		// standardized rebase→build→test→promote→deploy procedure) rather
		// than the minimal in-place rebuild steps.
		//
		// S6: gated at build_refresh_behind_n (default 500) — below that,
		// drift is visible as an info finding without a suggestion.
		if b.UpstreamAhead >= rebuildBehindThreshold(env) {
			// G8 lesson (first live build_refresh eval, 2026-08-20): the
			// tracked binary's label can misdescribe what a refresh
			// actually touches — "llama.cpp (vulkan)" turned out to back
			// nemotron's rocm build, sharing its tree with the real
			// vulkan build and a laguna-branch build. Enumerate the real
			// blast radius off the live catalog at PROPOSAL time so the
			// operator approves against named configs, never a label.
			blast := rebuildBlastRadius(env, b.SourceRef)
			steps := []RunbookStep{
				{
					Title: "Run the full build-refresh procedure for " + b.Name,
					Command: fmt.Sprintf("cd %s && git fetch %s && git rev-list --count HEAD..%s",
						b.SourceRef, upstreamRemote(b.UpstreamRef), b.UpstreamRef),
					Why:           "binary_versions measured " + b.Name + " " + fmt.Sprintf("%d commit(s) behind %s", b.UpstreamAhead, b.UpstreamRef) + " — the installed build lags upstream" + blastRadiusSummary(blast),
					Verify:        "shows the commit count the check reported",
					VerifyCommand: fmt.Sprintf("cd %s && git rev-list --count HEAD..%s", b.SourceRef, b.UpstreamRef),
				},
				{
					Title:         "Rebase the fork onto latest upstream",
					Command:       "git fetch upstream && git rebase upstream/master",
					Why:           "the fork's patches must sit on current upstream to pick up bug fixes (the 2026-08-16 qwen38-27b device-lost was compounded by a stale base)",
					Verify:        "cleanly rebased, fork commit on top of a recent upstream sha",
					VerifyCommand: "git log --oneline -1",
				},
				{
					Title:         "Build + test + promote per the runbook",
					Command:       "see KB runbook:build-refresh (steps 2-8)",
					Why:           "dual-build vulkan+rocm, reliability + perf test, promote winner, repoint configs, SELinux-relabel deploy",
					Verify:        "winner promoted, all repointed modes load and serve",
					VerifyCommand: "smith check binary_versions",
				},
			}
			detail, err := json.Marshal(map[string]any{
				"check_id": f.CheckID, "name": b.Name, "path": b.Path, "source_ref": b.SourceRef,
				"upstream_ref": b.UpstreamRef, "upstream_ahead": b.UpstreamAhead,
				"installed_version": b.InstalledVersion, "source_version": b.SourceVersion, "steps": steps,
				"blast_radius": blast,
			})
			if err != nil {
				continue
			}
			out = append(out, ActionDraft{
				Kind:      KindRunbook,
				Title:     "Build refresh recommended: " + b.Name + " is " + strconv.Itoa(b.UpstreamAhead) + " commit(s) behind " + b.UpstreamRef,
				Risk:      RiskInfo,
				Detail:    detail,
				DedupeKey: dedupeKeyBinaryUpstreamPrefix + b.Name,
			})
			continue
		}

		if !b.Stale {
			continue
		}
		steps := []RunbookStep{
			{
				Title:         "Rebuild " + b.Name + " from its current source tree",
				Command:       fmt.Sprintf("cd %s && cmake --build build -j $(nproc)", b.SourceRef),
				Why:           "binary_versions found the installed build's --version lagging this source tree's own HEAD",
				Verify:        "reports a commit hash matching " + b.SourceRef + "'s new HEAD",
				VerifyCommand: b.Path + " --version",
			},
			{
				// No sudo: chcon only needs the file's own owner, not
				// root — confirmed live 2026-08-12 (the forge binary
				// deploy path uses the same unprivileged chcon), and a
				// build's own output is owned by whoever ran cmake, same
				// as this binary's real on-host ownership.
				Title:         "Re-apply the SELinux label (required after any llama-server rebuild)",
				Command:       "chcon -t bin_t " + b.Path,
				Why:           "SELinux relabeling is required after any rebuild; the file's own owner can do it — no sudo needed",
				Verify:        "shows bin_t",
				VerifyCommand: "ls -Z " + b.Path,
			},
		}
		detail, err := json.Marshal(map[string]any{
			"check_id": f.CheckID, "name": b.Name, "path": b.Path, "source_ref": b.SourceRef,
			"installed_version": b.InstalledVersion, "source_version": b.SourceVersion, "steps": steps,
		})
		if err != nil {
			continue
		}
		out = append(out, ActionDraft{
			Kind:      KindRunbook,
			Title:     "Rebuild recommended: " + b.Name + " lags its source tree",
			Risk:      RiskInfo,
			Detail:    detail,
			DedupeKey: KindRunbook + ":binary_stale:" + b.Name,
		})
	}
	return out
}

// upstreamRemote splits "origin/master" into the remote name ("origin").
// Returns "" when there's no slash (a bare ref can't name a remote).
func upstreamRemote(ref string) string {
	_, after, ok := strings.Cut(ref, "/")
	if !ok {
		return ""
	}
	if after == "" {
		return ""
	}
	return strings.TrimSuffix(ref, "/"+after)
}

// blastRadiusBuild is one catalog build a refresh of sourceRef would
// actually touch, with the configs that consume it — the proposal-time
// half of the G8 fix (the run-time half is build_refresh's own live
// discovery, which uses the SAME buildsUnderSourceTree rule).
type blastRadiusBuild struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Backend string   `json:"backend"`
	Configs []string `json:"configs"`
}

// rebuildBlastRadius reads the live catalog (never a cached assumption)
// and enumerates every build under sourceRef plus each build's consumer
// configs. Any failure — unwired catalog, read error — yields nil, which
// callers disclose as "unknown", never as "nothing to worry about".
func rebuildBlastRadius(env *CheckEnv, sourceRef string) []blastRadiusBuild {
	if env == nil || env.Catalog == nil || sourceRef == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	allBuilds, err := env.Catalog.ListBuilds(ctx)
	if err != nil {
		return nil
	}
	allConfigs, err := env.Catalog.ListConfigs(ctx)
	if err != nil {
		return nil
	}
	var out []blastRadiusBuild
	for _, b := range buildsUnderSourceTree(allBuilds, sourceRef) {
		var consumers []string
		for _, c := range allConfigs {
			if c.BuildID == b.ID {
				consumers = append(consumers, c.Name)
			}
		}
		out = append(out, blastRadiusBuild{ID: b.ID, Name: b.Name, Backend: b.Backend, Configs: consumers})
	}
	return out
}

// blastRadiusSummary renders the blast radius for a human-readable Why
// line. Empty when there is nothing discovered (or the catalog was
// unreadable) — a proposal that can't name its blast radius simply says
// nothing extra, rather than claiming safety it didn't verify.
func blastRadiusSummary(blast []blastRadiusBuild) string {
	if len(blast) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blast))
	for _, b := range blast {
		part := fmt.Sprintf("%s (%s", b.Name, b.Backend)
		if len(b.Configs) > 0 {
			part += "; configs: " + strings.Join(b.Configs, ", ")
		} else {
			part += "; no configs"
		}
		parts = append(parts, part+")")
	}
	return "; note: this source tree backs " + strconv.Itoa(len(blast)) +
		" catalog build(s) — a refresh touches ALL of them: " + strings.Join(parts, ", ")
}

// proposeComfyUIDelete turns comfyui_prune's candidate list into ONE
// delete_files proposal bundling every candidate from this sweep — never
// per-file, and never auto-approved (RiskHigh, always). A refused map
// (Severity warn) produces no proposal at all: proposeFrom only ever sees
// the finding, and an unbuildable map's candidates list is always empty by
// construction (comfyui.BuildMap never returns Candidates alongside a
// refusal) — so this function doesn't need its own guardrail-(a) check,
// the map builder already enforces it upstream.
func proposeComfyUIDelete(_ *CheckEnv, f Finding, _ BrainResolution) []ActionDraft {
	if f.Severity != SeverityInfo {
		return nil
	}
	candidates := evidenceAs[[]comfyui.FileInfo](f.Evidence, "candidates")
	if len(candidates) == 0 {
		return nil
	}
	var total int64
	files := make([]deleteFileEntry, 0, len(candidates))
	for _, c := range candidates {
		files = append(files, deleteFileEntry{Path: c.FullPath, FolderType: c.FolderType, SizeBytes: c.SizeBytes})
		total += c.SizeBytes
	}
	detail, err := json.Marshal(deleteFilesDetail{Files: files, TotalBytes: total, Guidance: comfyUIKeepGuidance})
	if err != nil {
		return nil
	}
	return []ActionDraft{{
		Kind:      KindDeleteFiles,
		Title:     fmt.Sprintf("Delete %d unreferenced ComfyUI model file(s) (%.1f GB)", len(files), float64(total)/(1<<30)),
		Risk:      RiskHigh,
		Detail:    detail,
		DedupeKey: KindDeleteFiles + ":comfyui_prune",
	}}
}

// ── proposeFrom: check → proposal orchestration + dedupe ────────────────────

// proposeFrom runs the proposer pass over one sweep's findings, after
// persistFindings so findingIDs lines up 1:1 with findings by index (0 for
// any finding that failed to persist). It mutates findings in place —
// findings[i].ProposalIDs is stamped with every action ID created or reused
// for that finding — and returns the flat list of every action ID touched
// this call. No-op (nil, no mutation) when Store is nil: without
// persistence there is nowhere to dedupe against or anything to link a
// finding_id to. invID, when non-nil, is stamped onto every draft as
// InvestigationID — callers running a plain sweep (checks.go) pass nil;
// RunChecksIntoInvestigation passes the investigation it's running into, so
// the §2.4.1 auto-close (maybeProposeResolution) can actually find these
// proposals (docs/v5-smith-experience.md §8 item 23 — previously always
// NULL regardless of caller).
func (s *Smith) proposeFrom(ctx context.Context, env *CheckEnv, findings []Finding, findingIDs []int64, invID *int64) []int64 {
	if s.d.Store == nil {
		return nil
	}
	br := s.Brain(ctx)
	openCount, err := s.pendingAutoProposalCount(ctx)
	if err != nil {
		s.logf("propose: count pending auto-proposals: %v", err)
		openCount = 0
	}

	var all []int64
	cappedLogged := false
	for i := range findings {
		f := findings[i]
		prop, ok := proposers[f.CheckID]
		if !ok {
			continue
		}
		drafts := prop(env, f, br)
		if len(drafts) == 0 {
			continue
		}
		var findingID *int64
		if i < len(findingIDs) && findingIDs[i] != 0 {
			id := findingIDs[i]
			findingID = &id
		}

		ids := make([]int64, 0, len(drafts))
		for _, d := range drafts {
			if openCount >= maxAutoOpenProposals {
				if !cappedLogged {
					s.logf("auto-propose cap (%d) reached, skipping further proposals this sweep", maxAutoOpenProposals)
					cappedLogged = true
				}
				break
			}
			d.CreatedBy = "smith"
			d.FindingID = findingID
			d.InvestigationID = invID
			id, inserted, err := s.createOrReuseProposal(ctx, d)
			if err != nil {
				s.logf("propose: create action for check %s: %v", f.CheckID, err)
				continue
			}
			if inserted {
				openCount++
				s.maybeAutoRunProcedure(ctx, id)
			}
			ids = append(ids, id)
			all = append(all, id)
		}
		findings[i].ProposalIDs = ids
	}
	return all
}

// pendingAutoProposalCount counts pending, smith-created actions — the
// auto-propose cap's live counter.
func (s *Smith) pendingAutoProposalCount(ctx context.Context) (int, error) {
	var n int
	err := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM smith_actions WHERE status = ? AND created_by = 'smith'`, StatusPending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("smith: count pending auto-proposals: %w", err)
	}
	return n, nil
}

// createOrReuseProposal implements the dedupe/supersede rule
// (docs/v5-smith.md §4.6): same dedupe_key + same (normalized) detail as an
// existing pending row → reuse it, no insert. Same key + changed detail →
// supersede the old pending row, insert fresh. No pending row for the key
// (never existed, or the only prior rows are rejected/done/failed) → insert
// fresh — a past rejection must not suppress a recurring problem forever.
// inserted reports whether a new row was actually created (for the caller's
// cap counter).
func (s *Smith) createOrReuseProposal(ctx context.Context, d ActionDraft) (id int64, inserted bool, err error) {
	if d.DedupeKey == "" {
		a, err := s.CreateAction(ctx, d)
		if err != nil {
			return 0, false, err
		}
		return a.ID, true, nil
	}

	existingID, existingDetail, ok, err := s.pendingByDedupeKey(ctx, d.DedupeKey)
	if err != nil {
		return 0, false, err
	}
	if ok {
		if normalizeJSON(existingDetail) == normalizeJSON(string(d.Detail)) {
			return existingID, false, nil
		}
		if _, err := s.casSuperseded(ctx, existingID); err != nil {
			return 0, false, err
		}
	}
	a, err := s.CreateAction(ctx, d)
	if err != nil {
		return 0, false, err
	}
	return a.ID, true, nil
}

// pendingByDedupeKey returns the most recent pending action for key, if any.
func (s *Smith) pendingByDedupeKey(ctx context.Context, key string) (id int64, detail string, ok bool, err error) {
	row := s.d.Store.SQL().QueryRowContext(ctx,
		`SELECT id, detail FROM smith_actions WHERE dedupe_key = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		key, StatusPending)
	if err := row.Scan(&id, &detail); err != nil {
		if err == sql.ErrNoRows {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf("smith: lookup pending proposal for %q: %w", key, err)
	}
	return id, detail, true, nil
}

// casSuperseded CAS's a pending action to superseded.
func (s *Smith) casSuperseded(ctx context.Context, id int64) (bool, error) {
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		StatusSuperseded, s.d.Now().Unix(), id, StatusPending)
	if err != nil {
		return false, fmt.Errorf("smith: supersede proposal %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// normalizeJSON re-marshals a JSON string so semantically-identical detail
// blobs compare equal regardless of key order (encoding/json sorts map keys
// on Marshal). Falls back to the raw string on unparseable input so a
// malformed value never crashes the dedupe comparison — it will simply
// never equal anything and always insert fresh, the safe default.
func normalizeJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// SPDX-License-Identifier: Apache-2.0

package smith

// answers.go implements the smith fast-path answer synthesis engine
// (docs/v5-smith-experience.md §2.2 FAST ANSWER step, §3.1, §3.2, §3.3).
// Given a classified Intent, it produces a specific, sourced answer in plain
// language — citing the live source and "checked just now" — or reports
// (ok=false) when the fast path can't answer (caller THINKs).
//
// The fast path never persists findings and never creates proposals (§3.2) —
// EXCEPT action intents, which create proposals by design (§2.4). Full action
// drafting is Sprint S3; S2-Go implements classification + a basic answer
// describing what smith would do, not the full restart-resolution loop.
//
// Offline guarantee (§2.2): the fast path works with smith.model="" — no LLM
// is involved anywhere. Every answer passes the redaction pass (redact.go)
// like all context the daemon produces.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FastAnswer is the result of a fast-path answer attempt.
type FastAnswer struct {
	Text      string           // the plain-language answer
	Evidence  []AnswerEvidence // expandable evidence detail
	ActionID  *int64           // optional: linked action proposal (remedy)
	DigDeeper bool             // always true — the "dig deeper" chip
}

// AnswerEvidence is one expandable evidence row under a fast answer.
type AnswerEvidence struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Answer attempts a fast-path answer for the given intent. Returns
// (answer, true) if the fast path fully answered, or (zero, false) if the
// fast path can't answer (caller should THINK). The fast path never persists
// findings and never creates proposals (§3.2) — EXCEPT action intents which
// create proposals by design (§2.4).
func (s *Smith) Answer(ctx context.Context, intent Intent) (FastAnswer, bool) {
	switch intent.Family {
	case FamilyHealth:
		return s.answerHealth(ctx, intent)
	case FamilyVersion:
		return s.answerVersion(ctx, intent)
	case FamilyQuantity:
		return s.answerQuantity(ctx, intent)
	case FamilyReachability:
		return s.answerReachability(ctx, intent)
	case FamilyListing:
		return s.answerListing(ctx, intent)
	case FamilyHistory:
		return s.answerHistory(ctx, intent)
	case FamilyLogs:
		return s.answerLogs(ctx, intent)
	case FamilyKB:
		return s.answerKB(ctx, intent)
	case FamilyAction:
		return s.answerAction(ctx, intent)
	}
	return FastAnswer{}, false
}

// digDeeperChip is the universal escalation affordance appended to every
// fast answer (§2.2: "Every fast answer ends with the dig deeper chip, so
// escalation is always one click").
const digDeeperChip = "\n\n_Dig deeper — ask a follow-up and smith will think it through._"

// finalizeAnswer stamps the dig-deeper chip and redacts the text.
func (a FastAnswer) finalize() FastAnswer {
	a.Text = scrubSecretPatterns(a.Text)
	if !a.DigDeeper {
		a.DigDeeper = true
	}
	if !strings.HasSuffix(strings.TrimSpace(a.Text), "_Dig deeper") {
		a.Text = strings.TrimRight(a.Text, "\n") + digDeeperChip
	}
	for i := range a.Evidence {
		a.Evidence[i].Value = scrubSecretPatterns(a.Evidence[i].Value)
	}
	return a
}

// gapAnswer is the honest "I can't see that" answer for known gaps
// (answerable=false in the fixture).
func gapAnswer(entity, reason string) (FastAnswer, bool) {
	txt := "I can't see that right now"
	if reason != "" {
		txt += " — " + reason
	}
	return FastAnswer{Text: txt, DigDeeper: true}.finalize(), true
}

// ── health ────────────────────────────────────────────────────────────────

// entityCheck maps a health entity to the check that owns it. "" = no direct
// check (answered from a different seam).
func entityCheck(entity string) string {
	switch entity {
	case "comfyui":
		return "comfyui_health"
	case "compressor":
		return "compressor_reachability"
	case "a0":
		return "a0_reachability"
	case "forge":
		return "forge_self"
	case "brain":
		return "brain_resolvable"
	case "gpu":
		return "gpu_hang"
	case "embedding", "stt", "tts", "aligner":
		return "always_on_ports"
	case "a1", "a2", "a3", "a4":
		return "slot_agreement"
	}
	return ""
}

func (s *Smith) answerHealth(ctx context.Context, intent Intent) (FastAnswer, bool) {
	entity := intent.Entity
	if entity == "" {
		return FastAnswer{}, false
	}

	// search / internet: web research capability health.
	if entity == "search" || entity == "internet" {
		return s.answerWebHealth(ctx, entity)
	}

	// Slots a1–a4: read sched.Status directly (no check needed).
	if isSlotEntity(entity) {
		return s.answerSlotHealth(ctx, entity)
	}

	checkID := entityCheck(entity)
	if checkID == "" {
		return FastAnswer{}, false
	}
	f := s.runOneCheck(ctx, checkID)
	return s.healthAnswerFromFinding(entity, f), true
}

// runOneCheck runs a single check live via runSelected — lock-free,
// non-persisting (§3.2). Returns a zero finding on any failure (the answer
// degrades to "couldn't check just now").
func (s *Smith) runOneCheck(ctx context.Context, checkID string) Finding {
	env := s.checkEnv(ctx)
	for _, c := range registry {
		if c.ID == checkID {
			return runOne(ctx, c, env)
		}
	}
	return Finding{CheckID: checkID, Severity: SeverityInfo, Summary: "check not found"}
}

// healthAnswerFromFinding synthesizes a specific Yes/No answer from a check
// finding, citing the live source + "checked just now".
func (s *Smith) healthAnswerFromFinding(entity string, f Finding) FastAnswer {
	label := entityLabel(entity)
	up := f.Severity == SeverityOK || f.Severity == SeverityInfo
	// comfyui_health reports "down" with severity info (deliberate — a down
	// ComfyUI only gates pruning/GTT, not core health), so severity can't
	// tell up from down here; read the real signal from its evidence and
	// explain the reason rather than just "unreachable".
	if entity == "comfyui" {
		up = evidenceAs[bool](f.Evidence, "unit_active") && evidenceAs[bool](f.Evidence, "port_up")
	}
	var b strings.Builder
	if up {
		fmt.Fprintf(&b, "Yes — %s is %s (checked just now).", label, healthAdjective(entity, f))
	} else if entity == "comfyui" {
		fmt.Fprintf(&b, "No — %s is unreachable: %s (checked just now).", label,
			comfyUIDownReason(
				evidenceAs[string](f.Evidence, "unit"),
				evidenceAs[bool](f.Evidence, "unit_active"),
				evidenceAs[string](f.Evidence, "unit_state"),
				evidenceAs[string](f.Evidence, "unit_result"),
				evidenceAs[int32](f.Evidence, "unit_exec_main_status"),
				evidenceAs[int](f.Evidence, "port")))
	} else {
		fmt.Fprintf(&b, "No — %s %s (checked just now).", label, f.Summary)
	}
	ev := []AnswerEvidence{
		{Label: "check", Value: f.CheckID},
		{Label: "severity", Value: string(f.Severity)},
		{Label: "summary", Value: f.Summary},
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize()
}

func (s *Smith) answerSlotHealth(ctx context.Context, entity string) (FastAnswer, bool) {
	label := entityLabel(entity)
	if s.d.Sched == nil {
		return FastAnswer{}, false
	}
	st := s.d.Sched.Status()
	mode, ok := st.Slots[entity]
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "slot", Value: entity}}
	if !ok || mode == "" {
		fmt.Fprintf(&b, "%s is empty right now (checked just now).", label)
		ev = append(ev, AnswerEvidence{Label: "state", Value: "empty"})
	} else {
		fmt.Fprintf(&b, "Yes — %s is up, running %s (checked just now).", label, mode)
		ev = append(ev, AnswerEvidence{Label: "mode", Value: mode})
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerWebHealth(ctx context.Context, entity string) (FastAnswer, bool) {
	providers := s.WebProviders(ctx)
	enabled := s.WebConfig(ctx).Enabled
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "web_enabled", Value: fmt.Sprintf("%v", enabled)}}
	if !enabled {
		fmt.Fprintf(&b, "Web research is off right now (checked just now).")
	} else if entity == "internet" {
		// Direct probe: any reachable provider means yes.
		reachable := false
		for _, p := range providers {
			if p.Reachable {
				reachable = true
			}
			ev = append(ev, AnswerEvidence{Label: p.Name, Value: fmt.Sprintf("reachable=%v", p.Reachable)})
		}
		if reachable {
			fmt.Fprintf(&b, "Yes — smith can reach the internet right now (checked just now).")
		} else {
			fmt.Fprintf(&b, "No — none of smith's web providers are reachable right now (checked just now).")
		}
	} else { // search
		searchOK := false
		for _, p := range providers {
			if p.Role == "search" && p.Reachable {
				searchOK = true
			}
			ev = append(ev, AnswerEvidence{Label: p.Name, Value: fmt.Sprintf("role=%s reachable=%v", p.Role, p.Reachable)})
		}
		if searchOK {
			fmt.Fprintf(&b, "Yes — web search is working right now (checked just now).")
		} else {
			fmt.Fprintf(&b, "No — web search is not working right now (checked just now).")
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// ── version ───────────────────────────────────────────────────────────────

func (s *Smith) answerVersion(ctx context.Context, intent Intent) (FastAnswer, bool) {
	entity := intent.Entity
	if entity == "" {
		return FastAnswer{}, false
	}
	// Known gaps (fixture answerable=false).
	if entity == "comfyui" {
		return gapAnswer(entity, "no reliable ComfyUI version seam — /opt/comfyui isn't a git repo and /system_stats needs ComfyUI running")
	}
	if entity == "forge" {
		return gapAnswer(entity, "forge version is not tracked (the binary is cross-compiled and shipped, no on-box source tree to compare)")
	}
	// tailscale: read via the tailscale CLI seam (Sprint R verified `tailscale version`).
	if entity == "tailscale" {
		return s.answerTailscaleVersion(ctx)
	}
	// Tracked binaries (llama.cpp variants, headroom-ai): from the
	// binary_versions check, which compares installed --version vs source tree.
	if entity == "headroom-ai" || strings.HasPrefix(entity, "llama.cpp") {
		return s.answerBinaryVersion(ctx, entity)
	}
	return FastAnswer{}, false
}

func (s *Smith) answerTailscaleVersion(ctx context.Context) (FastAnswer, bool) {
	// The tailscale CLI seam (tailscale_peers uses `tailscale status --json`;
	// `tailscale version` is the first line). No Deps seam for version yet —
	// answer honestly that the version isn't surfaced through a check.
	return gapAnswer("tailscale", "tailscale version (1.102.2 live) is readable via `tailscale version` but not yet wired into a check or smith's Deps seam")
}

func (s *Smith) answerBinaryVersion(ctx context.Context, entity string) (FastAnswer, bool) {
	f := s.runOneCheck(ctx, "binary_versions")
	ev := []AnswerEvidence{
		{Label: "check", Value: "binary_versions"},
		{Label: "severity", Value: string(f.Severity)},
		{Label: "summary", Value: f.Summary},
	}
	var b strings.Builder
	if binaries, ok := f.Evidence["binaries"].([]any); ok {
		for _, bi := range binaries {
			if m, ok := bi.(map[string]any); ok {
				if name, _ := m["name"].(string); strings.EqualFold(name, entity) {
					installed, _ := m["installed_version"].(string)
					source, _ := m["source_version"].(string)
					stale, _ := m["stale"].(bool)
					fmt.Fprintf(&b, "%s is at %s", entity, installed)
					if source != "" {
						fmt.Fprintf(&b, " (source tree: %s)", source)
					}
					if stale {
						fmt.Fprintf(&b, " — STALE, the installed build lags its source tree (checked just now).")
					} else {
						fmt.Fprintf(&b, " — up to date with its source tree (checked just now).")
					}
					ev = append(ev, AnswerEvidence{Label: "installed", Value: installed}, AnswerEvidence{Label: "source", Value: source})
					return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
				}
			}
		}
	}
	// Fallback: report the check's summary if the specific binary wasn't found.
	if f.Severity != "" {
		fmt.Fprintf(&b, "%s — %s (checked just now).", entity, f.Summary)
		return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
	}
	return gapAnswer(entity, "binary version tracking isn't available right now")
}

// ── quantity ──────────────────────────────────────────────────────────────

func (s *Smith) answerQuantity(ctx context.Context, intent Intent) (FastAnswer, bool) {
	sc := s.SelfContext(ctx)
	if sc.Metrics == nil {
		return gapAnswer(intent.Entity, "no metrics snapshot available right now")
	}
	m := sc.Metrics
	switch intent.Entity {
	case "ram":
		free := m.MemAvailBytes
		total := m.MemTotalBytes
		if total <= 0 {
			return gapAnswer("ram", "memory metrics unavailable this cycle")
		}
		pct := 100 - m.MemPct
		if free > 0 {
			return FastAnswer{
				Text:      fmt.Sprintf("%.0f GB of %.0f GB free (%.0f%% available) — checked just now.", float64(free)/1e9, float64(total)/1e9, pct),
				Evidence:  []AnswerEvidence{{Label: "mem_avail_bytes", Value: fmt.Sprintf("%d", free)}, {Label: "mem_total_bytes", Value: fmt.Sprintf("%d", total)}, {Label: "mem_pct_used", Value: fmt.Sprintf("%.1f", m.MemPct)}},
				DigDeeper: true,
			}.finalize(), true
		}
		return gapAnswer("ram", "memory metrics unavailable this cycle")
	case "gtt":
		if m.GTTUsedBytes == nil || m.GTTTotalBytes == nil || *m.GTTTotalBytes <= 0 {
			return gapAnswer("gtt", "GTT metrics unavailable this cycle")
		}
		used, total := *m.GTTUsedBytes, *m.GTTTotalBytes
		free := total - used
		pct := pctOf(used, total)
		return FastAnswer{
			Text:      fmt.Sprintf("%.1f GB of %.1f GB GTT free (%.1f%% used) — checked just now.", float64(free)/1e9, float64(total)/1e9, pct),
			Evidence:  []AnswerEvidence{{Label: "gtt_used_bytes", Value: fmt.Sprintf("%d", used)}, {Label: "gtt_total_bytes", Value: fmt.Sprintf("%d", total)}, {Label: "gtt_pct", Value: fmt.Sprintf("%.1f", pct)}},
			DigDeeper: true,
		}.finalize(), true
	case "disk":
		if m.DiskTotalBytes <= 0 {
			return gapAnswer("disk", "disk metrics unavailable this cycle")
		}
		return FastAnswer{
			Text:      fmt.Sprintf("%.0f GB of %.0f GB free (%.1f%% used) — checked just now.", float64(m.DiskFreeBytes)/1e9, float64(m.DiskTotalBytes)/1e9, m.DiskPct),
			Evidence:  []AnswerEvidence{{Label: "disk_free_bytes", Value: fmt.Sprintf("%d", m.DiskFreeBytes)}, {Label: "disk_total_bytes", Value: fmt.Sprintf("%d", m.DiskTotalBytes)}, {Label: "disk_pct", Value: fmt.Sprintf("%.1f", m.DiskPct)}},
			DigDeeper: true,
		}.finalize(), true
	case "n_ctx":
		f := s.runOneCheck(ctx, "n_ctx_actual")
		return FastAnswer{
			Text:      f.Summary + " (checked just now).",
			Evidence:  []AnswerEvidence{{Label: "check", Value: "n_ctx_actual"}, {Label: "severity", Value: string(f.Severity)}, {Label: "summary", Value: f.Summary}},
			DigDeeper: true,
		}.finalize(), true
	}
	return FastAnswer{}, false
}

// ── reachability ──────────────────────────────────────────────────────────

// meshAddress looks up a mesh service entity's address in the live
// smith.mesh.services inventory (migration 0060 seeds it; operator-edited
// per deployment — open-source-readiness finding 1: the mesh map is
// deployment data, discovered at answer time, never compiled in).
func (s *Smith) meshAddress(ctx context.Context, entity string) (string, bool) {
	for _, svc := range s.MeshServices(ctx) {
		if svc.Name == entity {
			return svc.Address, true
		}
	}
	return "", false
}

// meshEntityHasAlias reports whether the named smith.mesh.services entry
// carries the given alias exactly. Used to recognize deployment-local
// services (e.g. the local ComfyUI) without hardcoding any host's naming.
func (s *Smith) meshEntityHasAlias(ctx context.Context, entity, alias string) bool {
	for _, svc := range s.MeshServices(ctx) {
		if svc.Name != entity {
			continue
		}
		for _, a := range svc.Aliases {
			if a == alias {
				return true
			}
		}
	}
	return false
}

func (s *Smith) answerReachability(ctx context.Context, intent Intent) (FastAnswer, bool) {
	entity := intent.Entity
	if entity == "" {
		return FastAnswer{}, false
	}
	// tailnet/tailscale: mesh connection state.
	if entity == "tailnet" {
		return s.answerTailnetReachability(ctx)
	}
	// internet: web probe.
	if entity == "internet" {
		return s.answerWebHealth(ctx, "internet")
	}
	addr, ok := s.meshAddress(ctx, entity)
	if !ok {
		// Slot entities (a1–a4): report the scheduler view.
		if isSlotEntity(entity) {
			return s.answerSlotHealth(ctx, entity)
		}
		return FastAnswer{}, false
	}
	// Compose the inventory address + live reachability evidence.
	var b strings.Builder
	fmt.Fprintf(&b, "The address is %s", addr)
	ev := []AnswerEvidence{{Label: "address", Value: addr}, {Label: "source", Value: "settings: " + SettingMeshServices}}
	// Live probe: if this entity maps to a health check, run it.
	checkID := entityCheck(entity)
	if checkID != "" && checkID != "slot_agreement" {
		f := s.runOneCheck(ctx, checkID)
		up := f.Severity == SeverityOK || f.Severity == SeverityInfo
		if up {
			b.WriteString(" — and yes, it's reachable right now (checked just now).")
		} else {
			fmt.Fprintf(&b, " — but it's not reachable right now: %s (checked just now).", f.Summary)
		}
		ev = append(ev, AnswerEvidence{Label: "check", Value: checkID}, AnswerEvidence{Label: "live", Value: string(f.Severity)})
	} else if s.meshEntityHasAlias(ctx, entity, "comfy") || s.meshEntityHasAlias(ctx, entity, "comfyui") {
		// ComfyUI reachability: this mesh entry is the deployment's local
		// ComfyUI (it carries the curated "comfy"/"comfyui" alias), so its
		// reachability is probeable via comfyui_health. Remote instances name
		// their aliases distinctly ("<host> comfyui") and stay address-only.
		f := s.runOneCheck(ctx, "comfyui_health")
		up := f.Severity == SeverityOK || f.Severity == SeverityInfo
		if up {
			b.WriteString(" — and yes, it's reachable via tailnet right now (checked just now).")
		} else {
			fmt.Fprintf(&b, " — but it's not reachable right now: %s (checked just now).", f.Summary)
		}
		ev = append(ev, AnswerEvidence{Label: "check", Value: "comfyui_health"}, AnswerEvidence{Label: "live", Value: string(f.Severity)})
	} else {
		b.WriteString(" (address from the mesh inventory — checked just now).")
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerTailnetReachability(ctx context.Context) (FastAnswer, bool) {
	f := s.runOneCheck(ctx, "tailscale_peers")
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "check", Value: "tailscale_peers"}, {Label: "severity", Value: string(f.Severity)}}
	if f.Severity == SeverityOK || f.Severity == SeverityInfo {
		b.WriteString("Yes — the tailnet is connected (checked just now).")
	} else {
		fmt.Fprintf(&b, "No — the tailnet check reports: %s (checked just now).", f.Summary)
	}
	ev = append(ev, AnswerEvidence{Label: "summary", Value: f.Summary})
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// ── listing ───────────────────────────────────────────────────────────────

func (s *Smith) answerListing(ctx context.Context, intent Intent) (FastAnswer, bool) {
	switch intent.Entity {
	case "pending_tasks":
		return s.answerPendingTasks(ctx)
	case "pending_actions":
		return s.answerPendingActions(ctx)
	case "open_investigations":
		return s.answerOpenInvestigations(ctx)
	case "backlog":
		return s.answerBacklog(ctx)
	case "degraded_services":
		return s.answerDegradedServices(ctx)
	}
	return FastAnswer{}, false
}

func (s *Smith) answerPendingTasks(ctx context.Context) (FastAnswer, bool) {
	if s.d.Sched == nil {
		return gapAnswer("pending_tasks", "scheduler not wired")
	}
	st := s.d.Sched.Status()
	q := st.Queue
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "queue_depth", Value: fmt.Sprintf("%d", len(q))}}
	if len(q) == 0 {
		b.WriteString("Nothing is pending in the scheduler queue right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d task(s) in the scheduler queue (checked just now):", len(q))
		for i, t := range q {
			if i >= 5 {
				fmt.Fprintf(&b, "\n- … +%d more", len(q)-5)
				break
			}
			fmt.Fprintf(&b, "\n- %s (%s)", t.Model, t.Status)
			ev = append(ev, AnswerEvidence{Label: fmt.Sprintf("task_%d", i), Value: t.Model})
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerPendingActions(ctx context.Context) (FastAnswer, bool) {
	actions, err := s.ListActions(ctx, StatusPending, nil, 10)
	if err != nil {
		return gapAnswer("pending_actions", "couldn't read smith actions")
	}
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "count", Value: fmt.Sprintf("%d", len(actions))}}
	if len(actions) == 0 {
		b.WriteString("No pending smith proposals right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d pending proposal(s) (checked just now):", len(actions))
		for _, a := range actions {
			fmt.Fprintf(&b, "\n- #%d %s (%s, %s)", a.ID, a.Title, a.Kind, a.Risk)
			ev = append(ev, AnswerEvidence{Label: fmt.Sprintf("action_%d", a.ID), Value: a.Title})
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerOpenInvestigations(ctx context.Context) (FastAnswer, bool) {
	invs, err := s.ListInvestigations(ctx, "")
	if err != nil {
		return gapAnswer("open_investigations", "couldn't read investigations")
	}
	open := 0
	for _, inv := range invs {
		if inv.Status == "open" {
			open++
		}
	}
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "open", Value: fmt.Sprintf("%d", open)}, {Label: "total", Value: fmt.Sprintf("%d", len(invs))}}
	if open == 0 {
		b.WriteString("No open investigations right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d open investigation(s) (checked just now):", open)
		for _, inv := range invs {
			if inv.Status != "open" {
				continue
			}
			fmt.Fprintf(&b, "\n- #%d %s", inv.ID, inv.Summary)
			ev = append(ev, AnswerEvidence{Label: fmt.Sprintf("inv_%d", inv.ID), Value: inv.Summary})
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerBacklog(ctx context.Context) (FastAnswer, bool) {
	items := s.ListBlockedItems()
	open := 0
	for _, it := range items {
		if it.Status == "open" {
			open++
		}
	}
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "open", Value: fmt.Sprintf("%d", open)}, {Label: "total", Value: fmt.Sprintf("%d", len(items))}}
	if len(items) == 0 {
		b.WriteString("The backlog is empty right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d of %d backlog item(s) are still open (checked just now):", open, len(items))
		for _, it := range items {
			if it.Status != "open" {
				continue
			}
			fmt.Fprintf(&b, "\n- #%d %s", it.Number, it.Title)
			ev = append(ev, AnswerEvidence{Label: fmt.Sprintf("item_%d", it.Number), Value: it.Title})
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerDegradedServices(ctx context.Context) (FastAnswer, bool) {
	// Aggregate always_on_ports + comfyui_health down findings.
	f := s.runOneCheck(ctx, "always_on_ports")
	down := []string{}
	if downs, ok := f.Evidence["down"].([]any); ok {
		for _, d := range downs {
			if m, ok := d.(map[string]any); ok {
				if svc, _ := m["service"].(string); svc != "" {
					down = append(down, svc)
				}
			}
		}
	}
	// ComfyUI is a known-degraded service when its check isn't OK.
	cf := s.runOneCheck(ctx, "comfyui_health")
	if cf.Severity != SeverityOK && cf.Severity != SeverityInfo && cf.Severity != "" {
		down = append(down, "comfyui")
	}
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "degraded_count", Value: fmt.Sprintf("%d", len(down))}}
	if len(down) == 0 {
		b.WriteString("No services are degraded right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d service(s) degraded right now (checked just now): %s", len(down), strings.Join(down, ", "))
		ev = append(ev, AnswerEvidence{Label: "degraded", Value: strings.Join(down, ", ")})
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// ── history ───────────────────────────────────────────────────────────────

func (s *Smith) answerHistory(ctx context.Context, intent Intent) (FastAnswer, bool) {
	if intent.Entity == "" {
		return FastAnswer{}, false
	}
	if intent.Entity == "any_finding_by_text" {
		return gapAnswer("any_finding_by_text", "tell me which finding or error you mean — paste the error text or name the check, and I'll look up how long it's been happening")
	}
	dur, err := s.FindingDuration(ctx, intent.Entity)
	if err != nil {
		return gapAnswer(intent.Entity, "couldn't read findings history")
	}
	count, repeats, err := s.FindingFrequency(ctx, intent.Entity, 7*24*time.Hour)
	if err != nil {
		return gapAnswer(intent.Entity, "couldn't read findings history")
	}
	var b strings.Builder
	ev := []AnswerEvidence{
		{Label: "check", Value: intent.Entity},
		{Label: "oldest_seen", Value: fmt.Sprintf("%s ago", roundDuration(dur))},
		{Label: "rows_7d", Value: fmt.Sprintf("%d", count)},
		{Label: "total_repeats_7d", Value: fmt.Sprintf("%d", repeats)},
	}
	if count == 0 {
		fmt.Fprintf(&b, "%s hasn't fired in the last 7 days (checked just now).", intent.Entity)
	} else {
		fmt.Fprintf(&b, "%s has been happening for %s — %d finding(s) in the last 7 days (checked just now).",
			intent.Entity, roundDuration(dur), count)
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// ── logs ──────────────────────────────────────────────────────────────────

func (s *Smith) answerLogs(ctx context.Context, intent Intent) (FastAnswer, bool) {
	if intent.Entity == "notifications" {
		return s.answerNotifications(ctx)
	}
	// forge_unit_error_digest: bounded journal read (§8 item 16).
	return s.answerJournalDigest(ctx)
}

func (s *Smith) answerNotifications(ctx context.Context) (FastAnswer, bool) {
	if s.d.Store == nil {
		return gapAnswer("notifications", "store not wired")
	}
	notifs, err := s.d.Store.Notifications().List(ctx, false)
	if err != nil {
		return gapAnswer("notifications", "couldn't read notifications")
	}
	var b strings.Builder
	ev := []AnswerEvidence{{Label: "count", Value: fmt.Sprintf("%d", len(notifs))}}
	if len(notifs) == 0 {
		b.WriteString("No active notifications right now (checked just now).")
	} else {
		fmt.Fprintf(&b, "%d active notification(s) (checked just now):", len(notifs))
		for i, n := range notifs {
			if i >= 5 {
				fmt.Fprintf(&b, "\n- … +%d more", len(notifs)-5)
				break
			}
			fmt.Fprintf(&b, "\n- [%s] %s: %s", n.Severity, n.Code, n.Message)
			ev = append(ev, AnswerEvidence{Label: n.Code, Value: n.Message})
		}
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerJournalDigest(ctx context.Context) (FastAnswer, bool) {
	if s.d.JournalErrors == nil {
		// Fall back to notifications when the journal seam isn't wired.
		return s.answerNotifications(ctx)
	}
	lines, err := s.d.JournalErrors(ctx, journalDigestLines, time.Time{})
	if err != nil || len(lines) == 0 {
		return gapAnswer("forge_unit_error_digest", "no error lines in the forge-* unit journals right now")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last %d error line(s) from forge-* unit journals (checked just now):", len(lines))
	ev := []AnswerEvidence{{Label: "lines", Value: fmt.Sprintf("%d", len(lines))}}
	for i, ln := range lines {
		if i >= journalDigestLines {
			break
		}
		fmt.Fprintf(&b, "\n- %s", ln)
		ev = append(ev, AnswerEvidence{Label: fmt.Sprintf("line_%d", i+1), Value: ln})
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// journalDigestLines bounds the forge-unit error digest (§8 item 16).
const journalDigestLines = 20

// ── kb ────────────────────────────────────────────────────────────────────

func (s *Smith) answerKB(ctx context.Context, intent Intent) (FastAnswer, bool) {
	if intent.Entity == "" {
		return gapAnswer("kb", "I couldn't find a matching knowledge-base entry for that")
	}
	// Resolve the slug back to a KB chunk and answer from it.
	results, err := s.KBSearch(ctx, intent.Entity, 1)
	if err == nil && len(results) > 0 {
		r := results[0]
		body := truncateForContext(r.Body, 800)
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n\n%s (from %s:%s — checked just now).", r.Title, body, r.Kind, r.Ref)
		ev := []AnswerEvidence{{Label: "ref", Value: r.Ref}, {Label: "kind", Value: r.Kind}, {Label: "title", Value: r.Title}}
		return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
	}
	// Direct chunk lookup by ref (kb slug may be a ref suffix).
	if c, ok := s.KBLookup(intent.Entity); ok {
		body := truncateForContext(c.Body, 800)
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n\n%s (from the KB — checked just now).", c.Title, body)
		ev := []AnswerEvidence{{Label: "ref", Value: c.Ref}, {Label: "source", Value: c.Source}}
		return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
	}
	return gapAnswer("kb", "I couldn't find a matching knowledge-base entry for that")
}

// ── action ────────────────────────────────────────────────────────────────

// restartUnitEntities maps a FamilyAction entity (e.g. "restart_forge-stt")
// to the real systemd unit name. The TTS unit is read from config at runtime
// (cfg.Server.TTSUnit) since it's deployment-specific.
var restartUnitEntities = map[string]string{
	"restart_forge-stt":       "forge-stt",
	"restart_forge-embedding": "forge-embedding",
	"restart_forge-aligner":   "forge-aligner",
	"restart_comfyui":         "forge-comfyui",
	"restart_compressor":      "headroom@local",
}

func (s *Smith) answerAction(ctx context.Context, intent Intent) (FastAnswer, bool) {
	entity := intent.Entity
	if entity == "" {
		return FastAnswer{}, false
	}
	// Known refusals (fixture answerable=false): restart_forge-daemon,
	// restart_slot_unit. Answer with the plain-language reason from
	// execute.go's restartAllowed.
	if entity == "restart_forge-daemon" {
		return FastAnswer{Text: "I won't restart forge-daemon — restarting the daemon would kill the executing action itself. If a restart is genuinely needed, do it yourself outside smith (checked just now).", DigDeeper: true}.finalize(), true
	}
	if entity == "restart_slot_unit" {
		return FastAnswer{Text: "I won't restart a scheduler slot unit directly — that breaks slot agreement. Use the scheduler-mediated path instead (unload + reload the config). Ask me to \"restart llama.cpp\" and I'll propose that (checked just now).", DigDeeper: true}.finalize(), true
	}
	// run_check_up: run the quick sweep (persisting — this one IS a sweep).
	if entity == "run_check_up" {
		return s.answerRunCheckUp(ctx)
	}
	// restart_llama.cpp: scheduler-mediated unload + reload (§3.5).
	if entity == "restart_llama.cpp" {
		return s.answerRestartLlamaCpp(ctx, intent)
	}
	// Allowlisted restart_forge_unit drafts.
	if unit, ok := restartUnitEntities[entity]; ok {
		return s.answerRestartUnit(ctx, intent, entity, unit)
	}
	// TTS unit is config-driven, so resolve it at runtime.
	if entity == "restart_tts" {
		unit := "forge-tts"
		if cfg := s.cfg(); cfg != nil && cfg.Server.TTSUnit != "" {
			unit = cfg.Server.TTSUnit
		}
		return s.answerRestartUnit(ctx, intent, entity, unit)
	}
	return FastAnswer{Text: fmt.Sprintf("I'd take that action — %s. (I can't draft this one automatically yet — trigger it from Diagnostics or approve an auto-proposal if one exists. Checked just now.)", entity), DigDeeper: true}.finalize(), true
}

// answerRestartUnit creates a restart_forge_unit proposal via CreateAction
// for an allowlisted unit (§3.5 case 1). The action is linked to the active
// conversation (intent.ConversationID) so the action-kind transcript message
// can be appended by Chat(). Returns the FastAnswer with ActionID set so
// Chat() knows to append the action card.
func (s *Smith) answerRestartUnit(ctx context.Context, intent Intent, entity, unit string) (FastAnswer, bool) {
	cfg := s.cfg()
	if allowed, reason := restartAllowed(cfg, unit); !allowed {
		return FastAnswer{Text: fmt.Sprintf("I won't restart %s — %s (checked just now).", unit, reason), DigDeeper: true}.finalize(), true
	}
	detail, err := json.Marshal(restartUnitDetail{Unit: unit})
	if err != nil {
		return FastAnswer{}, false
	}
	draft := ActionDraft{
		Kind:           KindRestartForgeUnit,
		Title:          "Restart " + unit,
		Risk:           RiskLow,
		Detail:         detail,
		DedupeKey:      KindRestartForgeUnit + ":" + unit,
		ConversationID: intent.ConversationID,
		CreatedBy:      "smith",
	}
	a, err := s.CreateAction(ctx, draft)
	if err != nil {
		return gapAnswer(entity, "couldn't create the restart proposal ("+err.Error()+")")
	}
	id := a.ID
	text := fmt.Sprintf("I've drafted a proposal to restart %s — approve it below and I'll run it (checked just now).", unit)
	ev := []AnswerEvidence{
		{Label: "action_id", Value: fmt.Sprintf("%d", id)},
		{Label: "unit", Value: unit},
		{Label: "risk", Value: RiskLow},
	}
	return FastAnswer{Text: text, Evidence: ev, ActionID: &id, DigDeeper: true}.finalize(), true
}

// answerRestartLlamaCpp implements §3.5 case 2: "restart llama.cpp" maps to
// scheduler-mediated unload + reload, never a raw slot unit restart. For each
// loaded slot, creates two linked proposals (unload_slot + load_config) via
// CreateAction. If nothing is loaded, answers honestly.
func (s *Smith) answerRestartLlamaCpp(ctx context.Context, intent Intent) (FastAnswer, bool) {
	if s.d.Sched == nil {
		return gapAnswer("restart_llama.cpp", "scheduler not wired — can't find loaded slots")
	}
	st := s.d.Sched.Status()
	type loadedSlot struct {
		slot string
		mode string
	}
	var loaded []loadedSlot
	for slot, mode := range st.Slots {
		if mode != "" {
			loaded = append(loaded, loadedSlot{slot: slot, mode: mode})
		}
	}
	if len(loaded) == 0 {
		return FastAnswer{Text: "No llama.cpp model is loaded right now — nothing to restart (checked just now).", DigDeeper: true}.finalize(), true
	}

	var b strings.Builder
	ev := []AnswerEvidence{}
	var firstActionID *int64
	for _, ls := range loaded {
		unloadDetail, err := json.Marshal(unloadSlotDetail{Slot: ls.slot})
		if err != nil {
			continue
		}
		loadDetail, err := json.Marshal(loadConfigDetail{Mode: ls.mode, Slot: ls.slot})
		if err != nil {
			continue
		}
		unloadDraft := ActionDraft{
			Kind:           KindUnloadSlot,
			Title:          fmt.Sprintf("Unload %s from slot %s (restart llama.cpp)", ls.mode, strings.ToUpper(ls.slot)),
			Risk:           RiskHigh,
			Detail:         unloadDetail,
			DedupeKey:      KindUnloadSlot + ":" + ls.slot,
			ConversationID: intent.ConversationID,
			CreatedBy:      "smith",
		}
		unloadAction, err := s.CreateAction(ctx, unloadDraft)
		if err != nil {
			s.logf("answerRestartLlamaCpp: create unload for %s: %v", ls.slot, err)
			continue
		}
		loadDraft := ActionDraft{
			Kind:           KindLoadConfig,
			Title:          fmt.Sprintf("Reload %s into slot %s (restart llama.cpp)", ls.mode, strings.ToUpper(ls.slot)),
			Risk:           RiskLow,
			Detail:         loadDetail,
			DedupeKey:      KindLoadConfig + ":" + ls.mode + ":" + ls.slot,
			ConversationID: intent.ConversationID,
			CreatedBy:      "smith",
		}
		loadAction, err := s.CreateAction(ctx, loadDraft)
		if err != nil {
			s.logf("answerRestartLlamaCpp: create load for %s/%s: %v", ls.mode, ls.slot, err)
			continue
		}
		if firstActionID == nil {
			firstActionID = &unloadAction.ID
		}
		fmt.Fprintf(&b, "- unload %s from slot %s (proposal #%d), then reload it (proposal #%d)\n", ls.mode, strings.ToUpper(ls.slot), unloadAction.ID, loadAction.ID)
		ev = append(ev,
			AnswerEvidence{Label: fmt.Sprintf("unload_%s", ls.slot), Value: fmt.Sprintf("#%d", unloadAction.ID)},
			AnswerEvidence{Label: fmt.Sprintf("load_%s", ls.slot), Value: fmt.Sprintf("#%d", loadAction.ID)},
		)
	}

	if firstActionID == nil {
		return gapAnswer("restart_llama.cpp", "couldn't create the scheduler-mediated restart proposals")
	}

	text := fmt.Sprintf("I've drafted scheduler-mediated restart proposals for llama.cpp — approve each below and I'll run them (checked just now):\n%s", b.String())
	return FastAnswer{Text: text, Evidence: ev, ActionID: firstActionID, DigDeeper: true}.finalize(), true
}

func (s *Smith) answerRunCheckUp(ctx context.Context) (FastAnswer, bool) {
	findings, err := s.RunChecks(ctx, ScopeQuick, nil, SweepManual)
	if err != nil {
		return gapAnswer("run_check_up", "couldn't run the sweep ("+err.Error()+")")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Ran a quick check-up — %d finding(s) (checked just now):", len(findings))
	ev := []AnswerEvidence{{Label: "count", Value: fmt.Sprintf("%d", len(findings))}}
	for _, f := range findings {
		fmt.Fprintf(&b, "\n- [%s] %s: %s", f.Severity, f.CheckID, f.Summary)
		ev = append(ev, AnswerEvidence{Label: f.CheckID, Value: string(f.Severity) + ": " + f.Summary})
	}
	return FastAnswer{Text: b.String(), Evidence: ev, DigDeeper: true}.finalize(), true
}

// ── helpers ───────────────────────────────────────────────────────────────

func isSlotEntity(entity string) bool {
	switch entity {
	case "a1", "a2", "a3", "a4":
		return true
	}
	return false
}

func entityLabel(entity string) string {
	switch entity {
	case "comfyui":
		return "ComfyUI"
	case "compressor":
		return "compressor"
	case "a0":
		return "a0"
	case "forge":
		return "forge"
	case "embedding":
		return "the embedding service"
	case "stt":
		return "speech-to-text"
	case "tts":
		return "text-to-speech"
	case "aligner":
		return "the aligner"
	case "brain":
		return "smith's brain"
	case "gpu":
		return "the GPU"
	}
	return entity
}

func healthAdjective(entity string, f Finding) string {
	switch entity {
	case "comfyui":
		if f.Severity == SeverityOK {
			return "up"
		}
		return "unreachable"
	case "compressor":
		if f.Severity == SeverityOK {
			return "healthy"
		}
		return "unhealthy"
	}
	if f.Severity == SeverityOK {
		return "healthy"
	}
	return "not healthy"
}

// roundDuration formats a duration as a human "2d 3h" / "4h 15m" / "12m" string.
func roundDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

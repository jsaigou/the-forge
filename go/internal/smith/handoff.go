// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jsaigou/the-forge/internal/store"
)

// Handoff state machine values (docs/v5-smith.md §4.5).
const (
	HandoffNotRequired   = "not_required"
	HandoffRequired      = "required"
	HandoffRunbookIssued = "runbook_issued"
	HandoffAcknowledged  = "acknowledged"
	// HandoffRemoteSwapped is P3's remote-probe branch: smith.model has been
	// swapped to a healthy candidate Offering's wire_model (audited,
	// hand-reversible). An alternative to the runbook path off the same
	// "required" state, not a state that follows runbook_issued — remote and
	// runbook are two different resolutions to the same decision, not two
	// steps of one flow.
	HandoffRemoteSwapped = "remote_swapped"
)

// Handoff is a self-evicting action's brain-relocation state (docs/v5-smith.md
// §4.5) — the signature P2/P3 flow: smith's own reasoning brain may live in a
// GPU slot like any other model, so a load_config/unload_slot proposal can
// evict smith itself. Candidates is probed at creation time (probeHandoffCandidates,
// via newRequiredHandoff) — real health, not a placeholder; Runbook is []
// until the "runbook" resolution renders it.
type Handoff struct {
	State          string        `json:"state"`
	Reason         string        `json:"reason"`
	BrainSlot      string        `json:"brain_slot"`
	BrainModel     string        `json:"brain_model"`
	Candidates     []Candidate   `json:"candidates"`
	Runbook        []RunbookStep `json:"runbook"`
	IssuedAt       *int64        `json:"issued_at"`
	AcknowledgedBy *string       `json:"acknowledged_by"`
	AcknowledgedAt *int64        `json:"acknowledged_at"`
}

// RunbookStep is one copyable instruction in a rendered handoff or
// informational runbook. Why/VerifyCommand are P7 additions (docs/v5-smith.md
// §4.6): Why is a one-line rationale for the step (why it's needed, not
// what it does — Title/Command already say what); VerifyCommand is the
// literal command to run when Verify's expected outcome is itself checkable
// by running something (Verify stays the prose description of what success
// looks like either way). Both are additive JSON — a stored `detail` blob
// from before this field existed simply decodes them as "", no migration,
// no backfill needed.
type RunbookStep struct {
	Title         string `json:"title"`
	Command       string `json:"command"`
	Verify        string `json:"verify"`
	Why           string `json:"why,omitempty"`
	VerifyCommand string `json:"verify_command,omitempty"`
}

// Candidate is a remote brain-swap candidate (P3 seam, shape-frozen now so
// the contract is visible with zero probing code — always [] in P2).
type Candidate struct {
	OfferingID int64  `json:"offering_id"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Healthy    bool   `json:"healthy"`
}

// HandoffRequiredError is returned by ApproveAction when a self_evicting
// action's handoff isn't acknowledged yet. Track B's httpapi layer turns
// this into a 409 handoff_required.
type HandoffRequiredError struct {
	ActionID int64
	Handoff  Handoff
}

func (e *HandoffRequiredError) Error() string {
	return fmt.Sprintf("smith: action %d requires handoff resolution before approval (state=%q)",
		e.ActionID, e.Handoff.State)
}

// stampSelfEviction runs at proposal-creation time for load_config/
// unload_slot drafts (docs/v5-smith.md §4.5):
//
//  1. smith's brain isn't a local slot right now (remote, deterministic-only,
//     or unresolvable) → nothing to evict, not self_evicting.
//  2. The draft targets smith's own slot directly (detail.slot == brain
//     slot) → self_evicting, no Placer needed.
//  3. load_config only: FitPlan(mode) says the placement or its eviction
//     list would land on/evict the brain's slot → self_evicting. The whole
//     plan is stored into detail.fit_plan either way (useful context for the
//     card even when not self-evicting).
//  4. Placer nil, or FitPlan errors, on an *implicit* (no explicit slot)
//     load_config → we cannot conclude "safe": stamp detail.fit_plan.unknown,
//     force Risk to "high", and treat it as self_evicting. "We couldn't
//     measure" is never "it's fine" for this specific guardrail.
func (s *Smith) stampSelfEviction(ctx context.Context, d *ActionDraft) (bool, *Handoff, error) {
	br := s.Brain(ctx)
	if br.Resolution != BrainLocalSlot {
		return false, nil, nil
	}

	var payload struct {
		Slot string `json:"slot"`
		Mode string `json:"mode"`
	}
	if len(d.Detail) > 0 {
		if err := json.Unmarshal(d.Detail, &payload); err != nil {
			return false, nil, fmt.Errorf("smith: parse action detail: %w", err)
		}
	}

	if payload.Slot != "" && payload.Slot == br.Slot {
		return true, s.newRequiredHandoff(ctx, br, fmt.Sprintf(
			"this action directly targets slot %s, where smith's brain (%s) is loaded", br.Slot, br.Model)), nil
	}

	if d.Kind != KindLoadConfig {
		return false, nil, nil
	}

	if s.d.Placer == nil {
		return s.stampUnknownFitPlan(ctx, d, br, "no placement engine wired")
	}
	plan, err := s.d.Placer.FitPlan(payload.Mode)
	if err != nil {
		return s.stampUnknownFitPlan(ctx, d, br, "FitPlan failed: "+err.Error())
	}
	if err := mergeDetailField(d, "fit_plan", plan); err != nil {
		return false, nil, err
	}

	selfEvicting := plan.Slot == br.Slot || slices.Contains(plan.Evict, br.Slot)
	if !selfEvicting {
		return false, nil, nil
	}
	return true, s.newRequiredHandoff(ctx, br, fmt.Sprintf(
		"loading %s would place on or evict slot %s, where smith's brain (%s) is loaded",
		payload.Mode, br.Slot, br.Model)), nil
}

// stampUnknownFitPlan is stampSelfEviction's conservative-degrade path (case
// 4 above): an unmeasurable eviction plan is treated as self-evicting, never
// as safe.
func (s *Smith) stampUnknownFitPlan(ctx context.Context, d *ActionDraft, br BrainResolution, reason string) (bool, *Handoff, error) {
	if err := mergeDetailField(d, "fit_plan", map[string]any{"unknown": true, "reason": reason}); err != nil {
		return false, nil, err
	}
	d.Risk = RiskHigh
	h := s.newRequiredHandoff(ctx, br, fmt.Sprintf(
		"cannot compute the eviction plan; smith's brain is on slot %s and may be evicted (%s)", br.Slot, reason))
	return true, h, nil
}

// newRequiredHandoff builds a fresh "required" handoff for a self-evicting
// draft, probing remote candidates immediately (§4.5's "probe handoff
// candidates" step runs at detection time, not lazily on operator request —
// so the "required" state already carries real Candidates/Healthy data for
// the FE to render alongside the runbook option).
func (s *Smith) newRequiredHandoff(ctx context.Context, br BrainResolution, reason string) *Handoff {
	return &Handoff{
		State:      HandoffRequired,
		Reason:     reason,
		BrainSlot:  br.Slot,
		BrainModel: br.Model,
		Candidates: s.probeHandoffCandidates(ctx, br.Model),
		Runbook:    []RunbookStep{},
	}
}

// probeHandoffCandidates builds the candidate set (docs/v5-smith.md §4.5):
// settings["smith.handoff_offerings"] (ordered Offering IDs, most-preferred
// first) ∪ any enabled Offering of the same Model as brainModel, each probed
// for provider health via Deps.ProviderHealth. Returns [] (never nil) when
// Catalog is unwired or brainModel doesn't resolve to a local Config — the
// candidate LIST always exists on the wire, even empty.
func (s *Smith) probeHandoffCandidates(ctx context.Context, brainModel string) []Candidate {
	if s.d.Catalog == nil {
		return []Candidate{}
	}
	offerings, err := s.d.Catalog.ListOfferings(ctx)
	if err != nil {
		return []Candidate{}
	}
	byID := make(map[int64]store.Offering, len(offerings))
	for _, o := range offerings {
		byID[o.ID] = o
	}

	var brainModelID int64
	if cfg, err := s.d.Catalog.ConfigByName(ctx, brainModel); err == nil && cfg.ID != 0 {
		if v, err := s.d.Catalog.GetVariant(ctx, cfg.VariantID); err == nil {
			brainModelID = v.ModelID
		}
	}

	seen := map[int64]bool{}
	var ids []int64
	for _, id := range s.HandoffOfferings(ctx) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if brainModelID != 0 {
		for _, o := range offerings {
			if o.ModelID == brainModelID && o.Enabled && !seen[o.ID] {
				seen[o.ID] = true
				ids = append(ids, o.ID)
			}
		}
	}

	candidates := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		o, ok := byID[id]
		if !ok || !o.Enabled {
			continue
		}
		healthy := false
		if s.d.ProviderHealth != nil {
			state, err := s.d.ProviderHealth(ctx, o.ProviderName)
			healthy = err == nil && state == "reachable"
		}
		candidates = append(candidates, Candidate{
			OfferingID: o.ID, Model: o.WireModel, Provider: o.ProviderName, Healthy: healthy,
		})
	}
	return candidates
}

// mergeDetailField merges {key: value} into d.Detail's JSON object
// (creating one if Detail is empty/absent), re-marshaling the result back
// into d.Detail.
func mergeDetailField(d *ActionDraft, key string, value any) error {
	m := map[string]any{}
	if len(d.Detail) > 0 {
		if err := json.Unmarshal(d.Detail, &m); err != nil {
			return fmt.Errorf("smith: merge detail.%s: %w", key, err)
		}
	}
	m[key] = value
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("smith: marshal detail.%s: %w", key, err)
	}
	d.Detail = b
	return nil
}

// ResolveHandoff advances a self-evicting action's handoff state
// (docs/v5-smith.md §4.5): "runbook" renders the deterministic instructions
// and moves required→runbook_issued; "acknowledge" moves
// runbook_issued→acknowledged (the only other state, besides
// remote_swapped, that ApproveAction accepts — issuing the runbook alone
// does NOT unblock approval); "remote" swaps smith.model to a healthy
// probed candidate and moves required→remote_swapped (an alternative
// branch off "required", not a follow-on to the runbook path); "cancel"
// rejects the action outright (the same pending→rejected transition
// RejectAction uses).
func (s *Smith) ResolveHandoff(ctx context.Context, id int64, resolution, actor string) (*Action, error) {
	if s.d.Store == nil {
		return nil, ErrStoreUnwired
	}
	switch resolution {
	case "runbook":
		return s.resolveHandoffRunbook(ctx, id, actor)
	case "acknowledge":
		return s.resolveHandoffAcknowledge(ctx, id, actor)
	case "remote":
		return s.resolveHandoffRemote(ctx, id, actor)
	case "cancel":
		return s.resolveHandoffCancel(ctx, id, actor)
	default:
		return nil, fmt.Errorf("smith: unknown handoff resolution %q (want runbook|acknowledge|remote|cancel)", resolution)
	}
}

// resolveHandoffRemote implements the "remote" resolution
// (docs/v5-smith.md §4.5): picks the first healthy probed candidate and
// swaps smith.model to its wire_model via the existing settings_change
// audit shape ({key,new,previous}) — that audit row IS the swap-back
// record, mirroring dispatchSettingsChange (execute.go) but invoked
// directly here (not through the action-proposal pipeline) since the swap
// itself is smith's own handoff bookkeeping, not a separately-approved
// mutation — approval already happened when this action's proposal was
// approved... except it hasn't yet: resolving the handoff is a
// PRECONDITION for approval, not a consequence of it, so this write is
// gated only by the same operator-role + step-up the handoff endpoint
// itself requires (httpapi's ResourceActionSmithExecute), matching how
// resolveHandoffRunbook/Acknowledge already mutate state pre-approval.
func (s *Smith) resolveHandoffRemote(ctx context.Context, id int64, actor string) (*Action, error) {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.SelfEvicting || a.Handoff == nil || a.Handoff.State != HandoffRequired {
		return nil, fmt.Errorf("smith: action %d is not awaiting a handoff decision (self_evicting=%v, handoff=%+v)",
			id, a.SelfEvicting, a.Handoff)
	}
	var chosen *Candidate
	for i := range a.Handoff.Candidates {
		if a.Handoff.Candidates[i].Healthy {
			chosen = &a.Handoff.Candidates[i]
			break
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("smith: no healthy remote candidate available for handoff — use the runbook path instead")
	}
	if _, err := s.swapBrainToRemote(ctx, chosen, actor, fmt.Sprintf("%d:%s", a.ID, a.Kind)); err != nil {
		return nil, err
	}

	h := *a.Handoff
	h.State = HandoffRemoteSwapped
	now := s.d.Now().Unix()
	h.AcknowledgedBy = &actor // reused: "who resolved this handoff", not literally "acknowledged the runbook"
	h.AcknowledgedAt = &now
	return s.saveHandoff(ctx, a, h, actor, "remote")
}

// swapBrainToRemote writes smith.model = chosen.Model and audits the swap
// (the audit row IS the swap-back record). Returns the previous smith.model
// value + any read error, so callers can attach it to their own audit
// entry. target is the audit Target string (an action id, or a slot for the
// auto-recovery path).
func (s *Smith) swapBrainToRemote(ctx context.Context, chosen *Candidate, actor, target string) (previous []byte, prevErr error) {
	if chosen == nil || chosen.Model == "" {
		return nil, errors.New("smith: no candidate model to swap to")
	}
	if s.d.Settings == nil {
		return nil, errors.New("smith: settings not wired")
	}
	previous, prevErr = s.d.Settings.Get(ctx, SettingModel)
	newVal, err := json.Marshal(chosen.Model)
	if err != nil {
		return previous, fmt.Errorf("smith: marshal candidate model: %w", err)
	}
	if err := s.d.Settings.Set(ctx, SettingModel, newVal); err != nil {
		return previous, fmt.Errorf("smith: swap smith.model: %w", err)
	}
	if s.d.Audit != nil {
		detail := map[string]any{"key": SettingModel, "new": chosen.Model, "offering_id": chosen.OfferingID}
		if prevErr == nil {
			detail["previous"] = json.RawMessage(previous)
		}
		b, _ := json.Marshal(detail)
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_handoff_remote_swap",
			Target: target, Detail: string(b),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	return previous, prevErr
}

// resolveHandoffRunbook implements the "runbook" resolution: required→
// runbook_issued.
func (s *Smith) resolveHandoffRunbook(ctx context.Context, id int64, actor string) (*Action, error) {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.SelfEvicting || a.Handoff == nil || a.Handoff.State != HandoffRequired {
		return nil, fmt.Errorf("smith: action %d is not awaiting a runbook (self_evicting=%v, handoff=%+v)",
			id, a.SelfEvicting, a.Handoff)
	}
	br := s.Brain(ctx)
	h := *a.Handoff
	h.Runbook = s.renderHandoffRunbook(br, a)
	h.State = HandoffRunbookIssued
	now := s.d.Now().Unix()
	h.IssuedAt = &now
	return s.saveHandoff(ctx, a, h, actor, "runbook")
}

// resolveHandoffAcknowledge implements the "acknowledge" resolution:
// runbook_issued→acknowledged — the only state ApproveAction accepts.
func (s *Smith) resolveHandoffAcknowledge(ctx context.Context, id int64, actor string) (*Action, error) {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Handoff == nil || a.Handoff.State != HandoffRunbookIssued {
		return nil, fmt.Errorf("smith: action %d handoff is not awaiting acknowledgment (handoff=%+v)", id, a.Handoff)
	}
	h := *a.Handoff
	h.State = HandoffAcknowledged
	now := s.d.Now().Unix()
	h.AcknowledgedBy = &actor
	h.AcknowledgedAt = &now
	return s.saveHandoff(ctx, a, h, actor, "acknowledge")
}

// resolveHandoffCancel implements the "cancel" resolution: the same
// pending→rejected CAS RejectAction uses, just audited under a
// smith_handoff_cancel action name instead of smith_action_reject.
func (s *Smith) resolveHandoffCancel(ctx context.Context, id int64, actor string) (*Action, error) {
	now := s.d.Now().Unix()
	res, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		StatusRejected, now, id, StatusPending)
	if err != nil {
		return nil, fmt.Errorf("smith: cancel handoff (reject action %d): %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInvalidTransition
	}
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_handoff_cancel", Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	s.publishActionUpdate(ctx, a, StatusRejected)
	return a, nil
}

// saveHandoff persists h onto action a.ID's handoff column, audits, and
// publishes smith:handoff_update.
func (s *Smith) saveHandoff(ctx context.Context, a *Action, h Handoff, actor, resolution string) (*Action, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("smith: marshal handoff: %w", err)
	}
	if _, err := s.d.Store.SQL().ExecContext(ctx,
		`UPDATE smith_actions SET handoff = ? WHERE id = ?`, string(b), a.ID); err != nil {
		return nil, fmt.Errorf("smith: save handoff for action %d: %w", a.ID, err)
	}
	updated, err := s.GetAction(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if s.d.Audit != nil {
		if err := s.d.Audit.Write(ctx, store.AuditEntry{
			Actor: actor, Action: "smith_handoff_" + resolution, Target: fmt.Sprintf("%d:%s", a.ID, a.Kind),
		}); err != nil {
			s.logf("audit write failed: %v", err)
		}
	}
	if s.d.Publisher != nil {
		s.d.Publisher.Publish(EventHandoffUpdate, map[string]any{"action_id": a.ID, "state": h.State})
	}
	return updated, nil
}

// renderHandoffRunbook builds the deterministic 4-step handoff runbook
// (docs/v5-smith.md §4.5): where the brain is now, expect a deterministic-
// tier degrade, the real command for this action, and how to restore the
// brain afterward. Pure given br/a and (for step 4's free-slot guess) the
// Sched/Placer already wired onto s — no LLM involved.
func (s *Smith) renderHandoffRunbook(br BrainResolution, a *Action) []RunbookStep {
	return []RunbookStep{
		{
			Title:         "Note where smith's brain is",
			Command:       "curl -s localhost:5000/api/v1/smith/status | jq .brain",
			Verify:        fmt.Sprintf(`resolution == "local_slot", slot == %q`, br.Slot),
			Why:           "so you know what to restore in the last step",
			VerifyCommand: "curl -s localhost:5000/api/v1/smith/status | jq .brain",
		},
		{
			Title:   "Expect smith to drop to the deterministic tier during this operation",
			Command: "",
			Verify:  `GET /api/v1/smith/status reports tier: "deterministic" throughout (this runbook path keeps the brain local rather than swapping to a remote candidate — see the "remote" handoff resolution if one is healthy)`,
			Why:     "this operation is about to evict smith's own brain slot — no remote candidate was healthy, so chat degrades rather than swapping",
		},
		{
			Title:         "Run the operation",
			Command:       operationCommand(a),
			Verify:        operationVerify(a),
			VerifyCommand: operationVerifyCommand(a),
		},
		{
			Title: "Restore smith's brain when done",
			Command: fmt.Sprintf(`curl -X POST localhost:5000/api/v1/load -d '{"mode":%q,"slot":%q}'`,
				br.Model, s.freeSlotGuess(br.Slot)),
			Verify:        `GET /api/v1/smith/status reports .brain.resolution == "local_slot"`,
			Why:           "the operation above doesn't reload smith's own brain automatically — this is a manual step by design (docs §4.5 guardrail 2: smith never unloads/loads its own slot as a side effect)",
			VerifyCommand: "curl -s localhost:5000/api/v1/smith/status | jq .brain",
		},
	}
}

// operationCommand renders a's real equivalent curl/systemctl command for
// step 3 of the handoff runbook.
func operationCommand(a *Action) string {
	switch a.Kind {
	case KindLoadConfig:
		d, _ := parseDetail[loadConfigDetail](a.Detail)
		return fmt.Sprintf(`curl -X POST localhost:5000/api/v1/load -d '{"mode":%q,"slot":%q}'`, d.Mode, d.Slot)
	case KindUnloadSlot:
		d, _ := parseDetail[unloadSlotDetail](a.Detail)
		return fmt.Sprintf(`curl -X POST localhost:5000/api/v1/unload -d '{"slot":%q}'`, d.Slot)
	case KindRestartForgeUnit:
		d, _ := parseDetail[restartUnitDetail](a.Detail)
		return "sudo systemctl restart " + d.Unit + ".service"
	default:
		return "# no direct command equivalent for action kind " + a.Kind
	}
}

// operationVerify renders the plain-language check for step 3.
func operationVerify(a *Action) string {
	switch a.Kind {
	case KindLoadConfig:
		return "GET /api/v1/status shows the target slot occupied by the intended mode"
	case KindUnloadSlot:
		return "GET /api/v1/status shows the target slot empty"
	case KindRestartForgeUnit:
		return "systemctl is-active reports active for the restarted unit"
	default:
		return "manually confirm the operation's effect"
	}
}

// operationVerifyCommand renders the literal command behind
// operationVerify's prose, when one exists — "" for a kind with no direct
// equivalent (mirrors operationCommand's default case).
func operationVerifyCommand(a *Action) string {
	switch a.Kind {
	case KindLoadConfig, KindUnloadSlot:
		return "curl -s localhost:5000/api/v1/status | jq .slots"
	case KindRestartForgeUnit:
		d, _ := parseDetail[restartUnitDetail](a.Detail)
		return "systemctl is-active " + d.Unit + ".service"
	default:
		return ""
	}
}

// freeSlotGuess picks a slot for step 4's brain-restore command: any
// Placer-known slot other than brainSlot that Sched reports empty. Falls
// back to a placeholder (never a guess that could be wrong) when either
// dependency is unavailable.
func (s *Smith) freeSlotGuess(brainSlot string) string {
	const placeholder = "<free slot — check /api/v1/status>"
	if s.d.Placer == nil || s.d.Sched == nil {
		return placeholder
	}
	occupied := map[string]bool{}
	for slot, mode := range s.d.Sched.Status().Slots {
		if mode != "" {
			occupied[slot] = true
		}
	}
	for _, slot := range s.d.Placer.Slots() {
		if slot == brainSlot || occupied[slot] {
			continue
		}
		return slot
	}
	return placeholder
}

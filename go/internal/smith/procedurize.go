// SPDX-License-Identifier: Apache-2.0

package smith

// procedurize.go — the "let smith fix it" button's backend (autonomous-
// remediation Sprint 3, docs/v5-smith.md §13). Turns a pending atomic
// action (restart_forge_unit / unload_slot / delete_files / the
// binary-upstream build-refresh runbook, Sprint 6) into its equivalent
// registered procedure, so approving it goes through the procedure
// runner's checkpoint/rollback/maintenance machinery instead of a bare
// one-shot dispatch (or, for a runbook, no execution at all). Every param
// fed to the new procedure is reshaped from the SOURCE action's own Detail
// — never operator free text, never LLM output.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/smith/procedures"
)

// ErrNoProcedureForKind is returned by ProcedurePreview/Procedurize when the
// source action's kind has no procedure mapped in procedureForActionKind.
var ErrNoProcedureForKind = errors.New("smith: this action kind has no equivalent procedure")

// procedureForActionKind maps an atomic action kind to the registered
// procedure that performs the same real operation through the procedure
// runner instead of a one-shot dispatch. Fixed coded data, mirrored on the
// frontend as a small const (Sprint 6 replaced that mirror with a
// server-computed Action.Procedurizable field instead — see
// actions.go's MarshalJSON — since a hand-duplicated set with no test
// cross-checking it against this map is exactly how a newly-mapped kind
// silently loses its button).
var procedureForActionKind = map[string]string{
	KindRestartForgeUnit: "restart_down_unit",
	KindUnloadSlot:       "reconcile_orphaned_slot",
	KindDeleteFiles:      "comfyui_prune",
	KindInstallLauncher:  "restore_unit_launcher",
}

// procedureForAction resolves a's mapped procedure ID. Most kinds are
// mapped 1:1 via procedureForActionKind, but KindRunbook is shared by
// several unrelated proposal shapes (self-review closures, kernel_params,
// gtt_ceiling, binary_stale, binary_upstream) that all carry the same
// Kind — so a runbook needs a second discriminator, its own DedupeKey
// prefix (Sprint 6, autonomous-remediation build-refresh capstone), rather
// than the kind alone.
func procedureForAction(a *Action) (string, bool) {
	if id, ok := procedureForActionKind[a.Kind]; ok {
		return id, true
	}
	if a.Kind == KindRunbook && strings.HasPrefix(a.DedupeKey, dedupeKeyBinaryUpstreamPrefix) {
		return "build_refresh", true
	}
	return "", false
}

// binaryUpstreamRunbookDetail is the subset of proposeRebuildRunbook's
// (propose.go) flat Detail shape procedureParamsForAction needs — just the
// tracked binary's own Name, which build_refresh's one declared Param
// ("binary") uses to resolve everything else (path, source tree, upstream
// ref) fresh from the live smith.binaries.tracked setting at dispatch time,
// never from this snapshot (the finding's evidence can go stale between
// proposal and approval; the live setting can't).
type binaryUpstreamRunbookDetail struct {
	Name string `json:"name"`
}

// procedureParamsForAction reshapes a's own Detail (its kind's normal
// dispatch shape, execute.go, or — for the binary-upstream runbook shape —
// propose.go's proposeRebuildRunbook) into the mapped procedure's declared
// Params.
func procedureParamsForAction(a *Action) (map[string]string, error) {
	switch {
	case a.Kind == KindRestartForgeUnit:
		d, err := parseDetail[restartUnitDetail](a.Detail)
		if err != nil {
			return nil, err
		}
		return map[string]string{"unit": d.Unit}, nil
	case a.Kind == KindInstallLauncher:
		d, err := parseDetail[installLauncherDetail](a.Detail)
		if err != nil {
			return nil, err
		}
		return map[string]string{"unit": d.Unit}, nil
	case a.Kind == KindUnloadSlot:
		d, err := parseDetail[unloadSlotDetail](a.Detail)
		if err != nil {
			return nil, err
		}
		return map[string]string{"slot": d.Slot}, nil
	case a.Kind == KindDeleteFiles:
		d, err := parseDetail[deleteFilesDetail](a.Detail)
		if err != nil {
			return nil, err
		}
		filesJSON, err := json.Marshal(d.Files)
		if err != nil {
			return nil, fmt.Errorf("smith: marshal files for procedurize: %w", err)
		}
		return map[string]string{"files_json": string(filesJSON)}, nil
	case a.Kind == KindRunbook && strings.HasPrefix(a.DedupeKey, dedupeKeyBinaryUpstreamPrefix):
		d, err := parseDetail[binaryUpstreamRunbookDetail](a.Detail)
		if err != nil {
			return nil, err
		}
		if d.Name == "" {
			return nil, fmt.Errorf("smith: procedurize %d: binary-upstream runbook detail has no name", a.ID)
		}
		return map[string]string{"binary": d.Name}, nil
	default:
		return nil, fmt.Errorf("smith: %w: %q", ErrNoProcedureForKind, a.Kind)
	}
}

// ProcedurePreview is the downtime-disclosure modal's read-only data,
// projected from the mapped procedure's own registered Impact so the
// frontend never hardcodes a stale copy of it.
type ProcedurePreview struct {
	ProcedureID      string   `json:"procedure_id"`
	Title            string   `json:"title"`
	NeedsMaintenance bool     `json:"needs_maintenance"`
	EstDurationSec   int64    `json:"est_duration_sec"`
	AffectedSlots    []string `json:"affected_slots,omitempty"`
	AffectedServices []string `json:"affected_services,omitempty"`
	DaemonRestart    bool     `json:"daemon_restart"`
}

// ProcedurePreview resolves sourceActionID's mapped procedure and projects
// its Impact for the downtime-disclosure modal. Read-only — never mutates
// the source action.
func (s *Smith) ProcedurePreview(ctx context.Context, sourceActionID int64) (*ProcedurePreview, error) {
	a, err := s.GetAction(ctx, sourceActionID)
	if err != nil {
		return nil, err
	}
	procID, ok := procedureForAction(a)
	if !ok {
		return nil, fmt.Errorf("smith: %w: %q", ErrNoProcedureForKind, a.Kind)
	}
	proc, ok := procedures.Get(procID)
	if !ok {
		return nil, fmt.Errorf("smith: %w: %q", ErrProcedureNotFound, procID)
	}
	return &ProcedurePreview{
		ProcedureID:      proc.ID,
		Title:            proc.Title,
		NeedsMaintenance: proc.Impact.NeedsMaintenance,
		EstDurationSec:   int64(proc.Impact.EstDuration.Seconds()),
		AffectedSlots:    proc.Impact.AffectedSlots,
		AffectedServices: proc.Impact.AffectedServices,
		DaemonRestart:    proc.Impact.DaemonRestart,
	}, nil
}

// Procedurize converts a pending atomic action into its mapped procedure
// action: creates a new KindProcedure action with Params derived from the
// source action's own Detail, approves it (kicking off execution exactly
// like a normal approve — including the create+approve step-up gate at the
// httpapi layer), and finally supersedes the source action so it stops
// appearing as a separate pending item. The source is only superseded once
// the replacement genuinely exists and is approved — an error partway
// through (e.g. CreateAction rejects the reshaped params) leaves the source
// action untouched, still pending, rather than orphaned.
func (s *Smith) Procedurize(ctx context.Context, sourceActionID int64, actor string) (*Action, error) {
	src, err := s.GetAction(ctx, sourceActionID)
	if err != nil {
		return nil, err
	}
	if src.Status != StatusPending {
		return nil, ErrInvalidTransition
	}
	if src.SelfEvicting {
		// CreateAction's self-eviction stamping (stampSelfEviction) only
		// runs for KindLoadConfig/KindUnloadSlot drafts — the new
		// KindProcedure action created below would silently skip it, so
		// ApproveAction's handoff-required gate would never fire for an
		// unload that evicts smith's own brain. Fails closed rather than
		// building a second handoff pass-through path for this edge case:
		// approve the source action directly instead.
		return nil, fmt.Errorf("smith: action %d is self-evicting and cannot be procedurized; approve it directly instead", src.ID)
	}
	procID, ok := procedureForAction(src)
	if !ok {
		return nil, fmt.Errorf("smith: %w: %q", ErrNoProcedureForKind, src.Kind)
	}
	proc, ok := procedures.Get(procID)
	if !ok {
		return nil, fmt.Errorf("smith: %w: %q", ErrProcedureNotFound, procID)
	}
	params, err := procedureParamsForAction(src)
	if err != nil {
		return nil, err
	}
	if err := procedures.ValidateParams(proc, params); err != nil {
		return nil, fmt.Errorf("smith: %w", err)
	}
	detail, err := json.Marshal(procedureDetail{ProcedureID: proc.ID, Params: params})
	if err != nil {
		return nil, fmt.Errorf("smith: marshal procedurize detail: %w", err)
	}

	newAction, err := s.CreateAction(ctx, ActionDraft{
		Kind:            KindProcedure,
		Title:           proc.Title,
		Detail:          detail,
		Risk:            src.Risk,
		CreatedBy:       actor,
		InvestigationID: src.InvestigationID,
		ConversationID:  src.ConversationID,
		FindingID:       src.FindingID,
	})
	if err != nil {
		return nil, err
	}
	approved, err := s.ApproveAction(ctx, newAction.ID, actor)
	if err != nil {
		return nil, err
	}
	note := fmt.Sprintf("superseded by procedure action %d (%s)", newAction.ID, proc.ID)
	if !s.supersedeActionWithNote(ctx, sourceActionID, note) {
		s.logf("procedurize: could not supersede source action %d after creating replacement %d", sourceActionID, newAction.ID)
	}
	return approved, nil
}

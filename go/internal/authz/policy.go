// SPDX-License-Identifier: Apache-2.0

package authz

// policy.go — Sprint 0-AUTH §3.4: policy matrix store + evaluation.
//
// The policy matrix maps resource keys to minimum assurance factors. It is
// orthogonal to RBAC role (role = who; assurance = how strongly proven). Both
// must pass. Admin-editable, stored in settings under "auth.policy" as JSON.
//
// Resource keys are per page + sensitive sub-areas (§3.4 locked granularity).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Resource keys (§3.4). Each maps to a minimum factor.
const (
	ResourcePageDashboard          = "page.dashboard"
	ResourcePageConsole            = "page.console"
	ResourcePageScheduling         = "page.scheduling"
	ResourcePageCompressor           = "page.compression"
	ResourcePageSettings           = "page.settings"
	ResourceAreaSettingsSecurity   = "area.settings.security"
	ResourceAreaSettingsProviderK  = "area.settings.provider_keys"
	ResourceActionModelLoadUnload  = "action.model.load_unload"
	ResourceActionCompressorTeardown = "action.compressor.teardown"
	ResourceActionReservationWrite = "action.reservation.write"
	ResourceActionModelProfile     = "action.model.profile"

	// Sprint 12 (was H): the Settings "Danger Zone" — boot-critical infra
	// (listen addresses, paths, ports, tailscale hostname) and the daemon
	// restart action it can require. Deliberately its own resource, not
	// folded into area.settings.security or page.settings: those gate
	// "settings in general" and "security config specifically," neither of
	// which implies "I trust this session to change what the daemon binds
	// to or to restart the process."
	ResourceAreaSettingsSystem  = "area.settings.system"
	ResourceActionSystemRestart = "action.system.restart"

	// ResourceActionSmithExecute gates the smith action model's mutating
	// routes (P2, docs/v5-smith.md §4.6): POST .../actions (a low-assurance
	// session could otherwise plant a payload for a distracted admin to
	// approve later — a confused-deputy path even though creation alone
	// doesn't execute anything) and POST .../actions/{id}/approve (the
	// mutation that actually triggers execution). reject and handoff
	// resolution are deliberately NOT gated by this key: reject is always
	// the safe direction, and resolving a handoff carries no privilege
	// beyond what create already required to exist.
	ResourceActionSmithExecute = "action.smith.execute"

	// ResourcePageSmith gates the Help page's "Ask smith" chat + Diagnostics
	// sub-tabs (P3, docs/v5-smith.md §7). Defaults alongside the other
	// operational pages (console/dashboard/scheduling/compressor) at network
	// assurance — chat itself never mutates anything privileged; the
	// mutations it can propose still go through ResourceActionSmithExecute.
	ResourcePageSmith = "page.smith"

	// ResourceActionSmithAutonomy gates the standing autonomy policy
	// (autonomous-remediation Sprint 5, docs/v5-smith.md §13.3 —
	// go/internal/smith/autonomy.go): PUT /api/v1/smith/autonomy, but only
	// when the request would ESCALATE trust (turn the global kill switch
	// on, or opt in a procedure that wasn't already) — lowering/disabling
	// autonomy needs no step-up, same "always the safe direction" posture
	// as reject/checkpoint-abort. Deliberately its own resource, not folded
	// into action.smith.execute: granting smith a STANDING license to skip
	// human approval for future occurrences of a problem is a materially
	// different trust decision than approving one action right now.
	ResourceActionSmithAutonomy = "action.smith.autonomy"

	// ResourceActionModelDownload gates starting/pausing/resuming/
	// cancelling/approving an HF model-acquisition job (go/internal/
	// hfdownload) — a real, tens-of-GB write to disk that ends in new
	// catalog rows. Deliberately network-tier, matching
	// ResourceActionModelLoadUnload rather than the password-tier
	// ResourceActionModelProfile: unlike profiling, a download never
	// evicts a live slot or touches traffic — the operator-facing risk is
	// disk space and catalog clutter, not an outage.
	ResourceActionModelDownload = "action.model.download"
)

// DefaultPolicy is the shipped seed (§3.4). Written to "auth.policy" on first
// boot if the key is absent; admin-overridable thereafter.
var DefaultPolicy = map[string]string{
	ResourcePageDashboard:          string(AssuranceNetwork),
	ResourcePageConsole:            string(AssuranceNetwork),
	ResourcePageScheduling:         string(AssuranceNetwork),
	ResourcePageCompressor:           string(AssuranceNetwork),
	ResourcePageSettings:           string(AssurancePassword),
	ResourceAreaSettingsSecurity:   string(AssurancePassword),
	ResourceAreaSettingsProviderK:  string(AssurancePassword),
	ResourceActionModelLoadUnload:  string(AssuranceNetwork),
	ResourceActionCompressorTeardown: string(AssurancePassword),
	ResourceActionReservationWrite: string(AssuranceNetwork),
	ResourceActionModelProfile:     string(AssurancePassword),
	ResourceAreaSettingsSystem:     string(AssurancePassword),
	ResourceActionSystemRestart:    string(AssurancePassword),
	ResourceActionSmithExecute:     string(AssurancePassword),
	ResourcePageSmith:              string(AssuranceNetwork),
	ResourceActionSmithAutonomy:    string(AssurancePassword),
	ResourceActionModelDownload:    string(AssuranceNetwork),
}

// allResources is the enumerable set of valid resource keys.
var allResources = []string{
	ResourcePageDashboard,
	ResourcePageConsole,
	ResourcePageScheduling,
	ResourcePageCompressor,
	ResourcePageSettings,
	ResourceAreaSettingsSecurity,
	ResourceAreaSettingsProviderK,
	ResourceActionModelLoadUnload,
	ResourceActionCompressorTeardown,
	ResourceActionReservationWrite,
	ResourceActionModelProfile,
	ResourceAreaSettingsSystem,
	ResourceActionSystemRestart,
	ResourceActionSmithExecute,
	ResourcePageSmith,
	ResourceActionSmithAutonomy,
	ResourceActionModelDownload,
}

// ValidResource reports whether key is a known resource.
func ValidResource(key string) bool {
	for _, r := range allResources {
		if r == key {
			return true
		}
	}
	return false
}

// PolicySettings is the JSON KV reader/writer the policy store needs.
// (Mirrors store.Settings but kept as a local seam so the authz package
// doesn't import store — the httpapi layer injects a concrete adapter.)
type PolicySettings interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte) error
}

// PolicyStore loads and saves the policy matrix from the settings KV.
type PolicyStore struct {
	settings PolicySettings
}

// NewPolicyStore returns a PolicyStore backed by the given settings KV.
func NewPolicyStore(settings PolicySettings) *PolicyStore {
	return &PolicyStore{settings: settings}
}

// Load returns the current policy, seeding DefaultPolicy on first access.
func (p *PolicyStore) Load(ctx context.Context) (map[string]string, error) {
	raw, err := p.settings.Get(ctx, "auth.policy")
	if err != nil {
		// ErrNotFound equivalent → seed the default.
		if isNotFoundErr(err) {
			if seedErr := p.Save(ctx, DefaultPolicy); seedErr != nil {
				return nil, fmt.Errorf("authz: policy seed: %w", seedErr)
			}
			return copyPolicy(DefaultPolicy), nil
		}
		return nil, fmt.Errorf("authz: policy load: %w", err)
	}
	var policy map[string]string
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("authz: policy parse: %w", err)
	}
	// Merge with defaults: any missing key falls back to the default.
	for k, v := range DefaultPolicy {
		if _, ok := policy[k]; !ok {
			policy[k] = v
		}
	}
	return policy, nil
}

// Save persists the policy matrix to the settings KV.
func (p *PolicyStore) Save(ctx context.Context, policy map[string]string) error {
	raw, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("authz: policy marshal: %w", err)
	}
	return p.settings.Set(ctx, "auth.policy", raw)
}

// ValidatePolicy reports whether a policy map has valid keys + factors.
// Returns the set of invalid entries (empty = valid).
func ValidatePolicy(policy map[string]string) map[string]string {
	invalid := map[string]string{}
	for k, v := range policy {
		if !ValidResource(k) {
			invalid[k] = "unknown resource key"
			continue
		}
		if !ValidAssurance(v) {
			invalid[k] = "must be one of: network, password, totp, passkey"
		}
	}
	return invalid
}

// ── Evaluation ───────────────────────────────────────────────────────────────

// StepUpTTL is how long an elevation remains valid before a resource above
// `network` requires re-stepping-up (§9 Q5, default 15 min).
const DefaultStepUpTTL = 15 * time.Minute

// PolicyEvaluator checks whether a session's assurance satisfies the policy
// for a given resource, accounting for the step-up TTL.
type PolicyEvaluator struct {
	policy map[string]string
	ttl    time.Duration
	now    func() time.Time
}

// NewPolicyEvaluator returns an evaluator for the given policy + TTL.
func NewPolicyEvaluator(policy map[string]string, ttl time.Duration, now func() time.Time) *PolicyEvaluator {
	if ttl == 0 {
		ttl = DefaultStepUpTTL
	}
	if now == nil {
		now = time.Now
	}
	return &PolicyEvaluator{policy: policy, ttl: ttl, now: now}
}

// Decision is the result of a policy evaluation.
type Decision struct {
	// Allowed is true when the session's assurance satisfies the policy.
	Allowed bool
	// Required is the minimum factor the policy demands (when not allowed).
	Required Assurance
	// Resource is the resource key that was checked.
	Resource string
	// Reason explains the decision (for step_up_required responses).
	Reason string
}

// Evaluate checks whether the session's assurance + assurance_at satisfies
// the policy for the given resource.
//
// Rules (§3.5):
//   - Network-only resources (min=network) have no TTL: an L0 session always
//     passes.
//   - Resources requiring password/totp/passkey: the session's assurance_at
//     must be within the TTL (recently proven). If the session's assurance is
//     high enough but the elevation is stale, it must re-step-up.
//   - A session whose assurance is below the minimum always fails (step-up
//     required).
func (e *PolicyEvaluator) Evaluate(resource string, assurance Assurance, assuranceAt time.Time) Decision {
	min, ok := e.policy[resource]
	if !ok {
		min = string(AssuranceNetwork)
	}
	minAssurance := Assurance(min)

	// Network-only resources: no TTL, L0 always passes.
	if minAssurance == AssuranceNetwork {
		if assurance.AtLeast(AssuranceNetwork) {
			return Decision{Allowed: true, Required: minAssurance, Resource: resource}
		}
		return Decision{Allowed: false, Required: minAssurance, Resource: resource,
			Reason: "network identity required"}
	}

	// Above-network resources: assurance must be high enough AND recently
	// proven (within TTL).
	if !assurance.AtLeast(minAssurance) {
		return Decision{Allowed: false, Required: minAssurance, Resource: resource,
			Reason: "step-up required"}
	}
	// Assurance is high enough — check TTL.
	if e.ttl > 0 && !assuranceAt.IsZero() {
		age := e.now().Sub(assuranceAt)
		if age > e.ttl {
			return Decision{Allowed: false, Required: minAssurance, Resource: resource,
				Reason: "step-up expired, re-authenticate"}
		}
	}
	return Decision{Allowed: true, Required: minAssurance, Resource: resource}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func copyPolicy(p map[string]string) map[string]string {
	out := make(map[string]string, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// isNotFoundErr reports whether err represents a "key not set" condition.
// The store.Settings.Get contract returns store.ErrNotFound for unset keys;
// this check avoids importing store (the seam uses the same sentinel).
func isNotFoundErr(err error) bool {
	return err != nil && err.Error() == "store: not found"
}

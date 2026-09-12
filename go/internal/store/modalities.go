// SPDX-License-Identifier: Apache-2.0
package store

// Modality-gap reasons (Sprint J1 vocabulary, verbatim — these strings are
// wire-facing via registry.ModalityGap.Reason).
const (
	ModalityReasonNoMMProj      = "no mmproj linked"
	ModalityReasonMMProjMissing = "mmproj file missing on disk"
)

// ModalityResolution is the outcome of narrowing one Model's architectural
// modalities to what one specific Config can actually deliver.
type ModalityResolution struct {
	Enabled     []string // always begins with "text"; never nil
	Unavailable []string // model modalities this config can't deliver; nil when none
	Reason      string   // "" iff Unavailable is empty
}

// ResolveModalities is the single source of truth for the Sprint J1
// precedence (moved here from registry.resolveModalities so a0's
// /v1/models and the PWA's cards can never disagree):
//
//  1. "text" is always enabled.
//  2. An explicit c.Modalities override wins verbatim, even an empty one
//     (an operator asserting "text only" despite a capable model/mmproj).
//  3. MMProjArtifactID == 0 → text only; every other model-level modality
//     is unavailable, reason ModalityReasonNoMMProj.
//  4. mmprojMissing → text only, reason ModalityReasonMMProjMissing.
//  5. Otherwise inherit mdl.Modalities.
//
// mmprojMissing means "the linked mmproj artifact row exists AND is
// flagged Missing". An unknown/zero artifact id must be passed as false,
// which preserves the pre-extraction `ok && a.Missing` behavior exactly.
func ResolveModalities(c Config, mdl Model, mmprojMissing bool) ModalityResolution {
	nonText := func(mods []string) []string {
		out := make([]string, 0, len(mods))
		for _, m := range mods {
			if m != "text" {
				out = append(out, m)
			}
		}
		return out
	}

	if c.Modalities != nil {
		return ModalityResolution{Enabled: append([]string{"text"}, nonText(*c.Modalities)...)}
	}

	if c.MMProjArtifactID == 0 {
		return newTextOnlyResolution(nonText(mdl.Modalities), ModalityReasonNoMMProj)
	}

	if mmprojMissing {
		return newTextOnlyResolution(nonText(mdl.Modalities), ModalityReasonMMProjMissing)
	}

	return ModalityResolution{Enabled: append([]string{"text"}, nonText(mdl.Modalities)...)}
}

// newTextOnlyResolution builds the "text only" result for the no-mmproj /
// mmproj-missing branches. Reason is only set when there's an actual gap to
// report — a model with no non-text modalities to begin with has nothing
// unavailable, so Unavailable/Reason both stay zero-valued.
func newTextOnlyResolution(gaps []string, reason string) ModalityResolution {
	if len(gaps) == 0 {
		return ModalityResolution{Enabled: []string{"text"}}
	}
	return ModalityResolution{Enabled: []string{"text"}, Unavailable: gaps, Reason: reason}
}

// SPDX-License-Identifier: Apache-2.0
package store

// Output modality values (ADR-0021). They mirror llama-server's architecture.output_modalities so the two vocabularies
// stay one thing: "audio" means the model speaks, "decisions" means a native decision model served on /v1/systemone.
const (
	OutputText      = "text"
	OutputAudio     = "audio"
	OutputDecisions = "decisions"
)

// normalizeOutputModalities returns xs, or ["text"] when it is empty. A model always produces something, and every
// pre-0098 model produced text, so an empty set on write means "the default", never "produces nothing".
func normalizeOutputModalities(xs []string) []string {
	if len(xs) == 0 {
		return []string{OutputText}
	}
	return xs
}

// OutputModalityResolution is the outcome of narrowing one Model's output modalities to what one Config can deliver.
type OutputModalityResolution struct {
	Enabled     []string // never empty; "text" is NOT forced (a decision model produces no text)
	Unavailable []string // model outputs this config can't deliver; nil when none
	Reason      string   // "" iff Unavailable is empty
}

// ResolveOutputModalities is the output-side counterpart of ResolveModalities:
//
//  1. An explicit c.OutputModalities override wins verbatim (an empty override falls back to ["text"]).
//  2. Otherwise the model's outputs are inherited.
//  3. "audio" output also needs a linked mmproj: the speech codec lives in the projector. Without one (or with the
//     file missing) "audio" is removed from Enabled and reported as unavailable. "text" and "decisions" never depend
//     on the projector.
//  4. If nothing is left, the result is ["text"] so a config is never advertised as producing nothing.
//
// Unlike ResolveModalities there is no always-on "text": [decisions] stays [decisions].
func ResolveOutputModalities(c Config, mdl Model, mmprojMissing bool) OutputModalityResolution {
	if c.OutputModalities != nil {
		return OutputModalityResolution{Enabled: normalizeOutputModalities(*c.OutputModalities)}
	}
	outs := normalizeOutputModalities(mdl.OutputModalities)
	enabled := make([]string, 0, len(outs))
	var gaps []string
	for _, m := range outs {
		if m == OutputAudio && (c.MMProjArtifactID == 0 || mmprojMissing) {
			gaps = append(gaps, m)
			continue
		}
		enabled = append(enabled, m)
	}
	res := OutputModalityResolution{Enabled: enabled}
	if len(gaps) > 0 {
		res.Unavailable = gaps
		res.Reason = ModalityReasonNoMMProj
		if c.MMProjArtifactID != 0 {
			res.Reason = ModalityReasonMMProjMissing
		}
	}
	if len(res.Enabled) == 0 {
		res.Enabled = []string{OutputText}
	}
	return res
}

// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MemoryTuple is everything a tuple-bound memory figure (a curated safe_memory_bytes benchmark for an engine whose footprint is set by its launch
// flags, enginekind.Spec.NeedsCuratedMemory) was measured at, in a form two parties can compare for equality: the build, the model variant and
// weight file, the configured context and the launch flags in canonical form. The flags are carried whole, not as a hand-picked list of names, so
// that --max-num-seqs, the KV dtype, the speculation depth, a card-budget flag or any flag added later are all part of the tuple without this code
// knowing them: a figure measured with one set of flags is never accepted for another.
//
// The benchmark row stores Key()'s JSON in benchmarks.tuple_json; the fit gate builds the tuple of the config being loaded and refuses unless an
// equal one is recorded.
type MemoryTuple struct {
	BuildID          int64    `json:"build_id"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	NCtx             int      `json:"n_ctx"`
	Args             []string `json:"args"`
}

// CanonicalArgs reduces a config's extra_args to an order-independent, comparable form: one entry per flag, "--flag value value", sorted. A token
// that starts with "-" and is not a plain (possibly negative) number begins a new flag; everything after it up to the next flag is its values. Tokens
// before the first flag (which no valid Radiance command line has) become entries of their own so that nothing is dropped.
func CanonicalArgs(args []string) []string {
	var groups []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, a := range args {
		if isFlagToken(a) {
			flush()
		}
		cur = append(cur, a)
	}
	flush()
	sort.Strings(groups)
	return groups
}

// isFlagToken: starts with "-" and is not a number such as -1 or -0.5.
func isFlagToken(a string) bool {
	if len(a) < 2 || a[0] != '-' {
		return false
	}
	rest := a[1:]
	digit := false
	for i, r := range rest {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' && i > 0:
		default:
			return true
		}
	}
	return !digit
}

// NewMemoryTuple builds the tuple of a config from the ids and the launch flags it will run with.
func NewMemoryTuple(buildID, variantID, weightArtifactID int64, nctx int, extraArgs []string) MemoryTuple {
	return MemoryTuple{BuildID: buildID, VariantID: variantID, WeightArtifactID: weightArtifactID, NCtx: nctx, Args: CanonicalArgs(extraArgs)}
}

// Key is the tuple's canonical JSON, the value stored in benchmarks.tuple_json and compared for equality.
func (t MemoryTuple) Key() string {
	if t.Args == nil {
		t.Args = []string{}
	}
	b, err := json.Marshal(t)
	if err != nil { // a struct of ints and strings cannot fail; never return an empty key that would equal another empty key
		panic(fmt.Sprintf("launch: tuple marshal: %v", err))
	}
	return string(b)
}

// ParseMemoryTuple reads a stored tuple_json. Unknown fields are refused (a typo must not silently weaken the comparison) and the flags are
// re-canonicalised, so a hand-written tuple with the flags in another order still compares equal to the same launch.
func ParseMemoryTuple(raw string) (MemoryTuple, error) {
	var t MemoryTuple
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return MemoryTuple{}, fmt.Errorf("tuple is not a JSON object with build_id, variant_id, weight_artifact_id, n_ctx and args: %w", err)
	}
	if dec.More() {
		return MemoryTuple{}, fmt.Errorf("tuple has trailing data after the JSON object")
	}
	if t.BuildID <= 0 || t.VariantID <= 0 || t.WeightArtifactID <= 0 || t.NCtx <= 0 {
		return MemoryTuple{}, fmt.Errorf("tuple needs positive build_id, variant_id, weight_artifact_id and n_ctx")
	}
	// Each entry is already "--flag value"; split on single spaces to recanonicalise.
	var flat []string
	for _, g := range t.Args {
		flat = append(flat, strings.Fields(g)...)
	}
	t.Args = CanonicalArgs(flat)
	return t, nil
}

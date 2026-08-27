// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"regexp"
	"sort"
	"strings"
)

// rank.go — GGUF candidate ranking against a memory budget. Same
// heuristics smith/sourcing.go's Evaluate already applies (modelselection.md's
// documented rules), reimplemented here rather than imported: smith will
// depend on internal/hfdownload (for its download tools), and
// internal/hfdownload depends on this package, so this package cannot
// depend back on smith without a cycle. sourcing.go keeps its own copy —
// it's a separate, narrower read-only research feature (FR4) that isn't
// being touched by the HF model-acquisition track.

// OneTwoXOverhead is modelselection.md's "VRAM needed ≈ GGUF file size ×
// 1.2" rule — KV cache, runtime buffers, and compute workspace at modest
// context.
const OneTwoXOverhead = 1.2

// NearTopWindow is how close (in bytes) to the largest fitting candidate a
// smaller one must be for QualityRank to break the tie instead of raw size
// — modelselection.md's own figure for _L vs _M ("usually only ~0.3 GB
// larger... almost always worth choosing"), rounded up generously.
const NearTopWindow = 1 << 30 // 1 GiB

// QuantCandidate is one GGUF file evaluated against a memory budget.
type QuantCandidate struct {
	Filename           string `json:"filename"`
	SizeBytes          int64  `json:"size_bytes"`
	Quant              string `json:"quant"` // extracted label, "" if unrecognized
	EstimatedVRAMBytes int64  `json:"estimated_vram_bytes"`
	FitsBudget         bool   `json:"fits_budget"`
	Recommended        bool   `json:"recommended"`
}

// quantLabelRe extracts a GGUF quant label from a filename — every suffix
// named in modelselection.md's quant landscape table AND the catalog's own
// seeded quantizations vocabulary (migration 0008: Q4_K_L/_XL, Q5_K_L/_XL,
// Q6_K_L exist as real rows, not just S/M). Go's regexp alternation is
// leftmost-first, not longest-match, so the _XL/_L variants must be listed
// before the bare "Q[0-9]_K_[SML]"/"Q[0-9]_K" alternatives or they're cut
// short (a real bug this pattern's earlier form had — "Q4_K_XL" matched as
// just "Q4_K" since the S/M/L class doesn't include X and the engine falls
// through to the bare Q[0-9]_K alternative rather than backtracking for a
// longer overall match).
var quantLabelRe = regexp.MustCompile(`(?i)(UD-Q[0-9]_K_XL|Q[0-9]_K_(?:XL|S|M|L)|IQ[0-9]_(?:NL|XS|S|M)|Q[0-9]_K|Q[0-9]_[01]|BF16|F16|F32)`)

// ExtractQuant returns the quant label found in filename, uppercased, or
// "" if none of the known patterns match.
func ExtractQuant(filename string) string {
	m := quantLabelRe.FindString(filename)
	return strings.ToUpper(m)
}

// QualityRank scores a quant label for "prefer this at equal file size" —
// grounded only in what modelselection.md states explicitly: IQ/UD variants
// beat K-quants at equal size, and among K-quants _L beats _M beats
// everything else. Lower is better.
func QualityRank(quant string) int {
	switch {
	case strings.HasPrefix(quant, "IQ"), strings.HasPrefix(quant, "UD-"):
		return 0
	case strings.HasSuffix(quant, "_L"):
		return 1
	case strings.HasSuffix(quant, "_M"):
		return 2
	default:
		return 3
	}
}

// RankCandidates evaluates every .gguf File in files against budgetBytes
// (0 ⇒ no fit check performed — every candidate reports FitsBudget=false
// and nothing is recommended: "recommend something that might not fit" is
// worse than recommending nothing). Recommendation = the largest fitting
// file, with QualityRank breaking ties among files within NearTopWindow of
// that size. Non-.gguf files in the input are ignored.
func RankCandidates(files []File, budgetBytes int64) ([]QuantCandidate, *QuantCandidate) {
	candidates := make([]QuantCandidate, 0, len(files))
	for _, f := range files {
		if f.IsDir || !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			continue
		}
		estVRAM := int64(float64(f.SizeBytes) * OneTwoXOverhead)
		candidates = append(candidates, QuantCandidate{
			Filename: f.Path, SizeBytes: f.SizeBytes, Quant: ExtractQuant(f.Path),
			EstimatedVRAMBytes: estVRAM,
			FitsBudget:         budgetBytes > 0 && estVRAM <= budgetBytes,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })

	if budgetBytes <= 0 {
		return candidates, nil
	}
	var topSize int64 = -1
	for _, c := range candidates {
		if c.FitsBudget {
			topSize = c.SizeBytes
			break // size-desc sorted, so the first fit is the largest
		}
	}
	if topSize < 0 {
		return candidates, nil
	}
	bestIdx := -1
	for i, c := range candidates {
		if !c.FitsBudget || topSize-c.SizeBytes > NearTopWindow {
			continue
		}
		if bestIdx == -1 || QualityRank(c.Quant) < QualityRank(candidates[bestIdx].Quant) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return candidates, nil
	}
	candidates[bestIdx].Recommended = true
	rec := candidates[bestIdx]
	return candidates, &rec
}

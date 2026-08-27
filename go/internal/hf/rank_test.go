// SPDX-License-Identifier: Apache-2.0

package hf

import "testing"

func TestExtractQuantRecognizesLongestAlternativeFirst(t *testing.T) {
	cases := map[string]string{
		"model-UD-Q4_K_XL.gguf": "UD-Q4_K_XL",
		"model-Q4_K_XL.gguf":    "Q4_K_XL",
		"model-Q4_K_L.gguf":     "Q4_K_L",
		"model-Q5_K_XL.gguf":    "Q5_K_XL",
		"model-Q4_K_M.gguf":     "Q4_K_M",
		"model-IQ4_NL.gguf":     "IQ4_NL",
		"model-F16.gguf":        "F16",
		"model-nothing.gguf":    "",
	}
	for name, want := range cases {
		if got := ExtractQuant(name); got != want {
			t.Errorf("ExtractQuant(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestQualityRankOrdering(t *testing.T) {
	if QualityRank("IQ4_NL") >= QualityRank("Q4_K_L") {
		t.Error("IQ quants must rank better than K-quant _L")
	}
	if QualityRank("Q4_K_L") >= QualityRank("Q4_K_M") {
		t.Error("_L must rank better than _M")
	}
	if QualityRank("Q4_K_M") >= QualityRank("Q4_K_S") {
		t.Error("_M must rank better than an unqualified/other suffix")
	}
}

func TestRankCandidatesRecommendsLargestFitting(t *testing.T) {
	files := []File{
		{Path: "model-Q8_0.gguf", SizeBytes: 8 << 30},   // too big
		{Path: "model-Q4_K_M.gguf", SizeBytes: 4 << 30}, // fits
		{Path: "model-Q4_K_S.gguf", SizeBytes: 3 << 30}, // fits, smaller
		{Path: "README.md", SizeBytes: 100},             // not a gguf — ignored
	}
	budget := int64(5 << 30) // 5 GiB
	candidates, rec := RankCandidates(files, budget)
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates, want 3 (README.md must be excluded)", len(candidates))
	}
	if rec == nil || rec.Filename != "model-Q4_K_M.gguf" {
		t.Fatalf("recommended = %+v, want model-Q4_K_M.gguf (largest fitting)", rec)
	}
}

func TestRankCandidatesPrefersQualityWithinNearTopWindow(t *testing.T) {
	files := []File{
		{Path: "model-Q4_K_M.gguf", SizeBytes: 4 << 30},
		{Path: "model-IQ4_NL.gguf", SizeBytes: 4<<30 - (1 << 20)}, // slightly smaller, within 1 GiB window
	}
	_, rec := RankCandidates(files, 10<<30)
	if rec == nil || rec.Filename != "model-IQ4_NL.gguf" {
		t.Fatalf("recommended = %+v, want the IQ4_NL file (better quality, within NearTopWindow)", rec)
	}
}

func TestRankCandidatesNothingFitsRecommendsNil(t *testing.T) {
	files := []File{{Path: "model-Q8_0.gguf", SizeBytes: 80 << 30}}
	candidates, rec := RankCandidates(files, 8<<30)
	if rec != nil {
		t.Errorf("recommended = %+v, want nil when nothing fits", rec)
	}
	if candidates[0].FitsBudget {
		t.Error("candidate should report FitsBudget=false")
	}
}

func TestRankCandidatesZeroBudgetRecommendsNothing(t *testing.T) {
	files := []File{{Path: "model-Q4_K_M.gguf", SizeBytes: 1 << 30}}
	candidates, rec := RankCandidates(files, 0)
	if rec != nil {
		t.Error("a zero budget must never recommend anything")
	}
	if candidates[0].FitsBudget {
		t.Error("FitsBudget must be false with no budget to compare against")
	}
}

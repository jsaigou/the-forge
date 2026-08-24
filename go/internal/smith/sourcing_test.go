// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/smith/web"
)

func TestExtractQuant(t *testing.T) {
	cases := map[string]string{
		"model-Q4_K_M.gguf":       "Q4_K_M",
		"model-Q4_K_L.gguf":       "Q4_K_L",
		"model-IQ4_NL.gguf":       "IQ4_NL",
		"model-UD-Q4_K_XL.gguf":   "UD-Q4_K_XL",
		"model-Q8_0.gguf":         "Q8_0",
		"model-BF16.gguf":         "BF16",
		"model-nonsense-tag.gguf": "",
	}
	for in, want := range cases {
		if got := extractQuant(in); got != want {
			t.Errorf("extractQuant(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQualityRank_MatchesDocumentedOrdering(t *testing.T) {
	// modelselection.md: "at the same file size, prefer IQ4_NL > Q4_K_L > Q4_K_M".
	if !(qualityRank("IQ4_NL") < qualityRank("Q4_K_L") && qualityRank("Q4_K_L") < qualityRank("Q4_K_M")) {
		t.Errorf("ranks: IQ4_NL=%d Q4_K_L=%d Q4_K_M=%d, want strictly increasing",
			qualityRank("IQ4_NL"), qualityRank("Q4_K_L"), qualityRank("Q4_K_M"))
	}
}

func TestRankCandidates_NoBudget_NothingRecommended(t *testing.T) {
	files := []HFFile{{Path: "a-Q4_K_M.gguf", SizeBytes: 4 << 30}}
	candidates, rec := rankCandidates(files, 0)
	if rec != nil {
		t.Error("expected no recommendation with budgetBytes=0 — never guess a fit")
	}
	if len(candidates) != 1 || candidates[0].FitsBudget {
		t.Errorf("candidates = %+v, want FitsBudget=false with no budget", candidates)
	}
}

func TestRankCandidates_PicksLargestThatFits(t *testing.T) {
	budget := int64(float64(10<<30) * oneTwoXOverhead) // fits up to a 10 GiB file
	files := []HFFile{
		{Path: "small-Q4_K_M.gguf", SizeBytes: 4 << 30},
		{Path: "mid-Q5_K_M.gguf", SizeBytes: 8 << 30},
		{Path: "toobig-Q8_0.gguf", SizeBytes: 20 << 30},
	}
	candidates, rec := rankCandidates(files, budget)
	if rec == nil {
		t.Fatal("expected a recommendation")
	}
	if rec.Filename != "mid-Q5_K_M.gguf" {
		t.Errorf("recommended = %q, want the largest fitting file", rec.Filename)
	}
	for _, c := range candidates {
		want := c.SizeBytes <= 8<<30
		if c.FitsBudget != want {
			t.Errorf("%s: FitsBudget = %v, want %v", c.Filename, c.FitsBudget, want)
		}
	}
}

func TestRankCandidates_PrefersIQOverKQuantAtNearEqualSize(t *testing.T) {
	// Two files within the 1 GiB near-top window — IQ4_NL must win over
	// Q4_K_M even though it's very slightly smaller.
	budget := int64(float64(10<<30) * oneTwoXOverhead)
	files := []HFFile{
		{Path: "a-Q4_K_M.gguf", SizeBytes: 10 << 30},
		{Path: "b-IQ4_NL.gguf", SizeBytes: (10 << 30) - (200 << 20)}, // 200 MiB smaller, well within 1 GiB
	}
	_, rec := rankCandidates(files, budget)
	if rec == nil || rec.Filename != "b-IQ4_NL.gguf" {
		t.Errorf("recommended = %+v, want the IQ4_NL file (documented preference at near-equal size)", rec)
	}
}

func TestRankCandidates_PrefersLOverMWithinWindow(t *testing.T) {
	budget := int64(float64(10<<30) * oneTwoXOverhead)
	files := []HFFile{
		{Path: "a-Q4_K_M.gguf", SizeBytes: 10 << 30},
		{Path: "b-Q4_K_L.gguf", SizeBytes: (10 << 30) - (300 << 20)}, // ~0.3 GB smaller, the doc's own figure
	}
	_, rec := rankCandidates(files, budget)
	if rec == nil || rec.Filename != "b-Q4_K_L.gguf" {
		t.Errorf("recommended = %+v, want the _L file per modelselection.md's explicit rule", rec)
	}
}

func TestRankCandidates_FarApartSizeWinsOnSizeNotQuant(t *testing.T) {
	// An IQ4_NL file far smaller than the top fitting Q4_K_M must NOT beat
	// it — qualityRank only breaks TIES near the top, never overrides size
	// outside the window (the doc has no cross-size quality claim to act on).
	budget := int64(float64(10<<30) * oneTwoXOverhead)
	files := []HFFile{
		{Path: "big-Q4_K_M.gguf", SizeBytes: 10 << 30},
		{Path: "small-IQ4_NL.gguf", SizeBytes: 3 << 30},
	}
	_, rec := rankCandidates(files, budget)
	if rec == nil || rec.Filename != "big-Q4_K_M.gguf" {
		t.Errorf("recommended = %+v, want the larger file (size is the primary key)", rec)
	}
}

func TestRankCandidates_NothingFits(t *testing.T) {
	files := []HFFile{{Path: "huge-Q8_0.gguf", SizeBytes: 100 << 30}}
	candidates, rec := rankCandidates(files, 1<<30)
	if rec != nil {
		t.Error("expected no recommendation when nothing fits")
	}
	if candidates[0].FitsBudget {
		t.Error("expected FitsBudget=false")
	}
}

func TestEvaluate_EmptyRepoRejected(t *testing.T) {
	s := New(Deps{Web: &fakeWebService{}})
	if _, err := s.Evaluate(context.Background(), "  ", 0); err == nil {
		t.Error("expected an error for an empty repo")
	}
}

func TestEvaluate_WebUnwired(t *testing.T) {
	s := New(Deps{})
	if _, err := s.Evaluate(context.Background(), "org/repo", 0); err == nil {
		t.Error("expected an error when web research isn't wired")
	}
}

func TestEvaluate_RealShape(t *testing.T) {
	treeJSON := `[
		{"type":"file","path":"README.md","size":100},
		{"type":"file","path":"model-Q4_K_M.gguf","size":4294967296},
		{"type":"file","path":"model-Q4_K_L.gguf","size":4402341888},
		{"type":"directory","path":"subdir","size":0}
	]`
	svc := &fakeWebService{fetchDocs: map[string]*web.Document{
		hfRepoModelURL("org/repo"): {Text: `{"id":"org/repo"}`},
		hfRepoTreeURL("org/repo"):  {Text: treeJSON},
	}}
	s := New(Deps{Web: svc})
	eval, err := s.Evaluate(context.Background(), "org/repo", 10<<30)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(eval.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want exactly 2 (README.md and the directory must be excluded)", eval.Candidates)
	}
	if eval.Recommended == nil {
		t.Fatal("expected a recommendation")
	}
	if len(eval.DownloadSteps) == 0 {
		t.Error("expected real download steps to be rendered")
	}
}

func TestEvaluate_RepoNotFound(t *testing.T) {
	s := New(Deps{Web: &fakeWebService{}}) // no docs configured -> fetch errors
	if _, err := s.Evaluate(context.Background(), "nonexistent/repo", 0); err == nil {
		t.Error("expected an error for an unreachable/nonexistent repo")
	}
}

// SPDX-License-Identifier: Apache-2.0

package smith

// sourcing.go implements smith P6's model sourcing module (docs/v5-smith.md
// §4.9 FR4): given a HuggingFace repo, fetch its real GGUF file listing
// (sizes from the repo's own tree API, never guessed) and rank candidates
// against modelselection.md's two concretely-documented rules — the 1.2×
// VRAM rule and "IQ4_NL > Q4_K_L > Q4_K_M at equal size, prefer _L over _M
// when the delta is small". Deliberately does NOT auto-propose a
// catalog_change in this sprint: writing Model/Variant/Artifact/Offering
// rows together needs the model's ID to exist before the variant/artifact
// rows can reference it, which spans more than one action's atomic
// approval — a real cross-action sequencing problem, not something to
// fake with a guessed ID. Evaluate() is read-only research; a runbook
// carries the real download command so the operator finishes the job by
// hand (docs/adding-a-model.md), same as the rest of P6's non-executable
// guidance paths. execute.go's dispatchCatalogChange and this file's
// applyCatalogChange are the real write path for catalog rows: P3smith
// minimally implemented applyCatalogChange for artifact rows (fetch_model's
// final step writes through it); Model/Variant/Offering sequencing remains
// the follow-up sprint's work.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// ErrCatalogChangeUnwired is returned by catalog_change execution when
// Deps.Catalog is nil.
var ErrCatalogChangeUnwired = errors.New("smith: catalog not wired")

// applyCatalogChange executes one catalog_change action's detail against
// store.Catalog.
//
// P3smith status — MINIMALLY implemented, deliberately narrow: Op
// create|update on Table "artifact" only, which is all fetch_model's final
// step needs. Every other table ("model"|"variant"|"offering") still
// returns "not implemented" exactly as this function always has — wiring
// those needs sourcing.go's cross-row sequencing (a Model must exist
// before Variant/Artifact rows can reference it; see the package doc's
// sequencing note) and stays with that follow-up sprint. The dispatch SEAM
// itself is real and unchanged: KindCatalogChange actions flow through
// execute.go's dispatchCatalogChange into THIS function, and fetch_model's
// opFetchCatalogLink invokes it as a library call behind the same seam, so
// whatever validation lands here later covers both callers.
func (s *Smith) applyCatalogChange(ctx context.Context, d catalogChangeDetail) error {
	if s.d.Catalog == nil {
		return ErrCatalogChangeUnwired
	}
	switch d.Table {
	case "artifact":
		var a store.Artifact
		if err := json.Unmarshal(d.Row, &a); err != nil {
			return fmt.Errorf("smith: catalog_change artifact row: %w", err)
		}
		switch d.Op {
		case "create":
			if _, err := s.d.Catalog.CreateArtifact(ctx, a); err != nil {
				return fmt.Errorf("smith: catalog_change create artifact: %w", err)
			}
			return nil
		case "update":
			if a.ID == 0 {
				return errors.New("smith: catalog_change update artifact requires a row id")
			}
			if err := s.d.Catalog.UpdateArtifact(ctx, a); err != nil {
				return fmt.Errorf("smith: catalog_change update artifact %d: %w", a.ID, err)
			}
			return nil
		default:
			return fmt.Errorf("smith: catalog_change op %q not supported (create|update)", d.Op)
		}
	default:
		return fmt.Errorf("smith: catalog_change execution not yet implemented for table %q (artifact only)", d.Table)
	}
}

// sourcingFetchTimeout bounds the two HF API calls Evaluate makes.
const sourcingFetchTimeout = 30 * time.Second

// sourcingCacheTTL — HF repo file listings change rarely; a long TTL means
// re-evaluating the same repo minutes apart costs zero network calls.
const sourcingCacheTTL = 6 * time.Hour

// oneTwoXOverhead is modelselection.md's "VRAM needed ≈ GGUF file size ×
// 1.2" rule (`modelselection.md` §"The 1.2× Rule") — KV cache, runtime
// buffers, and compute workspace at modest context.
const oneTwoXOverhead = 1.2

// HFFile is one real file in a HuggingFace repo's tree, as reported by the
// repo's own API — never guessed or derived from a filename pattern alone.
type HFFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// QuantCandidate is one GGUF file evaluated against a memory budget.
type QuantCandidate struct {
	Filename           string `json:"filename"`
	SizeBytes          int64  `json:"size_bytes"`
	Quant              string `json:"quant"`                // extracted label, "" if unrecognized
	EstimatedVRAMBytes int64  `json:"estimated_vram_bytes"` // SizeBytes * 1.2
	FitsBudget         bool   `json:"fits_budget"`
	Recommended        bool   `json:"recommended"`
}

// SourcingEvaluation is Evaluate's result.
type SourcingEvaluation struct {
	Repo          string           `json:"repo"`
	BudgetBytes   int64            `json:"budget_bytes"`
	Candidates    []QuantCandidate `json:"candidates"`
	Recommended   *QuantCandidate  `json:"recommended,omitempty"`
	DownloadSteps []RunbookStep    `json:"download_steps,omitempty"`
	Cached        bool             `json:"cached"`
}

// hfTreeEntry mirrors HuggingFace's GET /api/models/{repo}/tree/main
// response shape — one entry per file/dir in the repo root.
type hfTreeEntry struct {
	Type string `json:"type"` // "file" | "directory"
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// hfRepoTreeURL/hfRepoModelURL build the two real HF API endpoints Evaluate
// reads — the tree endpoint for real file sizes (the models endpoint's
// `siblings` list often omits size), the models endpoint to confirm the
// repo exists at all before reporting an empty candidate list as "found
// nothing" rather than "repo doesn't exist".
func hfRepoTreeURL(repo string) string {
	return "https://huggingface.co/api/models/" + repo + "/tree/main"
}
func hfRepoModelURL(repo string) string { return "https://huggingface.co/api/models/" + repo }

// Evaluate fetches repo's real file listing from HuggingFace and ranks its
// GGUF files against budgetBytes (0 ⇒ falls back to the live collector
// snapshot's GTT total, when a Source is wired — "what would actually fit
// on this box right now"). Web research must be enabled (Deps.Web
// non-nil, smith.web.enabled) — this makes exactly the same two calls
// blocked_work_recheck/binary_versions already make through the same
// service, no separate HF-specific config.
func (s *Smith) Evaluate(ctx context.Context, repo string, budgetBytes int64) (SourcingEvaluation, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return SourcingEvaluation{}, errors.New("smith: sourcing evaluate requires a non-empty hf_repo")
	}
	if s.d.Web == nil {
		return SourcingEvaluation{}, errors.New("smith: web research not wired")
	}
	if budgetBytes <= 0 {
		budgetBytes = s.liveGTTTotalBytes()
	}

	fctx, cancel := context.WithTimeout(ctx, sourcingFetchTimeout)
	defer cancel()

	if _, err := s.d.Web.FetchDirect(fctx, hfRepoModelURL(repo), sourcingCacheTTL); err != nil {
		return SourcingEvaluation{}, fmt.Errorf("smith: hf repo %q: %w", repo, err)
	}
	treeDoc, err := s.d.Web.FetchDirect(fctx, hfRepoTreeURL(repo), sourcingCacheTTL)
	if err != nil {
		return SourcingEvaluation{}, fmt.Errorf("smith: hf repo %q file tree: %w", repo, err)
	}
	var entries []hfTreeEntry
	if err := json.Unmarshal([]byte(treeDoc.Text), &entries); err != nil {
		return SourcingEvaluation{}, fmt.Errorf("smith: parse hf tree response for %q: %w", repo, err)
	}

	var files []HFFile
	for _, e := range entries {
		if e.Type == "file" && strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			files = append(files, HFFile{Path: e.Path, SizeBytes: e.Size})
		}
	}

	eval := SourcingEvaluation{Repo: repo, BudgetBytes: budgetBytes, Cached: treeDoc.Cached}
	eval.Candidates, eval.Recommended = rankCandidates(files, budgetBytes)
	if eval.Recommended != nil {
		eval.DownloadSteps = downloadSteps(repo, eval.Recommended.Filename)
	}
	return eval, nil
}

// liveGTTTotalBytes reads the live collector snapshot's total GTT — the
// real "what fits on this box" figure when the caller doesn't supply one.
// 0 (no budget check performed) when Source is nil or no snapshot exists
// yet.
func (s *Smith) liveGTTTotalBytes() int64 {
	if s.d.Source == nil {
		return 0
	}
	snap := s.d.Source.Current()
	if snap == nil || snap.Metrics.GTTTotalBytes == nil {
		return 0
	}
	return *snap.Metrics.GTTTotalBytes
}

// quantLabelRe extracts a GGUF quant label from a filename — every suffix
// actually named in modelselection.md's quant landscape table, longest
// alternatives first so e.g. "UD-Q4_K_XL" doesn't partially match "Q4_K".
var quantLabelRe = regexp.MustCompile(`(?i)(UD-Q[0-9]_K_XL|IQ[0-9]_(?:NL|XS|S|M)|Q[0-9]_K_[SML]|Q[0-9]_K|Q[0-9]_[01]|BF16|F16|F32)`)

// extractQuant returns the quant label found in filename, uppercased, or
// "" if none of the known patterns match.
func extractQuant(filename string) string {
	m := quantLabelRe.FindString(filename)
	return strings.ToUpper(m)
}

// qualityRank scores a quant label for "prefer this at equal file size" —
// grounded ONLY in what modelselection.md states explicitly: IQ/UD variants
// beat K-quants at equal size, and among K-quants _L beats _M beats
// everything else (the doc: "_L ... almost always worth choosing over M").
// Lower is better. This is deliberately NOT a cross-family/cross-size
// quality model — ranking, e.g., a 40 GB Q4_K_M against a 25 GB IQ4_NS is a
// real quality-vs-size tradeoff the doc doesn't resolve, so Evaluate never
// tries to; size (whichever fits the biggest) is the primary sort key, and
// this rank only breaks ties among near-equal sizes.
func qualityRank(quant string) int {
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

// nearTopWindow is how close (in bytes) to the largest fitting candidate a
// smaller one must be for qualityRank to break the tie instead of raw size
// — modelselection.md's own figure for _L vs _M ("usually only ~0.3 GB
// larger... almost always worth choosing"), rounded up generously.
const nearTopWindow = 1 << 30 // 1 GiB

// rankCandidates evaluates every GGUF file against budgetBytes (0 ⇒ no
// fit check performed — every candidate reports FitsBudget=false and
// nothing is recommended, since "recommend something that might not fit"
// is worse than recommending nothing). Recommendation = the largest
// fitting file, with qualityRank breaking ties among files within
// nearTopWindow of that size.
func rankCandidates(files []HFFile, budgetBytes int64) ([]QuantCandidate, *QuantCandidate) {
	candidates := make([]QuantCandidate, 0, len(files))
	for _, f := range files {
		estVRAM := int64(float64(f.SizeBytes) * oneTwoXOverhead)
		c := QuantCandidate{
			Filename: f.Path, SizeBytes: f.SizeBytes, Quant: extractQuant(f.Path),
			EstimatedVRAMBytes: estVRAM,
			FitsBudget:         budgetBytes > 0 && estVRAM <= budgetBytes,
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })

	if budgetBytes <= 0 {
		return candidates, nil
	}
	var topSize int64 = -1
	for _, c := range candidates {
		if c.FitsBudget {
			topSize = c.SizeBytes
			break // candidates is sorted size-desc, so the first fit is the largest
		}
	}
	if topSize < 0 {
		return candidates, nil
	}
	bestIdx := -1
	for i, c := range candidates {
		if !c.FitsBudget || topSize-c.SizeBytes > nearTopWindow {
			continue
		}
		if bestIdx == -1 || qualityRank(c.Quant) < qualityRank(candidates[bestIdx].Quant) {
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

// downloadSteps renders docs/adding-a-model.md's real `hf download`
// command for the recommended file — a runbook, never something smith
// executes (the download itself can be tens of GB and belongs behind an
// operator's own bandwidth/disk judgment).
func downloadSteps(repo, filename string) []RunbookStep {
	return []RunbookStep{
		{
			Title:         "Download the model file",
			Command:       fmt.Sprintf("cd /opt/forge/models && hf download %s %s --local-dir .", repo, filename),
			Why:           "smith only evaluates candidates against the memory budget — the download itself can be tens of GB and stays behind an operator's own bandwidth/disk judgment (§4.9)",
			Verify:        "the file appears in the models directory at the expected size",
			VerifyCommand: fmt.Sprintf("ls -lh /opt/forge/models/%s", filename),
		},
	}
}

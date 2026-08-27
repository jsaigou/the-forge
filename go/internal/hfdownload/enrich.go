// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"strings"
)

// enrich.go — Phase 4 asset enrichment: best-effort metadata gathered
// WHILE the download runs (runWorker launches this in its own goroutine
// before the file loop starts), never blocking or failing the download.
//
// Logo vendoring itself stays a human-run `npm run icons` build step
// (docs/v5-smith.md §4.9) — nothing here writes an SVG. The only logo this
// package ever sets is one that ALREADY exists on an existing catalog Model
// sharing the same Creator string; there's no Go port of the frontend's
// creatorIcon.ts alias table here (that would be a second copy of the same
// knowledge, guaranteed to drift), so a first-of-its-kind creator gets no
// logo and falls back to the existing letter-badge default, same as any
// other model without one today.

// Enrichment is best-effort metadata collected alongside a download.
type Enrichment struct {
	Description string
	LicenseName string
	Tags        []string
	Logo        string
	LogoDark    string
}

// enrich fetches repo's model card and resolves a logo from an existing
// catalog entry sharing the same creator, if any. Every failure is logged
// and swallowed — an enrichment miss never fails the job it's decorating.
func (s *Service) enrich(ctx context.Context, repo string) Enrichment {
	var out Enrichment
	if s.d.HF != nil {
		card, err := s.d.HF.Card(ctx, repo)
		if err != nil {
			s.d.logf("hfdownload: enrich %q: card fetch failed (best-effort, ignored): %v", repo, err)
		} else {
			out.Description = card.Description
			out.LicenseName = card.License
			out.Tags = card.Tags
		}
	}
	if creator := creatorFromRepo(repo); creator != "" {
		out.Logo, out.LogoDark = s.resolveLogoFromCreator(ctx, creator)
	}
	return out
}

// creatorFromRepo returns the org portion of "org/name", or "" for a bare
// repo id with no org.
func creatorFromRepo(repo string) string {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i]
	}
	return ""
}

// resolveLogoFromCreator reuses whatever logo an existing catalog Model
// from the same creator already has, rather than guessing or vendoring a
// new one.
func (s *Service) resolveLogoFromCreator(ctx context.Context, creator string) (logo, logoDark string) {
	if creator == "" || s.d.Store == nil {
		return "", ""
	}
	models, err := s.d.Store.Catalog().ListModels(ctx)
	if err != nil {
		return "", ""
	}
	for _, m := range models {
		if strings.EqualFold(m.Creator, creator) && m.Logo != "" {
			return m.Logo, m.LogoDark
		}
	}
	return "", ""
}

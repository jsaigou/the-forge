// SPDX-License-Identifier: Apache-2.0

package smith

// web_research.go — smith P5 (docs/v5-smith.md §4.8): the explicit-opt-in
// chat research path (researchForTurn, wired into reasoning.go's runTurn)
// plus the delegating nil-tolerant readers httpapi consumes for
// GET /smith/status and GET/PUT /smith/settings.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
)

// webProbeInterval is how often scheduleLoop re-probes web-provider
// reachability in the background, independent of real usage (which also
// updates the same status map via recordAttempt on every call).
const webProbeInterval = 10 * time.Minute

// researchForTurn's budgets (docs/v5-smith.md §4.8). webFetchTopN is fetched
// CONCURRENTLY under one overall webResearchBudget — not sequentially, and
// comfortably inside chatTimeout (360s) even in the worst case.
const (
	webResearchSearchLimit = 5
	webFetchTopN           = 2
	webResearchBudget      = 45 * time.Second
	// webSourceSnippetChars caps MessageSource.Snippet — the persisted/
	// rendered summary is much shorter than webDocContextChars' full
	// in-context document text (reasoning.go).
	webSourceSnippetChars = 300
)

// WebConfig returns the resolved smith.web.* settings. Never an error —
// nil Settings degrades to web.DefaultConfig() (disabled).
func (s *Smith) WebConfig(ctx context.Context) web.Config {
	return web.LoadConfig(ctx, s.d.Settings)
}

// WebProviders reports the current configuration + last-known reachability
// for every web-research adapter. Empty, never nil, when Web is unwired.
func (s *Smith) WebProviders(ctx context.Context) []web.ProviderStatus {
	if s.d.Web == nil {
		return []web.ProviderStatus{}
	}
	return s.d.Web.Providers(ctx)
}

// ProbeWeb actively re-checks web-provider reachability. nil Web ⇒ no-op.
// Safe to call from a background goroutine (Start()'s one-shot probe,
// scheduleLoop's periodic re-probe) or synchronously (the settings PUT
// handler, per docs/v5-smith.md §4.8: "probe on save/startup").
func (s *Smith) ProbeWeb(ctx context.Context) {
	if s.d.Web == nil {
		return
	}
	s.d.Web.Probe(ctx)
	s.webProbeMu.Lock()
	s.lastWebProbeAt = s.d.Now()
	s.webProbeMu.Unlock()
}

// maybeProbeWeb re-probes when the last probe is older than
// webProbeInterval, called from scheduleLoop's 1-minute tick. Runs the
// probe itself in a goroutine so a slow/hung provider never delays the
// scheduler's own sweep timing.
func (s *Smith) maybeProbeWeb(ctx context.Context, now time.Time) {
	if s.d.Web == nil {
		return
	}
	s.webProbeMu.Lock()
	due := now.Sub(s.lastWebProbeAt) >= webProbeInterval
	s.webProbeMu.Unlock()
	if due {
		go s.ProbeWeb(ctx)
	}
}

// researchForTurn is P5's explicit web:true chat step: search, then fetch
// the top few results concurrently, persisting what was read via
// setMessageSources immediately — before the LLM (or the deterministic
// renderer) ever sees it, so a turn that fails afterward still records what
// smith looked at (plan decision D2: "never lose what you read"). Returns
// the persisted-shape sources, the full Documents for buildContext's
// web-sources block, and a non-empty notice when research degraded (never
// an error — the caller must never fail the turn over this).
func (s *Smith) researchForTurn(ctx context.Context, msgID int64, userText string) (sources []MessageSource, docs []*web.Document, notice string) {
	if s.d.Web == nil {
		return nil, nil, "smith: web research is unavailable (no provider wired) — answering from local evidence only."
	}
	rctx, cancel := context.WithTimeout(ctx, webResearchBudget)
	defer cancel()

	results, err := s.d.Web.Search(rctx, userText, webResearchSearchLimit)
	if err != nil {
		if errors.Is(err, web.ErrDisabled) {
			return nil, nil, "smith: web research is disabled in Settings — answering from local evidence only."
		}
		return nil, nil, "smith: web search failed (" + err.Error() + ") — answering from local evidence only."
	}
	if len(results) == 0 {
		return nil, nil, "smith: web search returned no results — answering from local evidence only."
	}

	n := min(webFetchTopN, len(results))
	fetched := make([]*web.Document, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doc, err := s.d.Web.Fetch(rctx, results[i].URL)
			if err != nil {
				s.logf("web research: fetch %q: %v", results[i].URL, err)
				return
			}
			fetched[i] = doc // index-owned write, no shared-append race
		}(i)
	}
	wg.Wait()

	for _, doc := range fetched {
		if doc == nil {
			continue
		}
		docs = append(docs, doc)
		sources = append(sources, MessageSource{
			Provider:  doc.Provider,
			URL:       doc.URL,
			Title:     doc.Title,
			Snippet:   truncateForContext(doc.Text, webSourceSnippetChars),
			FetchedAt: doc.FetchedAt.Unix(),
			Cached:    doc.Cached,
		})
	}
	if len(sources) == 0 {
		return nil, nil, "smith: web fetch failed for every search result — answering from local evidence only."
	}
	if err := s.setMessageSources(ctx, msgID, sources); err != nil {
		s.logf("web research: set message sources: %v", err)
	}
	return sources, docs, ""
}

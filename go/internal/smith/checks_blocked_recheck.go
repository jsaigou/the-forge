// SPDX-License-Identifier: Apache-2.0

package smith

// checks_blocked_recheck.go implements smith P5's narrow deterministic
// auto-use of web research (docs/v5-smith.md §4.7/§4.9, the P5 plan's
// "blocked-item recheck"): a deep-sweep-only check that re-tests whether an
// externally-blocked item from the operator-local blocked-work tracker has
// unblocked.
//
// Four hard bounds keep this from becoming a background crawler:
//  1. Only Status=="open" items with a non-empty WhereCheck URL are
//     candidates — closed/resolved items and items with nothing to fetch
//     are skipped for free.
//  2. The cache IS the cooldown: web.CheckUnblockSignal's ttl argument
//     (blockedRecheckCacheTTL, 7 days) means an item whose cache entry is
//     still fresh costs zero network calls; the sweep stops after
//     blockedRecheckMaxNetworkFetches real fetches regardless of how many
//     candidates remain, so the very last URLs in file order naturally get
//     picked up on a later sweep instead of every URL being probed every
//     run — self-rotating, no extra state table needed.
//  3. The whole check runs under its own bounded context
//     (blockedRecheckTimeout), independent of the sweep's own timeout.
//  4. env.Web == nil, or web research disabled in Settings, ⇒ a plain
//     skipFinding with zero network calls — never a silent partial result.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/store"
)

const (
	// blockedRecheckMaxNetworkFetches bounds real (non-cached) fetches per
	// sweep — the plan's "≤3 fetches/day" budget, since blocked_work_recheck
	// only runs in the deep sweep (default 24h).
	blockedRecheckMaxNetworkFetches = 3
	// blockedRecheckCacheTTL is the freshness window CheckUnblockSignal
	// tolerates before it counts as a real network fetch — the actual
	// cooldown mechanism (bound 2 above).
	blockedRecheckCacheTTL = 7 * 24 * time.Hour
	// blockedRecheckTimeout bounds the whole check independent of any
	// individual fetch's own timeout.
	blockedRecheckTimeout = 90 * time.Second
)

// blockedRecheckSignal is one item whose recheck found a positive signal —
// the evidence shape rendered by the Diagnostics finding card.
type blockedRecheckSignal struct {
	ItemNumber int    `json:"item_number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Detail     string `json:"detail"`
}

// blockedRecheckEvidence is the finding's persisted Evidence shape. Hashes
// carries every URL's last-known content hash forward across sweeps — it is
// the state store for the non-GitHub hash-diff fallback signal (no new
// table: the previous sweep's own finding row is the state).
type blockedRecheckEvidence struct {
	CheckedItems   int                    `json:"checked_items"`
	OpenItems      int                    `json:"open_items"`
	NetworkFetches int                    `json:"network_fetches"`
	Signals        []blockedRecheckSignal `json:"signals"`
	Hashes         map[string]string      `json:"hashes"`
}

// lastBlockedRecheckHashes reads the most recent blocked_work_recheck
// finding's carried-forward hash map, or {} when there is no prior run (or
// no store wired) — never an error, this is best-effort continuity, not a
// hard dependency.
func lastBlockedRecheckHashes(ctx context.Context, db *store.DB) map[string]string {
	var evidenceRaw string
	err := db.SQL().QueryRowContext(ctx,
		`SELECT evidence FROM smith_findings WHERE check_id = 'blocked_work_recheck'
		 ORDER BY created_at DESC LIMIT 1`).Scan(&evidenceRaw)
	if err != nil {
		return map[string]string{}
	}
	var ev blockedRecheckEvidence
	if err := json.Unmarshal([]byte(evidenceRaw), &ev); err != nil || ev.Hashes == nil {
		return map[string]string{}
	}
	return ev.Hashes
}

// runBlockedWorkRecheck is the check registered in registry (checks.go).
// Pure function over CheckEnv per the house convention (see
// runBrainResolvable's doc comment) — env.BlockedItems is the tracker
// parsed live from the operator-local file (Deps.BlockedWorkPath,
// kb_investigations.go; layer-2 deployment data, never embedded). Thin
// wrapper over runBlockedWorkRecheckItems so tests can supply a controlled
// item list instead of depending on any particular deployment's tracker.
func runBlockedWorkRecheck(ctx context.Context, env *CheckEnv) Finding {
	return runBlockedWorkRecheckItems(ctx, env, env.BlockedItems)
}

func runBlockedWorkRecheckItems(ctx context.Context, env *CheckEnv, items []BlockedItem) Finding {
	const id = "blocked_work_recheck"
	if env.Web == nil {
		return skipFinding(id, "web research not wired")
	}

	var candidates []BlockedItem
	for _, item := range items {
		if item.Status != "open" || len(item.URLs) == 0 {
			continue
		}
		candidates = append(candidates, item)
	}
	if len(candidates) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary: "no open externally-blocked items with a where-to-check URL"}
	}

	prevHashes := map[string]string{}
	if env.Store != nil {
		prevHashes = lastBlockedRecheckHashes(ctx, env.Store)
	}
	hashes := make(map[string]string, len(prevHashes))
	for k, v := range prevHashes {
		hashes[k] = v // carried forward so an item this sweep doesn't reach keeps its baseline
	}

	rctx, cancel := context.WithTimeout(ctx, blockedRecheckTimeout)
	defer cancel()

	checkedItems := 0
	networkFetches := 0
	var signals []blockedRecheckSignal

outer:
	for _, item := range candidates {
		for _, url := range item.URLs {
			if networkFetches >= blockedRecheckMaxNetworkFetches {
				break outer
			}
			sig, sha, err := web.CheckUnblockSignal(rctx, env.Web, url, prevHashes[url], blockedRecheckCacheTTL)
			if err != nil {
				if errors.Is(err, web.ErrDisabled) {
					return skipFinding(id, "web research is disabled in Settings")
				}
				continue // one bad URL shouldn't abort the whole sweep
			}
			checkedItems++
			if !sig.Cached {
				networkFetches++
			}
			hashes[url] = sha
			if sig.Changed {
				signals = append(signals, blockedRecheckSignal{
					ItemNumber: item.Number, Title: item.Title, URL: url, Detail: sig.Detail,
				})
			}
		}
	}

	ev := blockedRecheckEvidence{
		CheckedItems: checkedItems, OpenItems: len(candidates),
		NetworkFetches: networkFetches, Signals: signals, Hashes: hashes,
	}
	evMap := map[string]any{
		"checked_items": ev.CheckedItems, "open_items": ev.OpenItems,
		"network_fetches": ev.NetworkFetches, "signals": ev.Signals, "hashes": ev.Hashes,
	}

	if len(signals) > 0 {
		// distinctSignalItems, not len(signals): one item can carry several
		// blocking URLs (item 2 has two — a merged PR and a closed one), so
		// signals can outnumber candidates. Found live 2026-08-11: the raw
		// len(signals) count produced a real "5 of 4 open blocked item(s)"
		// summary — signals counts per-URL hits, candidates counts items,
		// and nothing enforces signals <= candidates.
		items := make(map[int]bool, len(signals))
		for _, s := range signals {
			items[s.ItemNumber] = true
		}
		// Info, deliberately not warn: "possibly unblocked" is good news,
		// and warn would light up the Console alert path for a non-problem.
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary:  fmt.Sprintf("%d of %d open blocked item(s) may have unblocked — verify manually", len(items), len(candidates)),
			Evidence: evMap}
	}
	return Finding{CheckID: id, Severity: SeverityInfo,
		Summary:  fmt.Sprintf("%d open blocked item(s); %d rechecked (%d network fetch(es)); no change detected", len(candidates), checkedItems, networkFetches),
		Evidence: evMap}
}

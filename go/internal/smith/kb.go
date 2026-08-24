// SPDX-License-Identifier: Apache-2.0

package smith

// kb.go — the P4 knowledge base (docs/v5-smith.md §4.7). Two source
// classes behind one ranked search:
//
//  1. The embedded doc corpus (kb/corpus/*.md + kb/runbooks/*.md) — curated
//     extracts of docs/pitfalls.md, docs/modes.md incident sections,
//     docs/diagnosis-trial.md, modelselection.md, research.md, and
//     docs/v5-headroom-topology.md, built by cmd/kbsync from
//     kb/manifest.json (`go generate ./internal/smith/...` re-runs it).
//     Versioned with the binary — no deploy-sync problem, and no runtime
//     dependency on docs/ existing on ForgeHost.
//  2. Live DB evidence — notifications, mode_history, audit_log,
//     model_profiles, smith_findings. Store nil ⇒ these contribute nothing
//     and the corpus still answers, same nil-tolerance convention as every
//     other Deps field.
//
// Ranking v1 is keyword scoring (§4.7: FTS5 only if profiling shows
// keyword search is inadequate) — title/slug matches outweigh body
// matches, normalized by chunk length so a long chunk doesn't win purely
// on repetition.
//
// checks.go's KBRefs (e.g. "pitfalls:gtt-ceiling") are exactly the `ref`
// values chunks carry here; kb_test.go's ref-integrity test walks the
// check registry and asserts every emitted ref resolves via KBLookup.

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"sort"
	"strings"
)

//go:generate go run ../../cmd/kbsync

//go:embed kb/corpus/*.md
var kbCorpusFS embed.FS

//go:embed kb/runbooks/*.md
var kbRunbooksFS embed.FS

// Chunk is one embedded KB entry — a curated doc extract or a hand-authored
// runbook. Ref is the exact string checks.go's KBRefs and the FE's finding
// cards use to resolve one ("pitfalls:gtt-ceiling").
type Chunk struct {
	Ref      string `json:"ref"`
	Doc      string `json:"doc"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Body     string `json:"body"`
}

// KBResult is one ranked hit from KBSearch, doc chunk or live-DB row alike
// — the two source classes share one wire shape so the FE (and Tier 2
// context assembly) don't need to special-case which kind matched.
type KBResult struct {
	Kind  string  `json:"kind"` // doc | notification | mode_history | audit | profile | finding
	Ref   string  `json:"ref"`
	Title string  `json:"title"`
	Body  string  `json:"body"`
	Score float64 `json:"score"`
	TS    *int64  `json:"ts"` // nil for doc chunks (versioned with the binary, not dated)
}

var (
	kbChunks      []Chunk
	kbChunksByRef map[string]Chunk
)

func init() {
	kbChunks = mustLoadEmbeddedChunks()
	kbChunksByRef = make(map[string]Chunk, len(kbChunks))
	for _, c := range kbChunks {
		kbChunksByRef[c.Ref] = c
	}
}

// mustLoadEmbeddedChunks parses every embedded corpus/runbook file. A parse
// failure here can only mean the embedded content itself is malformed
// (cmd/kbsync's own output, or a hand-authored runbook with a broken
// header) — a build-time invariant, not a runtime/Deps condition, so it
// panics rather than degrading silently. kb_sync_test.go and the package's
// own tests both exercise this path on every `go test`, which is what
// actually catches it in practice.
func mustLoadEmbeddedChunks() []Chunk {
	var out []Chunk
	for _, src := range []struct {
		fsys fs.FS
		dir  string
	}{
		{kbCorpusFS, "kb/corpus"},
		{kbRunbooksFS, "kb/runbooks"},
	} {
		entries, err := fs.ReadDir(src.fsys, src.dir)
		if err != nil {
			panic(fmt.Sprintf("smith: kb: read %s: %v", src.dir, err))
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			raw, err := fs.ReadFile(src.fsys, src.dir+"/"+e.Name())
			if err != nil {
				panic(fmt.Sprintf("smith: kb: read %s/%s: %v", src.dir, e.Name(), err))
			}
			c, err := parseChunkFile(raw)
			if err != nil {
				panic(fmt.Sprintf("smith: kb: parse %s/%s: %v", src.dir, e.Name(), err))
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// parseChunkFile reads cmd/kbsync's on-disk chunk format: a small
// `key: value` header, one blank line, then the body verbatim. Not YAML —
// no parser dependency needed on either side, and the header never needs
// more than flat scalar fields.
func parseChunkFile(raw []byte) (Chunk, error) {
	parts := strings.SplitN(string(raw), "\n\n", 2)
	if len(parts) != 2 {
		return Chunk{}, fmt.Errorf("no header/body separator")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(parts[0], "\n") {
		idx := strings.Index(line, ": ")
		if idx < 0 {
			continue
		}
		fields[line[:idx]] = line[idx+2:]
	}
	c := Chunk{
		Ref:      fields["ref"],
		Doc:      fields["doc"],
		Slug:     fields["slug"],
		Title:    fields["title"],
		Category: fields["category"],
		Source:   fields["source"],
		Body:     strings.TrimRight(parts[1], "\n"),
	}
	if c.Ref == "" || c.Body == "" {
		return Chunk{}, fmt.Errorf("missing ref or body")
	}
	return c, nil
}

// KBLookup resolves one KBRef to its chunk — the finding-card expansion
// and GET /api/v1/smith/kb/{ref}.
func (s *Smith) KBLookup(ref string) (Chunk, bool) {
	c, ok := kbChunksByRef[ref]
	return c, ok
}

// kbSearchLimitDefault/Max bound KBSearch's returned result count — same
// posture as findings/limit query params elsewhere in this package.
const (
	kbSearchLimitDefault = 10
	kbSearchLimitMax     = 50
)

// KBSearch ranks the embedded corpus plus live-DB evidence against a
// keyword query. Store nil ⇒ only the corpus contributes. Never returns an
// error for a source-specific query failure — one noisy/unavailable table
// degrades that source to zero results, not the whole search.
func (s *Smith) KBSearch(ctx context.Context, q string, limit int) ([]KBResult, error) {
	if limit <= 0 {
		limit = kbSearchLimitDefault
	}
	if limit > kbSearchLimitMax {
		limit = kbSearchLimitMax
	}
	tokens := tokenizeKBQuery(q)
	if len(tokens) == 0 {
		return []KBResult{}, nil
	}

	var results []KBResult
	results = append(results, s.searchCorpus(tokens)...)
	results = append(results, s.kbSearchNotifications(ctx, tokens)...)
	results = append(results, s.kbSearchModeHistory(ctx, tokens)...)
	results = append(results, s.kbSearchAuditLog(ctx, tokens)...)
	results = append(results, s.kbSearchModelProfiles(ctx, tokens)...)
	results = append(results, s.kbSearchFindings(ctx, tokens)...)

	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

var kbTokenRe = regexp.MustCompile(`[a-z0-9._-]+`)

func tokenizeKBQuery(q string) []string {
	tokens := kbTokenRe.FindAllString(strings.ToLower(q), -1)
	out := tokens[:0]
	for _, t := range tokens {
		if len(t) >= 2 {
			out = append(out, t)
		}
	}
	return out
}

func (s *Smith) searchCorpus(tokens []string) []KBResult {
	var out []KBResult
	for _, c := range kbChunks {
		score := scoreChunk(tokens, c)
		if score <= 0 {
			continue
		}
		out = append(out, KBResult{Kind: "doc", Ref: c.Ref, Title: c.Title, Body: c.Body, Score: score})
	}
	return out
}

// bodyTermCap bounds how much a single token's repeat count inside a body
// can contribute (bodyTermScore below) — raw linear term-frequency was
// found live (P4 verification, a real smith chat grounding check) to let
// the single longest, most verbose chunk in the corpus (pitfalls:gtt-
// ceiling, ~7.6 KB of accumulated incident history) out-count a much more
// topically relevant chunk on common administrative vocabulary
// ("context", "mode", "configured") appearing 40+ times, purely by being
// long — the log-length normalization below dampens that by barely more
// than 1x for a ~10x length difference, nowhere near enough. Capping
// per-token contribution makes "uses the right vocabulary a normal amount"
// beat "happens to be long," without discarding term frequency entirely
// (a term appearing 2-3 times is still a stronger signal than once).
const bodyTermCap = 4

func bodyTermScore(body, token string) float64 {
	if c := strings.Count(body, token); c > 0 {
		return math.Min(float64(c), bodyTermCap)
	}
	return 0
}

// scoreChunk favors an exact/partial slug match (the strongest possible
// signal — a slug is a hand-picked identifier, not incidental text), then
// title matches, then capped body term frequency, normalized by body
// length so a long chunk doesn't win purely on repetition.
func scoreChunk(tokens []string, c Chunk) float64 {
	lt, lb, ls := strings.ToLower(c.Title), strings.ToLower(c.Body), strings.ToLower(c.Slug)
	var score float64
	for _, t := range tokens {
		if t == ls {
			score += 12
		} else if strings.Contains(ls, t) {
			score += 6
		}
		if strings.Contains(lt, t) {
			score += 3
		}
		score += bodyTermScore(lb, t)
	}
	if score == 0 {
		return 0
	}
	return score / math.Log(float64(len(c.Body))+10)
}

func scoreText(tokens []string, title, body string) float64 {
	lt, lb := strings.ToLower(title), strings.ToLower(body)
	var score float64
	for _, t := range tokens {
		if strings.Contains(lt, t) {
			score += 3
		}
		score += bodyTermScore(lb, t)
	}
	if score == 0 {
		return 0
	}
	return score / math.Log(float64(len(body))+10)
}

// dbEvidenceWindow bounds how many recent rows per table are pulled before
// scoring — scoring happens in Go (not SQL LIKE) so a variable token count
// never needs dynamic query building. dbEvidencePerSourceCap keeps one
// noisy table (e.g. a chatty audit log) from crowding out the other four
// sources in the merged result set.
const (
	dbEvidenceWindow       = 200
	dbEvidencePerSourceCap = 5
)

func topN(rs []KBResult, n int) []KBResult {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Score > rs[j].Score })
	if len(rs) > n {
		rs = rs[:n]
	}
	return rs
}

func (s *Smith) kbSearchNotifications(ctx context.Context, tokens []string) []KBResult {
	if s.d.Store == nil {
		return nil
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT code, subject, message, last_seen FROM notifications ORDER BY last_seen DESC LIMIT ?`,
		dbEvidenceWindow)
	if err != nil {
		s.logf("kb: notifications search: %v", err)
		return nil
	}
	defer rows.Close()

	var out []KBResult
	for rows.Next() {
		var code, subject, message string
		var lastSeen int64
		if err := rows.Scan(&code, &subject, &message, &lastSeen); err != nil {
			continue
		}
		title := code
		if subject != "" {
			title = code + " (" + subject + ")"
		}
		body := scrubSecretPatterns(message)
		score := scoreText(tokens, title, body)
		if score <= 0 {
			continue
		}
		ts := lastSeen
		out = append(out, KBResult{Kind: "notification", Ref: "notification:" + code + ":" + subject,
			Title: title, Body: body, Score: score, TS: &ts})
	}
	return topN(out, dbEvidencePerSourceCap)
}

func (s *Smith) kbSearchModeHistory(ctx context.Context, tokens []string) []KBResult {
	if s.d.Store == nil {
		return nil
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, mode, ts, configured_ctx, actual_ctx, load_time_s, result
		 FROM mode_history ORDER BY ts DESC LIMIT ?`, dbEvidenceWindow)
	if err != nil {
		s.logf("kb: mode_history search: %v", err)
		return nil
	}
	defer rows.Close()

	var out []KBResult
	for rows.Next() {
		var id, ts int64
		var mode, result string
		var configuredCtx, actualCtx sql.NullInt64
		var loadTimeS sql.NullFloat64
		if err := rows.Scan(&id, &mode, &ts, &configuredCtx, &actualCtx, &loadTimeS, &result); err != nil {
			continue
		}
		title := fmt.Sprintf("%s load: %s", mode, result)
		body := fmt.Sprintf("mode=%s result=%s configured_ctx=%d actual_ctx=%d load_time_s=%.1f",
			mode, result, configuredCtx.Int64, actualCtx.Int64, loadTimeS.Float64)
		score := scoreText(tokens, title, body)
		if score <= 0 {
			continue
		}
		out = append(out, KBResult{Kind: "mode_history", Ref: fmt.Sprintf("mode_history:%d", id),
			Title: title, Body: body, Score: score, TS: &ts})
	}
	return topN(out, dbEvidencePerSourceCap)
}

func (s *Smith) kbSearchAuditLog(ctx context.Context, tokens []string) []KBResult {
	if s.d.Store == nil {
		return nil
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, ts, actor, action, target, detail FROM audit_log ORDER BY ts DESC LIMIT ?`,
		dbEvidenceWindow)
	if err != nil {
		s.logf("kb: audit_log search: %v", err)
		return nil
	}
	defer rows.Close()

	var out []KBResult
	for rows.Next() {
		var id, ts int64
		var actor, action string
		var target, detail sql.NullString
		if err := rows.Scan(&id, &ts, &actor, &action, &target, &detail); err != nil {
			continue
		}
		title := action
		if target.String != "" {
			title = action + " on " + target.String
		}
		body := scrubSecretPatterns(actor + ": " + title)
		if detail.String != "" {
			ev := redactValue(evidenceFromJSON(detail.String))
			body += fmt.Sprintf(" (%v)", ev)
		}
		score := scoreText(tokens, title, body)
		if score <= 0 {
			continue
		}
		out = append(out, KBResult{Kind: "audit", Ref: fmt.Sprintf("audit:%d", id),
			Title: title, Body: body, Score: score, TS: &ts})
	}
	return topN(out, dbEvidencePerSourceCap)
}

func (s *Smith) kbSearchModelProfiles(ctx context.Context, tokens []string) []KBResult {
	if s.d.Store == nil {
		return nil
	}
	// model_profiles is keyed by config_id since the 0042 surrogate-key
	// migration — join configs for the human-readable mode name this
	// search result's title/body need.
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT c.name, mp.backend, mp.parallel, mp.n_ctx, mp.actual_n_ctx,
		        mp.safe_memory_bytes, mp.prefill_tps, mp.decode_tps, mp.measured_at
		 FROM model_profiles mp JOIN configs c ON c.id = mp.config_id
		 ORDER BY mp.measured_at DESC LIMIT ?`, dbEvidenceWindow)
	if err != nil {
		s.logf("kb: model_profiles search: %v", err)
		return nil
	}
	defer rows.Close()

	var out []KBResult
	for rows.Next() {
		var mode, backend string
		var parallel, nCtx, actualNCtx int
		var safeMemoryBytes int64
		var prefillTPS, decodeTPS float64
		var measuredAt int64
		if err := rows.Scan(&mode, &backend, &parallel, &nCtx, &actualNCtx, &safeMemoryBytes, &prefillTPS, &decodeTPS, &measuredAt); err != nil {
			continue
		}
		title := mode + " profile"
		body := fmt.Sprintf("mode=%s backend=%s parallel=%d n_ctx=%d actual_n_ctx=%d safe_memory_mb=%.0f prefill_tps=%.1f decode_tps=%.1f",
			mode, backend, parallel, nCtx, actualNCtx, float64(safeMemoryBytes)/(1<<20), prefillTPS, decodeTPS)
		score := scoreText(tokens, title, body)
		if score <= 0 {
			continue
		}
		ts := measuredAt
		out = append(out, KBResult{Kind: "profile", Ref: "profile:" + mode, Title: title, Body: body, Score: score, TS: &ts})
	}
	return topN(out, dbEvidencePerSourceCap)
}

func (s *Smith) kbSearchFindings(ctx context.Context, tokens []string) []KBResult {
	if s.d.Store == nil {
		return nil
	}
	rows, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT id, check_id, severity, summary, evidence, created_at
		 FROM smith_findings ORDER BY created_at DESC LIMIT ?`, dbEvidenceWindow)
	if err != nil {
		s.logf("kb: smith_findings search: %v", err)
		return nil
	}
	defer rows.Close()

	var out []KBResult
	for rows.Next() {
		var id, createdAt int64
		var checkID, severity, summary, evidence string
		if err := rows.Scan(&id, &checkID, &severity, &summary, &evidence, &createdAt); err != nil {
			continue
		}
		title := fmt.Sprintf("%s (%s)", checkID, severity)
		body := scrubSecretPatterns(summary)
		if evidence != "" && evidence != "{}" {
			ev := redactValue(evidenceFromJSON(evidence))
			body += fmt.Sprintf(" (%v)", ev)
		}
		score := scoreText(tokens, title, body)
		if score <= 0 {
			continue
		}
		ts := createdAt
		out = append(out, KBResult{Kind: "finding", Ref: fmt.Sprintf("finding:%d", id),
			Title: title, Body: body, Score: score, TS: &ts})
	}
	return topN(out, dbEvidencePerSourceCap)
}

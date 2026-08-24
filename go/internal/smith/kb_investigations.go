// SPDX-License-Identifier: Apache-2.0

package smith

// kb_investigations.go parses the externally-blocked-work tracker into
// structured items (docs/v5-smith.md §4.7 "Externally-blocked work").
//
// TWO-LAYER KNOWLEDGE ARCHITECTURE (operator directive 2026-08-21): the
// PARSER and the item format below are product knowledge and ship; the
// tracker's CONTENT is layer-2 deployment data — an operator-maintained
// markdown file local to each install, read live from
// Deps.BlockedWorkPath, never embedded and never shipped. This used to be
// a `//go:embed kb/investigations.md` copy of docs/investigations.md
// (synced by kbsync); that shipped one deployment's live work log inside
// every binary, which is exactly what the two-layer split forbids.
//
// Consequences of the seam being live and per-install:
//
//   - No path / no file ⇒ zero items. A fresh install has no blocked work
//     yet — an honest empty state, not an error.
//   - An unparseable file logs and yields zero items rather than taking
//     the daemon down: the file is operator-edited local data, so a
//     malformed edit must degrade gracefully, not panic at init like the
//     embedded version did.
//   - ListBlockedItems re-reads on every call, so the operator edits the
//     file and the change is live immediately (same posture as the
//     settings-backed seams).
//
// The file is hand-written prose accumulated over time, not a generated
// format — field labels are consistent in name but not in exact
// punctuation (see the format notes on each regex below), so this parser
// is deliberately tolerant of variation rather than assuming a single
// template. kb_investigations_test.go pins the parser against the shipped
// synthetic example (testdata/blocked-work-example.md).

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// BlockedItem is one blocked-work tracker entry. Status is derived
// (open|resolved|closed); StatusText is the item's own "Status:" prose.
// URLs is extracted from WhereCheck now and unused now — P5 (web research)
// is what fetches them to test whether an item unblocked.
type BlockedItem struct {
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	StatusText  string   `json:"status_text"`
	BlockedOn   string   `json:"blocked_on"`
	WhereCheck  string   `json:"where_to_check"`
	WhenUnblock string   `json:"when_unblocked"`
	LastChecked string   `json:"last_checked"` // YYYY-MM-DD, "" if never
	URLs        []string `json:"urls"`
}

// ListBlockedItems reads and parses the operator's blocked-work file
// (Deps.BlockedWorkPath) live, file order preserved (which is NOT numeric
// order — items are numbered by when they were opened, not reordered as
// older ones close). Empty path, absent file, or unreadable content all
// yield nil: a fresh install has no blocked work yet.
func (s *Smith) ListBlockedItems() []BlockedItem {
	path := s.d.BlockedWorkPath
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	items, err := parseBlockedItems(string(raw))
	if err != nil {
		s.logf("blocked-work file %s unparseable, treating as empty: %v", path, err)
		return nil
	}
	return items
}

var (
	itemHeadingRe = regexp.MustCompile(`^##\s+(\d+)\.\s+(.*)$`)
	statusARe     = regexp.MustCompile(`\*\*Status\b[^*]*:\*\*\s*`)
	statusBRe     = regexp.MustCompile(`\*\*Status:\s*([^*]+)\*\*`)
	blockedOnRe   = regexp.MustCompile(`\*\*Blocked on[^*]*:\*\*`)
	whereCheckRe  = regexp.MustCompile(`\*\*Where to check:\*\*`)
	whenUnblockRe = regexp.MustCompile(`\*\*When unblocked[^*]*:\*\*`)
	checkedDateRe = regexp.MustCompile(`\*\*(?:Also c|C)hecked (\d{4}-\d{2}-\d{2})`)
	urlInBodyRe   = regexp.MustCompile(`https?://[^\s)\]]+`)
)

// parseBlockedItems splits the raw file on its "---" item separators (one
// precedes every item, including the first — the preamble/"ForgeHost Hardware
// Notes" section before it is discarded) and parses each item body
// independently.
func parseBlockedItems(raw string) ([]BlockedItem, error) {
	segments := strings.Split(raw, "\n---\n")
	var items []BlockedItem
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" || !strings.HasPrefix(seg, "##") {
			continue // the preamble segment (and any trailing empty split)
		}
		item, err := parseBlockedItem(seg)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// fieldMatch is one recognized field-label occurrence within an item body,
// used to bound each field's value to "everything up to the next
// recognized label" — the values themselves (a Blocked-on paragraph, a
// bulleted Where-to-check list) vary in length and internal structure far
// too much to capture with a single blank-line-terminated regex.
type fieldMatch struct {
	kind       string // blocked_on | where | when | checked
	start, end int
	date       string // only for kind == checked
}

func scanFields(body string) []fieldMatch {
	var out []fieldMatch
	for _, m := range blockedOnRe.FindAllStringIndex(body, -1) {
		out = append(out, fieldMatch{kind: "blocked_on", start: m[0], end: m[1]})
	}
	for _, m := range whereCheckRe.FindAllStringIndex(body, -1) {
		out = append(out, fieldMatch{kind: "where", start: m[0], end: m[1]})
	}
	for _, m := range whenUnblockRe.FindAllStringIndex(body, -1) {
		out = append(out, fieldMatch{kind: "when", start: m[0], end: m[1]})
	}
	for _, m := range checkedDateRe.FindAllStringSubmatchIndex(body, -1) {
		out = append(out, fieldMatch{kind: "checked", start: m[0], end: m[1], date: body[m[2]:m[3]]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func parseBlockedItem(seg string) (BlockedItem, error) {
	lines := strings.SplitN(seg, "\n", 2)
	m := itemHeadingRe.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return BlockedItem{}, fmt.Errorf("item heading %q doesn't match '## N. Title'", firstLine(seg))
	}
	item := BlockedItem{Number: atoiOrZero(m[1]), Title: strings.TrimSpace(m[2])}

	body := seg
	fields := scanFields(body)

	// Status: two shapes seen in the live file — "**Status:** <text until
	// the next field>" (the common case), and "**Status: <text>.**" (the
	// closed/narrative items, e.g. item 11/12 — value wrapped entirely
	// inside the bold span, self-terminating at "**", no boundary scan
	// needed).
	if loc := statusARe.FindStringIndex(body); loc != nil {
		end := len(body)
		for _, f := range fields {
			if f.start > loc[1] {
				end = f.start
				break
			}
		}
		item.StatusText = strings.TrimSpace(body[loc[1]:end])
	} else if sm := statusBRe.FindStringSubmatch(body); sm != nil {
		item.StatusText = strings.TrimSpace(sm[1])
	}
	item.Status = deriveBlockedStatus(item.Title, item.StatusText)

	for i, f := range fields {
		end := len(body)
		if i+1 < len(fields) {
			end = fields[i+1].start
		}
		switch f.kind {
		case "blocked_on":
			if item.BlockedOn == "" {
				item.BlockedOn = strings.TrimSpace(body[f.end:end])
			}
		case "where":
			if item.WhereCheck == "" {
				item.WhereCheck = strings.TrimSpace(body[f.end:end])
			}
		case "when":
			if item.WhenUnblock == "" {
				item.WhenUnblock = strings.TrimSpace(body[f.end:end])
			}
		case "checked":
			if f.date > item.LastChecked {
				item.LastChecked = f.date
			}
		}
	}

	item.URLs = urlInBodyRe.FindAllString(item.WhereCheck, -1)

	if item.Title == "" || item.Status == "" {
		return BlockedItem{}, fmt.Errorf("item %d: missing title or status", item.Number)
	}
	return item, nil
}

var (
	closedInTitleRe   = regexp.MustCompile(`(?i)\bclosed\b`)
	resolvedInTitleRe = regexp.MustCompile(`(?i)\bresolved\b`)
)

// deriveBlockedStatus classifies an item from its own title + Status
// prose only — never the whole item body, so a later "Resolved for Gemma4"
// sub-note deep in a still-open item's history doesn't misclassify the
// item itself (item 9 is the live example: still open overall, with one
// architecture-specific sub-case resolved along the way).
//
// statusText uses a startswith check, not "contains anywhere": a long
// Status paragraph can legitimately mention "closed" mid-sentence about
// something else entirely (item 8/IsoQuant's Status prose references a
// "closed PR #21722" while the item itself is very much still open) — the
// two real closed items (11, 12) both use the "**Status: closed
// DATE[, ...].**" shape (docs/v5-smith.md §4.7's format notes), so their
// captured StatusText always starts with the word itself.
func deriveBlockedStatus(title, statusText string) string {
	trimmed := strings.ToLower(strings.TrimSpace(statusText))
	switch {
	case closedInTitleRe.MatchString(title), strings.HasPrefix(trimmed, "closed"):
		return "closed"
	case resolvedInTitleRe.MatchString(title), strings.HasPrefix(trimmed, "resolved"):
		return "resolved"
	default:
		return "open"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func atoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

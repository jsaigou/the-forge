// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"os"
	"path/filepath"
	"testing"
)

// blockedWorkExampleSmith wires the blocked-work seam at the synthetic
// example tracker (testdata/blocked-work-example.md) — the shipped shape a
// real deployment's operator-local file uses. Real trackers are layer-2
// deployment data (Deps.BlockedWorkPath); nothing about their CONTENT is
// embedded or pinned here, only the parser's behavior against the format.
func blockedWorkExampleSmith(t *testing.T) *Smith {
	t.Helper()
	return New(Deps{BlockedWorkPath: filepath.Join("testdata", "blocked-work-example.md"), Logf: func(string, ...any) {}})
}

// TestParseBlockedItems_AllParse asserts every example item parses with a
// non-empty title/status and a unique, positive number — the P4 exit
// criterion ("externally blocked work list renders") mechanized: a parser
// change that breaks the field-label tolerance fails here instead of
// silently dropping an item from GET /api/v1/smith/kb/blocked.
func TestParseBlockedItems_AllParse(t *testing.T) {
	items := blockedWorkExampleSmith(t).ListBlockedItems()
	if len(items) == 0 {
		t.Fatal("expected at least one parsed item from the example tracker")
	}

	seenNumbers := map[int]bool{}
	for _, it := range items {
		if it.Number <= 0 {
			t.Errorf("item %+v: non-positive number", it)
		}
		if seenNumbers[it.Number] {
			t.Errorf("duplicate item number %d", it.Number)
		}
		seenNumbers[it.Number] = true

		if it.Title == "" {
			t.Errorf("item %d: empty title", it.Number)
		}
		switch it.Status {
		case "open", "resolved", "closed":
		default:
			t.Errorf("item %d: unrecognized derived status %q", it.Number, it.Status)
		}
	}
}

// TestParseBlockedItems_StatusShapes pins the parser's three status shapes
// against the false positive it specifically had to be fixed for: a
// "closed" substring appearing mid-sentence in an otherwise-open item's
// Status prose must never flip that item's derived status (item 3 mentions
// a closed upstream issue while staying open; only item 2 — the
// "**Status: closed DATE...**" shape — derives closed).
func TestParseBlockedItems_StatusShapes(t *testing.T) {
	items := blockedWorkExampleSmith(t).ListBlockedItems()
	byNumber := map[int]BlockedItem{}
	for _, it := range items {
		byNumber[it.Number] = it
	}

	if it, ok := byNumber[2]; !ok {
		t.Fatal("example item 2 missing")
	} else if it.Status != "closed" {
		t.Errorf("item 2 (%s): expected status closed (the '**Status: closed DATE...**' shape), got %q (statusText=%q)", it.Title, it.Status, it.StatusText)
	}

	if it, ok := byNumber[3]; !ok {
		t.Fatal("example item 3 missing")
	} else if it.Status == "closed" {
		t.Errorf("item 3 (%s) misclassified as closed — its Status prose mentions a closed upstream issue mid-sentence, not the item itself; statusText=%q", it.Title, it.StatusText)
	}

	if it, ok := byNumber[1]; !ok {
		t.Fatal("example item 1 missing")
	} else {
		if it.Status != "open" {
			t.Errorf("item 1 (%s): expected status open, got %q", it.Title, it.Status)
		}
		if it.LastChecked != "2026-02-01" {
			t.Errorf("item 1: LastChecked = %q, want 2026-02-01 (the '**Checked DATE**' shape)", it.LastChecked)
		}
		if len(it.URLs) != 1 {
			t.Errorf("item 1: URLs = %v, want the WhereCheck link extracted", it.URLs)
		}
	}
}

// TestListBlockedItems_EmptyByDefault pins the fresh-install posture: no
// path wired, or a path with no file, reads honestly empty — never an
// error, never fabricated items.
func TestListBlockedItems_EmptyByDefault(t *testing.T) {
	if items := New(Deps{}).ListBlockedItems(); len(items) != 0 {
		t.Errorf("no BlockedWorkPath wired: got %d items, want 0", len(items))
	}
	s := New(Deps{BlockedWorkPath: filepath.Join(t.TempDir(), "does-not-exist.md")})
	if items := s.ListBlockedItems(); len(items) != 0 {
		t.Errorf("absent tracker file: got %d items, want 0", len(items))
	}
}

// TestListBlockedItems_MalformedFileDegradesGracefully pins the seam's
// runtime posture: the tracker is operator-edited local data, so a
// malformed edit logs and yields zero items instead of taking the daemon
// down (the embedded version panicked at init on this class of error).
func TestListBlockedItems_MalformedFileDegradesGracefully(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tracker.md")
	if err := os.WriteFile(path, []byte("## not-a-numbered-item heading\nno fields at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{BlockedWorkPath: path})
	if items := s.ListBlockedItems(); len(items) != 0 {
		t.Errorf("malformed tracker: got %d items, want 0 (graceful degradation)", len(items))
	}
}

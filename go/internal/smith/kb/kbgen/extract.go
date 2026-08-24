// SPDX-License-Identifier: Apache-2.0

// Package kbgen extracts smith's curated knowledge-base corpus (docs/v5-smith.md
// §4.7) from the repo's own documentation files into go/internal/smith/kb's
// embedded corpus. It is used two ways: cmd/kbsync writes the extraction to
// disk (invoked via `go generate` from internal/smith/kb.go, or directly with
// `go run ./cmd/kbsync` from the go/ module root), and internal/smith's
// kb_sync_test.go re-runs the same extraction against the live repo docs and
// diffs it against what's committed — a doc edited without re-running
// kbsync fails the build instead of silently going stale.
//
// go:embed cannot reach outside its own package directory, which is why the
// corpus is a build-time copy rather than a live read of docs/ (the daemon
// binary also has to run on the deployment host, where the docs/ tree isn't necessarily
// present at the embedded path).
package kbgen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ManifestChunk is one entry in kb/manifest.json — the curation list. Slugs
// are explicit and stable (checks.go's KBRefs name them directly; a doc
// reword must never break a ref already shipped in code).
type ManifestChunk struct {
	Slug     string `json:"slug"`
	Doc      string `json:"doc"`
	Source   string `json:"source"` // path relative to the repo root
	Category string `json:"category"`
	Mode     string `json:"mode"`   // heading | line | paragraph
	Anchor   string `json:"anchor"` // heading text (mode=heading) or a substring (mode=line/paragraph)
	// Title overrides the extracted title. Required for mode=line/paragraph
	// (there's no heading to derive one from); optional for mode=heading
	// (defaults to the heading text itself).
	Title string `json:"title"`
}

// Manifest is the top-level kb/manifest.json shape.
type Manifest struct {
	Chunks []ManifestChunk `json:"chunks"`
}

// LoadManifest reads and parses manifest.json from the given path.
func LoadManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("kbgen: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("kbgen: parse manifest: %w", err)
	}
	return m, nil
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

func isHeadingLine(l string) bool {
	return headingRe.MatchString(l)
}

// extractHeading captures from the line matching `anchor` (exact text after
// stripping the leading `#`s) to the next heading of the same or shallower
// level, or EOF. The heading line itself is kept in body — it carries
// section context that's worth keeping when the chunk is read standalone.
func extractHeading(lines []string, anchor string) (body string, err error) {
	start, level := -1, 0
	for i, l := range lines {
		m := headingRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if strings.TrimSpace(m[2]) == anchor {
			start, level = i, len(m[1])
			break
		}
	}
	if start == -1 {
		return "", fmt.Errorf("heading not found: %q", anchor)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if m := headingRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) <= level {
			end = i
			break
		}
	}
	return strings.TrimRight(strings.Join(lines[start:end], "\n"), " \t\n"), nil
}

// extractLine captures the single physical line containing `anchor` as a
// substring. Built for pitfalls.md, whose bullets are each one (long)
// physical line with no soft-wrapping — confirmed against the live file
// before relying on this.
func extractLine(lines []string, anchor string) (body string, err error) {
	for _, l := range lines {
		if strings.Contains(l, anchor) {
			return strings.TrimSpace(l), nil
		}
	}
	return "", fmt.Errorf("anchor line not found: %q", anchor)
}

// extractParagraph captures the blank-line-delimited block containing
// `anchor` — for docs whose prose wraps a topic across multiple lines
// separated by blank lines from its neighbors (unlike pitfalls.md's
// one-line bullets). Stops at a heading line even without an intervening
// blank line, so a paragraph never bleeds into the next section.
func extractParagraph(lines []string, anchor string) (body string, err error) {
	idx := -1
	for i, l := range lines {
		if strings.Contains(l, anchor) {
			idx = i
			break
		}
	}
	if idx == -1 {
		return "", fmt.Errorf("anchor not found: %q", anchor)
	}
	start := idx
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" && !isHeadingLine(lines[start-1]) {
		start--
	}
	end := idx
	for end < len(lines)-1 && strings.TrimSpace(lines[end+1]) != "" && !isHeadingLine(lines[end+1]) {
		end++
	}
	return strings.TrimSpace(strings.Join(lines[start:end+1], "\n")), nil
}

// extractChunk resolves one manifest entry against the repo's real doc
// content. cache memoizes source files (split into lines) across chunks
// that share a source doc.
func extractChunk(repoRoot string, c ManifestChunk, cache map[string][]string) (title, body string, err error) {
	srcPath := filepath.Join(repoRoot, filepath.FromSlash(c.Source))
	lines, ok := cache[srcPath]
	if !ok {
		raw, err := os.ReadFile(srcPath)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", c.Source, err)
		}
		lines = strings.Split(string(raw), "\n")
		cache[srcPath] = lines
	}

	switch c.Mode {
	case "heading":
		body, err = extractHeading(lines, c.Anchor)
		title = c.Anchor
	case "line":
		body, err = extractLine(lines, c.Anchor)
	case "paragraph":
		body, err = extractParagraph(lines, c.Anchor)
	default:
		return "", "", fmt.Errorf("unknown mode %q", c.Mode)
	}
	if err != nil {
		return "", "", err
	}
	if c.Title != "" {
		title = c.Title
	}
	return title, body, nil
}

// renderChunkFile is the on-disk format for one corpus/*.md file: a small
// plain key:value header (not YAML frontmatter — no parser dependency
// needed on either side), a blank line, then the extracted body verbatim.
func renderChunkFile(c ManifestChunk, title, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ref: %s:%s\n", c.Doc, c.Slug)
	fmt.Fprintf(&b, "doc: %s\n", c.Doc)
	fmt.Fprintf(&b, "slug: %s\n", c.Slug)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "category: %s\n", c.Category)
	fmt.Fprintf(&b, "source: %s\n", c.Source)
	b.WriteString("\n")
	b.WriteString(body)
	b.WriteString("\n")
	return b.String()
}

// Generate re-runs the whole manifest against the repo's real doc content
// and returns every output file kb/ should contain, keyed by its path
// relative to go/internal/smith/kb/ ("corpus/<doc>-<slug>.md" for each
// manifest entry). It performs no I/O against go/internal/smith/kb itself,
// which is what lets kb_sync_test.go use it as a pure comparison against
// the committed corpus. The blocked-work tracker is deliberately NOT part
// of the generated set: it is layer-2 deployment data, read live from an
// operator-local file (Deps.BlockedWorkPath), never shipped
// (two-layer knowledge architecture, 2026-08-21).
func Generate(repoRoot string) (map[string]string, error) {
	manifestPath := filepath.Join(repoRoot, "go", "internal", "smith", "kb", "manifest.json")
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	files := map[string]string{}
	cache := map[string][]string{}
	seen := map[string]bool{}
	for _, c := range m.Chunks {
		ref := c.Doc + ":" + c.Slug
		if seen[ref] {
			return nil, fmt.Errorf("kbgen: duplicate ref %q in manifest", ref)
		}
		seen[ref] = true
		if (c.Mode == "line" || c.Mode == "paragraph") && c.Title == "" {
			return nil, fmt.Errorf("kbgen: %s: mode %q requires an explicit title (no heading to derive one from)", ref, c.Mode)
		}

		title, body, err := extractChunk(repoRoot, c, cache)
		if err != nil {
			return nil, fmt.Errorf("kbgen: %s: %w", ref, err)
		}
		files["corpus/"+c.Doc+"-"+c.Slug+".md"] = renderChunkFile(c, title, body)
	}

	return files, nil
}

// Sync writes Generate's output to go/internal/smith/kb/, replacing the
// corpus/ directory wholesale (so a manifest entry that's renamed or
// removed doesn't leave an orphaned stale file behind).
func Sync(repoRoot string) (int, error) {
	files, err := Generate(repoRoot)
	if err != nil {
		return 0, err
	}
	kbDir := filepath.Join(repoRoot, "go", "internal", "smith", "kb")
	corpusDir := filepath.Join(kbDir, "corpus")
	if err := os.RemoveAll(corpusDir); err != nil {
		return 0, fmt.Errorf("kbgen: clean corpus dir: %w", err)
	}
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		return 0, fmt.Errorf("kbgen: create corpus dir: %w", err)
	}
	for name, content := range files {
		path := filepath.Join(kbDir, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return 0, fmt.Errorf("kbgen: write %s: %w", name, err)
		}
	}
	return len(files), nil
}

// SPDX-License-Identifier: Apache-2.0

// Package hf is a small client for the public HuggingFace Hub API — model
// search, a repo's full (recursive) file tree, and best-effort model-card
// metadata. Built for the HF model-acquisition track
// (docs/adding-a-model.md's manual `hf download` flow, automated).
//
// Deliberately separate from smith/web.Service (the general web-research
// fetcher smith.Evaluate already uses via Deps.Web.FetchDirect): that
// service is a caching, SSRF-guarded fetcher with no per-request auth
// header and a cache key that doesn't account for one, so an authenticated
// (gated-repo) response could be served back to an unauthenticated caller.
// HF is one well-known host; this package talks to it directly.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// DefaultBaseURL is the real HF Hub API root. A package-level var (not a
// const) so tests can point Client.BaseURL at an httptest server the same
// way smith/fetch_model_ops.go's fetchModelBaseURL does.
const DefaultBaseURL = "https://huggingface.co"

// ErrGated is returned when HF answers 401/403 — the repo is gated
// (license click-through) or private, and either no token was sent or the
// token isn't authorized for it. Callers surface this as "needs a token /
// needs terms accepted" rather than a bare transport error.
var ErrGated = errors.New("hf: repository is gated or private — a valid access token is required")

// ErrNotFound is returned when HF answers 404 for a repo lookup.
var ErrNotFound = errors.New("hf: repository not found")

// Model is one HF search result.
type Model struct {
	ID           string   `json:"id"`       // "org/name" or a bare name
	Author       string   `json:"author"`
	Downloads    int64    `json:"downloads"`
	Likes        int64    `json:"likes"`
	Tags         []string `json:"tags"`
	Gated        bool     `json:"-"` // decoded from the tolerant gatedValue below
	PipelineTag  string   `json:"pipeline_tag"`
	LastModified string   `json:"lastModified"`
	// NoGGUF marks a synthetic entry Search injects for the true publisher
	// of a model family when NONE of the real (gguf-filtered) results come
	// from that publisher — see addOfficialFallback. Operator feedback
	// (2026-08-26, the Ling-3.0 case): inclusionAI never published a GGUF
	// for Ling-3.0 at all, so every real result is a third-party
	// requantization; rather than silently omit the actual publisher,
	// Search surfaces their repo anyway with NoGGUF=true so the caller can
	// show "not compatible — no GGUF; download from HuggingFace directly"
	// instead of leaving the question of "is there an official one?"
	// unanswered.
	NoGGUF bool `json:"no_gguf,omitempty"`
}

// gatedValue tolerates HF's wire shape for "gated", which is either the
// JSON literal false or a non-empty string ("auto" | "manual").
type gatedValue bool

func (g *gatedValue) UnmarshalJSON(b []byte) error {
	var asBool bool
	if err := json.Unmarshal(b, &asBool); err == nil {
		*g = gatedValue(asBool)
		return nil
	}
	var asString string
	if err := json.Unmarshal(b, &asString); err != nil {
		return fmt.Errorf("hf: gated field is neither bool nor string: %s", string(b))
	}
	*g = gatedValue(asString != "")
	return nil
}

// modelWire is the raw HF search-result shape; Search projects it into
// Model with Gated normalized.
type modelWire struct {
	ID           string     `json:"id"`
	Author       string     `json:"author"`
	Downloads    int64      `json:"downloads"`
	Likes        int64      `json:"likes"`
	Tags         []string   `json:"tags"`
	Gated        gatedValue `json:"gated"`
	PipelineTag  string     `json:"pipeline_tag"`
	LastModified string     `json:"lastModified"`
}

// File is one entry in a repo's file tree.
type File struct {
	Path      string
	SizeBytes int64
	IsDir     bool
}

// treeEntryWire mirrors GET /api/models/{repo}/tree/{rev}?recursive=1's
// per-entry shape.
type treeEntryWire struct {
	Type string `json:"type"` // "file" | "directory"
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Card is best-effort model-card metadata (Phase 4 asset enrichment).
// Every field is optional — a card with everything empty is not an error,
// since not every repo's README has parseable front matter.
type Card struct {
	Repo        string
	License     string
	BaseModel   string
	Description string
	Tags        []string
}

// modelCardWire is GET /api/models/{repo}'s shape — the subset Card reads.
// cardData mirrors the YAML front matter HF itself already parses out of
// README.md, sparing this package a YAML dependency.
type modelCardWire struct {
	Tags     []string `json:"tags"`
	CardData struct {
		License   string `json:"license"`
		BaseModel any    `json:"base_model"` // string or []string in the wild
	} `json:"cardData"`
	Description string `json:"description"`
}

// Query is one Search request.
type Query struct {
	Text  string
	Limit int // 0 ⇒ DefaultSearchLimit
}

// DefaultSearchLimit bounds an unset Query.Limit.
const DefaultSearchLimit = 20

// MaxSearchLimit is the hard cap regardless of what a caller (including
// smith's hf_search tool) requests.
const MaxSearchLimit = 50

// Client talks to the HF Hub API.
type Client struct {
	// HTTP is the transport used for every request. Callers on a
	// long-lived daemon must NOT hand this a client with a short blanket
	// Timeout (smith's Deps.HTTPClient defaults to 3s, sized for loopback
	// health probes) — construct one from just its Transport instead, the
	// same pattern fetch_model_ops.go already uses, and bound individual
	// calls via context.
	HTTP *http.Client
	// BaseURL defaults to DefaultBaseURL; overridden by tests.
	BaseURL string
	// Token returns the current HF access token, or "" for an
	// unauthenticated request. A function rather than a field so callers
	// can back it with live settings (mask-on-read, never cached in a
	// struct a debugger might dump). Never logged, never placed in an
	// error string — see doGet.
	Token func() string
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

// doGet issues an authenticated GET and classifies the response. The
// returned error NEVER includes response headers or the request URL's
// query string verbatim beyond the repo path already known to the caller
// — nothing here can leak a token even if a caller later wraps err in a
// user-facing message.
func (c *Client) doGet(ctx context.Context, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("hf: build request: %w", err)
	}
	if tok := c.Token; tok != nil {
		if t := tok(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("hf: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap — tree/card JSON, never a weight file
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("hf: read response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, resp.StatusCode, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, resp.StatusCode, ErrGated
	case http.StatusNotFound:
		return nil, resp.StatusCode, ErrNotFound
	default:
		return nil, resp.StatusCode, fmt.Errorf("hf: unexpected status %d", resp.StatusCode)
	}
}

// Search queries HF's model search, restricted to GGUF-tagged repos
// (filter=gguf) and sorted by downloads — the same defaults an operator
// browsing huggingface.co/models would see for "which GGUF repo is this".
//
// full=true is required — live-verified against the real API (2026-08-26):
// the default (non-full) search response omits author/gated/lastModified
// entirely (confirmed via a raw curl against huggingface.co, not assumed
// from docs), which would otherwise make every search result report a
// blank creator and Gated=false regardless of the repo's real state.
func (c *Client) Search(ctx context.Context, q Query) ([]Model, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	u := c.baseURL() + "/api/models?" + url.Values{
		"search": {q.Text},
		"filter": {"gguf"},
		"sort":   {"downloads"},
		"limit":  {strconv.Itoa(limit)},
		"full":   {"true"},
	}.Encode()

	body, _, err := c.doGet(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("hf: search: %w", err)
	}
	var wire []modelWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("hf: search: parse response: %w", err)
	}
	out := make([]Model, len(wire))
	for i, w := range wire {
		out[i] = Model{
			ID: w.ID, Author: w.Author, Downloads: w.Downloads, Likes: w.Likes,
			Tags: w.Tags, Gated: bool(w.Gated), PipelineTag: w.PipelineTag,
			LastModified: w.LastModified,
		}
	}
	return c.addOfficialFallback(ctx, out), nil
}

// baseModelTagRe extracts the "org/name" out of a base_model:org/name tag.
// baseModelFromTags skips base_model:quantized:org/name separately (that
// variant names who did the quantizing, not the original publisher) before
// this ever runs, so the pattern itself doesn't need to account for it.
var baseModelTagRe = regexp.MustCompile(`^base_model:([^/]+/[^/]+)$`)

func baseModelFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, "base_model:quantized:") {
			continue
		}
		if m := baseModelTagRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

// maxOfficialFallbackChecks bounds how many distinct base-model repos
// addOfficialFallback will fetch per search — a broad query can surface
// many distinct families, and each check is a real extra HTTP call.
const maxOfficialFallbackChecks = 3

// addOfficialFallback finds every distinct base_model a GGUF search result
// was quantized from, and — for any base model whose own org has no GGUF
// result among results already — fetches that base model's real repo info
// and prepends a NoGGUF=true entry for it. Best-effort: a fetch failure
// here is silently skipped, never surfaces as a Search error (the real
// GGUF results are the primary answer; this is a courtesy addition).
func (c *Client) addOfficialFallback(ctx context.Context, results []Model) []Model {
	officialOrgs := map[string]bool{}
	for _, m := range results {
		if m.Author != "" {
			officialOrgs[strings.ToLower(m.Author)] = true
		}
	}
	var baseModels []string
	seen := map[string]bool{}
	for _, m := range results {
		id := baseModelFromTags(m.Tags)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		baseModels = append(baseModels, id)
	}

	var extra []Model
	checks := 0
	for _, baseID := range baseModels {
		if checks >= maxOfficialFallbackChecks {
			break
		}
		org, _, ok := strings.Cut(baseID, "/")
		if !ok || officialOrgs[strings.ToLower(org)] {
			continue // already has a real GGUF from this publisher
		}
		checks++
		info, err := c.repoInfo(ctx, baseID)
		if err != nil {
			continue // best-effort — see doc comment
		}
		info.NoGGUF = true
		extra = append(extra, info)
	}
	return append(extra, results...)
}

// repoInfo fetches one repo's own metadata (unfiltered — works for a
// safetensors-only repo just as well as a GGUF one).
func (c *Client) repoInfo(ctx context.Context, repo string) (Model, error) {
	body, _, err := c.doGet(ctx, c.baseURL()+"/api/models/"+repo)
	if err != nil {
		return Model{}, err
	}
	var w modelWire
	if err := json.Unmarshal(body, &w); err != nil {
		return Model{}, err
	}
	author := w.Author
	if author == "" {
		author, _, _ = strings.Cut(w.ID, "/")
	}
	return Model{
		ID: w.ID, Author: author, Downloads: w.Downloads, Likes: w.Likes,
		Tags: w.Tags, Gated: bool(w.Gated), PipelineTag: w.PipelineTag,
		LastModified: w.LastModified,
	}, nil
}

// Tree returns repo's full file listing at revision (recursive — this is
// the fix over smith.Evaluate's root-of-main-only read, which can't see
// sharded GGUFs living under a quant subdirectory, e.g. Q4_K_M/model-00001-
// of-00003.gguf, exactly the layout docs/adding-a-model.md's "Sharded
// models" section documents).
func (c *Client) Tree(ctx context.Context, repo, revision string) ([]File, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return nil, errors.New("hf: tree requires a non-empty repo")
	}
	if revision == "" {
		revision = "main"
	}
	u := c.baseURL() + "/api/models/" + repo + "/tree/" + revision + "?recursive=1"
	body, _, err := c.doGet(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("hf: tree %q: %w", repo, err)
	}
	var wire []treeEntryWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("hf: tree %q: parse response: %w", repo, err)
	}
	out := make([]File, 0, len(wire))
	for _, e := range wire {
		out = append(out, File{Path: e.Path, SizeBytes: e.Size, IsDir: e.Type == "directory"})
	}
	return out, nil
}

// Card fetches best-effort model-card metadata for repo. Every field is
// optional; a Card with everything blank is a valid, non-error result —
// not every repo's README carries parseable front matter.
func (c *Client) Card(ctx context.Context, repo string) (Card, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return Card{}, errors.New("hf: card requires a non-empty repo")
	}
	u := c.baseURL() + "/api/models/" + repo
	body, _, err := c.doGet(ctx, u)
	if err != nil {
		return Card{}, fmt.Errorf("hf: card %q: %w", repo, err)
	}
	var wire modelCardWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return Card{}, fmt.Errorf("hf: card %q: parse response: %w", repo, err)
	}
	card := Card{Repo: repo, License: wire.CardData.License, Tags: wire.Tags, Description: wire.Description}
	switch bm := wire.CardData.BaseModel.(type) {
	case string:
		card.BaseModel = bm
	case []any:
		if len(bm) > 0 {
			if s, ok := bm[0].(string); ok {
				card.BaseModel = s
			}
		}
	}
	return card, nil
}

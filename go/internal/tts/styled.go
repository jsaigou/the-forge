// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jsaigou/the-forge/internal/tts/style"
)

// Inline style tags (docs/zonos-style-tags-plan.md). The grammar and the plan live in package style; this file is the seam: it takes tags out of a request's input
// before any engine sees it, so no engine ever reads "<happy>" aloud, and it carries the parsed text to the engine that can act on it.

// styledInput is a request's text with its tags taken out, and the defaults the client sent with it.
type styledInput struct {
	parsed   *style.Parsed
	defaults style.Defaults
}

// Response headers that say what the style handling did.
const (
	headerStyleApplied  = "X-Style-Applied"
	headerStyleWarnings = "X-Style-Warnings"
)

// applyTags parses req.Input for style tags. The returned request carries the spoken text and the parsed form; when the request has no tags and no style defaults
// it is returned unchanged. A client that does not want tags read sends "tags": false.
func applyTags(req SpeechRequest) SpeechRequest {
	if req.Tags != nil && !*req.Tags {
		return req
	}
	hasDefaults := req.Style != nil
	if !style.LooksTagged(req.Input) && !hasDefaults {
		return req
	}
	v := style.Default()
	p := style.Parse(req.Input, v)
	if !p.HasTags() && !hasDefaults && len(p.Warnings) == 0 {
		return req // "a < b": a bracket that is not a tag
	}
	var d style.Defaults
	if req.Style != nil {
		d = *req.Style
	}
	req.Input = p.Spoken
	req.styled = &styledInput{parsed: p, defaults: d}
	return req
}

// ignoredNotes are the headers for an engine that cannot steer: the tags were removed from the text, and the client is told that is all that happened.
func (s *styledInput) ignoredNotes(why string) map[string]string {
	if s == nil {
		return nil
	}
	v := style.Default()
	r := style.Resolve(s.parsed, s.defaults, v)
	if !r.Active() && len(s.parsed.Uses) == 0 {
		return nil
	}
	return map[string]string{headerStyleWarnings: headerSafe(strings.Join(append([]string{"tags and style were removed from the text and not applied: " + why}, r.Warnings...), "; "))}
}

// headerSafe makes text fit in an HTTP header value: one line, ASCII, bounded.
func headerSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r > 0x7e:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 1500 {
		out = out[:1500] + "..."
	}
	return out
}

func setNotes(h http.Header, notes map[string]string) {
	for k, v := range notes {
		if v != "" {
			h.Set(k, headerSafe(v))
		}
	}
}

// ---- endpoints

type styleParseRequest struct {
	Text  string          `json:"text"`
	Style *style.Defaults `json:"style,omitempty"`
}

type styleUse struct {
	Name     string  `json:"name"`
	Tag      string  `json:"tag"`
	Strength float64 `json:"strength"`
	Scope    string  `json:"scope"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Open     bool    `json:"open,omitempty"`
}

type styleParseResponse struct {
	Spoken   string          `json:"spoken"`
	HasTags  bool            `json:"has_tags"`
	Uses     []styleUse      `json:"uses"`
	Plan     *style.Resolved `json:"plan"`
	Warnings []string        `json:"warnings"`
}

// handleStyleVocabulary lists the tags, so a client builds its palette and sliders from the server instead of carrying its own list.
func (s *Server) handleStyleVocabulary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, style.Default())
}

// handleStyleParse is the grammar as a service: text in, the spoken text, the tags found and the plan out. It needs no engine and costs nothing, so a client can call it
// as someone types. It is the one parser: a client does not re-implement it.
func (s *Server) handleStyleParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req styleParseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	v := style.Default()
	p := style.Parse(req.Text, v)
	var d style.Defaults
	if req.Style != nil {
		d = *req.Style
	}
	plan := style.Resolve(p, d, v)
	out := styleParseResponse{Spoken: p.Spoken, HasTags: p.HasTags(), Plan: plan, Warnings: plan.Warnings, Uses: []styleUse{}}
	for _, u := range p.Uses {
		out.Uses = append(out.Uses, styleUse{Name: u.Name, Tag: u.Tag.Name, Strength: u.Strength, Scope: u.Tag.Scope, Start: u.Start, End: u.End, Open: u.Open})
	}
	writeJSON(w, http.StatusOK, out)
}

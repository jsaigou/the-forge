// SPDX-License-Identifier: Apache-2.0

package duplexobs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// Redacted replaces a token-like substring in a log/event string.
const Redacted = "[redacted]"

// Field is one logfmt-style key/value pair.
type Field struct {
	Key   string
	Value string
}

// RedactionStatus is the session summary's account of what was stripped.
type RedactionStatus struct {
	Enabled                bool   `json:"enabled"`
	ReferenceAudioWithheld bool   `json:"reference_audio_withheld"`
	PatternsRedacted       uint64 `json:"patterns_redacted"`
}

var defaultPatterns = []*regexp.Regexp{
	// Authorization headers and protocol.md §1's bearer.<token> subprotocol.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{4,}`),
	regexp.MustCompile(`(?i)\bbearer\.[A-Za-z0-9._~+/=-]{4,}`),
	// Forge keys: sk-router-…, sk-mcp-…, sk-forge-…, and older sk- tokens.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{6,}`),
	// key=value / key: value forms for the usual secret-bearing names.
	regexp.MustCompile(`(?i)\b(?:token|secret|password|authorization|api[_-]?key)\s*[=:]\s*[^\s,;]+`),
}

// Redactor strips token-like text from reasons, ids and other free-form event
// fields before they reach a log or a session summary. It never handles audio:
// reference bytes are not accepted by the observer at all (see SetReference).
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor returns a redactor with the package's default patterns.
func NewRedactor() *Redactor { return &Redactor{patterns: defaultPatterns} }

// Redact returns s with token-like substrings replaced and the number of
// replacements. A nil Redactor behaves like NewRedactor.
func (r *Redactor) Redact(s string) (string, int) {
	if s == "" {
		return s, 0
	}
	if r == nil {
		r = NewRedactor()
	}
	n := 0
	for _, re := range r.patterns {
		s = re.ReplaceAllStringFunc(s, func(string) string {
			n++
			return Redacted
		})
	}
	return s, n
}

// ReferenceLogFields returns safe log fields for a session reference. It emits
// the reference kind and byte count only: the id and the audio bytes never
// reach a log line or a summary (protocol.md §9).
func ReferenceLogFields(ref duplex.Reference) []Field {
	kind := "none"
	switch {
	case ref.ID != "" && len(ref.Audio) > 0:
		kind = "named+audio"
	case ref.ID != "":
		kind = "named"
	case len(ref.Audio) > 0:
		kind = "audio"
	}
	return []Field{
		{Key: "reference", Value: kind},
		{Key: "reference_bytes", Value: strconv.Itoa(len(ref.Audio))},
		{Key: "reference_audio", Value: "withheld"},
	}
}

// LogLine renders fields as one logfmt line, quoting values that are empty or
// contain anything outside the conservative safe set.
func LogLine(fields []Field) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, formatField(f))
	}
	return strings.Join(parts, " ")
}

func formatField(f Field) string {
	if logfmtSafe(f.Value) {
		return f.Key + "=" + f.Value
	}
	return f.Key + "=" + strconv.Quote(f.Value)
}

func logfmtSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':', r == '/':
		default:
			return false
		}
	}
	return true
}

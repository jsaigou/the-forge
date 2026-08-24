// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"regexp"
	"strings"
)

// redact.go implements the Tier 2 context redaction rule (docs/v5-smith.md
// §4.3/§7): "the LLM never sees secrets — explicit redaction list: provider
// API keys, compressor tokens, session IDs, recovery codes — same list as
// docs/v5-store-schema.md's secret rule." Two layers, defense in depth:
//
//  1. sensitiveKeyNames — a key-name-based recursive redactor for
//     Finding.Evidence (map[string]any) and any other structured value that
//     enters context assembly. This is the primary defense: it does not
//     depend on knowing the shape of the secret, only its field name, so a
//     future check that widens its Evidence to include (say) a captured
//     request header can't silently leak a bearer token through it.
//  2. scrubSecretPatterns — a text-pattern safety net over the fully
//     assembled prompt string, catching key-shaped substrings
//     (sk-<kind>-<keyid>-<secret>, the one format every credential in this
//     repo shares — authz.go's GenerateKey) regardless of which field they
//     arrived in.
//
// Neither layer is a substitute for the other: (1) catches secrets nested
// inside structured evidence before it's ever serialized; (2) catches
// secrets that made it into free text (a notification message, a check
// summary string) where there is no key name to filter on.

// sensitiveKeyNames are map keys whose values are dropped wholesale by
// redactValue, matched case-insensitively against the last path segment
// (so "provider.api_key" and "apiKey" both match "api_key"/"apikey").
// Mirrors docs/v5-store-schema.md's secret rule: provider API keys,
// compressor tokens, session IDs, recovery codes, api_keys rows.
var sensitiveKeyNames = map[string]bool{
	"api_key":         true,
	"apikey":          true,
	"active_token":    true, // the exact field GET /compressor/config leaked, 2026-07-21
	"token":           true,
	"access_token":    true,
	"bearer_token":    true,
	"secret":          true,
	"password":        true,
	"password_hash":   true,
	"session_id":      true,
	"sessionid":       true,
	"cookie":          true,
	"authorization":   true,
	"recovery_code":   true,
	"recovery_codes":  true,
	"totp_secret":     true,
	"webauthn_secret": true,
	"pepper":          true,
}

// redactedPlaceholder replaces a dropped value so the shape of the
// structure (that a key existed at all) is still visible in evidence —
// useful for a human reading the transcript later — without the value.
const redactedPlaceholder = "[redacted]"

// redactValue recursively redacts v (typically a Finding.Evidence map or
// similar map[string]any tree) in place on a copy — the input is never
// mutated, since Evidence is also used for the FE's expandable-detail view
// and must keep its real values there.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitiveKeyName(k) {
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = redactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	case string:
		return scrubSecretPatterns(t)
	default:
		return v
	}
}

// isSensitiveKeyName reports whether key (possibly a dotted path, e.g.
// "smith.web.firecrawl.api_key") is or ends in a known-sensitive field name.
func isSensitiveKeyName(key string) bool {
	lower := strings.ToLower(key)
	if sensitiveKeyNames[lower] {
		return true
	}
	if i := strings.LastIndex(lower, "."); i >= 0 {
		return sensitiveKeyNames[lower[i+1:]]
	}
	return false
}

// secretPatterns catch key-shaped substrings regardless of which free-text
// field they arrived in. sk-<kind>-<keyid>-<secret> is the one format every
// credential in this repo shares (authz.GenerateKey); "Bearer <token>"
// catches a raw Authorization header value that leaked into a log line or
// error string.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[a-zA-Z0-9]+-[a-zA-Z0-9]+-[a-zA-Z0-9]{8,}`),
	regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9._\-]{16,}`),
}

// scrubSecretPatterns replaces any secret-shaped substring of s with the
// redacted placeholder.
func scrubSecretPatterns(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactedPlaceholder)
	}
	return s
}



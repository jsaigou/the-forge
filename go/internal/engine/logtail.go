// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"regexp"
	"strings"
	"time"
)

const (
	startupDetailLines    = 12
	startupDetailMaxRunes = 2000
)

// logSecretPattern matches things that must never be echoed into an API response from a service's own log:
// API keys and tokens in the forms this deployment uses, bearer credentials, and key=value / key: value pairs
// whose key names a credential.
var logSecretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{8,}|hf_[a-z0-9]{10,}|bearer\s+[a-z0-9._~+/=-]{8,}|(api[-_]?key|token|password|secret)\s*[=:]\s*\S+)`)

// redactLogTail drops blank lines, redacts credential-looking text, and keeps only the last startupDetailMaxRunes
// runes (the most recent output is the useful part of a crash).
func redactLogTail(raw string) string { return RedactLog(raw, startupDetailMaxRunes) }

// RedactLog is redactLogTail with a caller-chosen cap, for a log view that wants more than a failure detail.
func RedactLog(raw string, maxRunes int) string {
	var lines []string
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if l == "" {
			continue
		}
		lines = append(lines, logSecretPattern.ReplaceAllString(l, "[redacted]"))
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > maxRunes {
		out = "…" + string(r[len(r)-maxRunes:])
	}
	return out
}

// startupDetail is the redacted tail of a unit's log since a load began, for a failed start. "" when the engine
// was not given a LogTail.
func (m *Manager) startupDetail(ctx context.Context, unit string, since time.Time) string {
	if m.d.LogTail == nil {
		return ""
	}
	return redactLogTail(m.d.LogTail(ctx, unit, since, startupDetailLines))
}

// SPDX-License-Identifier: Apache-2.0

package procedures

import (
	"encoding/json"
	"slices"
	"strings"
)

// ArgvAllowed reports whether argv is exactly one of procID's registered
// steps' own Argv or Rollback. This is the re-validation dispatchProcedure
// performs immediately before every exec (belt-and-braces, same convention
// as execute.go's deleteAllowed/restartAllowed re-checks): a Step's Argv is
// fixed Go data at registration time, so under normal operation this is
// always true — its purpose is to make a hypothetical corrupted/mutated
// in-memory Procedure (or, longer-term, a step index that drifted from
// what was persisted) fail closed instead of silently executing an argv
// nobody actually reviewed.
func ArgvAllowed(procID string, argv []string) bool {
	p, ok := Get(procID)
	if !ok {
		return false
	}
	for _, step := range p.Steps {
		if slices.Equal(step.Argv, argv) || (len(step.Rollback) > 0 && slices.Equal(step.Rollback, argv)) {
			return true
		}
	}
	return false
}

// shallowTokenAllowed is a Param.Allowed for a bare identifier-shaped value
// (a systemd unit name, a slot name) — charset/shape only, matching the
// injection guard restartAllowed already enforces for real. The
// authoritative check for what a Sprint 3 native op actually does with this
// value stays in the op handler itself (runNativeOp, which calls
// restartAllowed/deleteAllowed unchanged).
func shallowTokenAllowed(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '@' || r == '.':
		default:
			return false
		}
	}
	return true
}

// binaryNameAllowed is a Param.Allowed for a smith.binaries.tracked display
// name (build_refresh's "binary" param) — unlike shallowTokenAllowed, real
// tracked-binary names legitimately contain spaces and parentheses (e.g.
// "llama.cpp (vulkan)"), so this only excludes shell metacharacters,
// control characters, and unreasonable length. The authoritative check is
// resolveBuildRefreshFork's live-setting lookup by exact string equality
// (internal/smith/build_refresh_forks.go) — a value that shape-passes here
// but doesn't exactly match a real tracked entry is refused there.
func binaryNameAllowed(v string) bool {
	if v == "" || len(v) > 200 {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return !strings.ContainsAny(v, "\t\n;|&$`")
}

// jsonArrayParamAllowed is a Param.Allowed for a JSON-encoded array value
// (comfyui_prune's files_json) — well-formedness only. Per-entry validation
// (every path really is under an allowlisted ComfyUI model root) happens in
// the op handler via deleteAllowed, the same as every other native op.
func jsonArrayParamAllowed(v string) bool {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return false
	}
	trimmed := []byte(raw)
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\n' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	return len(trimmed) > 0 && trimmed[0] == '['
}

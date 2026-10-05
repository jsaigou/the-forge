// Package toolmap attributes a tool-result message to the tool that produced
// it (contract C3). It is used only by the observe-only context-creation
// ledger: nothing here mutates, reorders or drops any message (P0).
package toolmap

import "strings"

// Unknown is the attribution bucket for results whose tool cannot be resolved.
const Unknown = "unknown"

const (
	maxNameLen = 96
	mcpInfix   = "_mcp_" // LibreChat: "<tool>_mcp_<server>"
)

// Resolve returns the raw function name of the assistant tool_call that owns
// the tool-result message at messages[toolMsgIndex]. ok is false (callers
// attribute to Unknown) when the index is out of range, the message is not a
// tool result, its tool_call_id is missing or not a string, or no assistant
// message *before* it declares a matching call (tool before call).
//
// Duplicate ids resolve to the nearest preceding declaration. Parallel tool
// calls are handled because every tool_calls entry of an assistant message is
// inspected. The scan is a single bounded backward pass, O(len(messages)),
// with no allocation proportional to input size. Legacy role "function"
// messages carry the name themselves and are resolved from it.
func Resolve(messages []any, toolMsgIndex int) (name string, ok bool) {
	if toolMsgIndex < 0 || toolMsgIndex >= len(messages) {
		return "", false
	}
	msg, _ := messages[toolMsgIndex].(map[string]any)
	if msg == nil {
		return "", false
	}
	role, _ := msg["role"].(string)
	switch role {
	case "function":
		n, _ := msg["name"].(string)
		return n, n != ""
	case "tool":
	default:
		return "", false
	}
	id, _ := msg["tool_call_id"].(string)
	if id == "" {
		return "", false
	}
	for i := toolMsgIndex - 1; i >= 0; i-- {
		m, _ := messages[i].(map[string]any)
		if m == nil {
			continue
		}
		if r, _ := m["role"].(string); r != "assistant" {
			continue
		}
		calls, _ := m["tool_calls"].([]any)
		// later entries first so a duplicate id inside one message resolves
		// to the last declaration, consistent with "nearest preceding".
		for j := len(calls) - 1; j >= 0; j-- {
			c, _ := calls[j].(map[string]any)
			if c == nil {
				continue
			}
			if cid, _ := c["id"].(string); cid != id {
				continue
			}
			fn, _ := c["function"].(map[string]any)
			n, _ := fn["name"].(string)
			if n == "" {
				return "", false
			}
			return n, true
		}
	}
	return "", false
}

// Normalize maps a raw tool name to its canonical attribution key. It is
// total (any input yields a non-empty, bounded, [a-z0-9_.-] string) and
// idempotent. Rules, each grounded in observed naming (see table test):
//
//   - lower-case; any char outside [a-z0-9_.-] becomes '_'
//   - "functions." / "functions_" namespace prefix dropped
//   - LibreChat MCP "<tool>_mcp_<server>" -> "<tool>"
//   - Claude-Code style "mcp__<server>__<tool>" -> "<tool>"
//   - edge '_' '-' '.' trimmed; empty -> Unknown; capped at 96 bytes
//
// OpenCode MCP names ("deja_recall") are "<server>_<tool>" with no reliable
// delimiter, so they are kept whole: distinct tools stay distinct.
func Normalize(raw string) string {
	s := clean(raw)
	for {
		n := step(s)
		if n == s {
			break
		}
		s = n
	}
	if s == "" {
		return Unknown
	}
	return s
}

func clean(raw string) string {
	b := make([]byte, 0, min(len(raw), 4*maxNameLen))
	for i := 0; i < len(raw) && len(b) < 4*maxNameLen; i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			c = '_'
		}
		b = append(b, c)
	}
	return string(b)
}

// builtinMCPNames are real client built-ins (OpenCode) that merely contain
// "_mcp_" and must not be mistaken for LibreChat's "<tool>_mcp_<server>".
var builtinMCPNames = map[string]bool{
	"list_mcp_resources":          true,
	"list_mcp_resource_templates": true,
	"read_mcp_resource":           true,
}

// step performs one strictly-shortening rewrite (or none) so the caller's
// fixpoint loop terminates and the result is idempotent.
func step(s string) string {
	s = strings.Trim(s, "_-.")
	if t, ok := strings.CutPrefix(s, "functions."); ok {
		return t
	}
	if t, ok := strings.CutPrefix(s, "functions_"); ok {
		return t
	}
	if t, ok := strings.CutPrefix(s, "mcp__"); ok {
		if i := strings.LastIndex(t, "__"); i >= 0 && i+2 < len(t) {
			return t[i+2:]
		}
		return t
	}
	if i := strings.Index(s, mcpInfix); i > 0 && !builtinMCPNames[s] {
		return s[:i]
	}
	if len(s) > maxNameLen {
		return s[:maxNameLen]
	}
	return s
}

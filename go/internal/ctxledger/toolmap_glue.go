// SPDX-License-Identifier: Apache-2.0

package ctxledger

import "github.com/jsaigou/the-forge/internal/ctxledger/toolmap"

// resolveTool / normalizeTool are the only call sites of C3 (toolmap, WS-B1).
func resolveTool(messages []any, idx int) (string, bool) { return toolmap.Resolve(messages, idx) }

func normalizeTool(raw string) string { return toolmap.Normalize(raw) }

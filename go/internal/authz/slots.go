// SPDX-License-Identifier: Apache-2.0

package authz

// ValidSlots is the frozen set of valid bay/slot names (a1–a4). Shared by
// httpapi and mcp to keep the slot-name literal in one place — previously
// duplicated as local validSlots maps that had to move in lockstep.
var ValidSlots = map[string]bool{"a1": true, "a2": true, "a3": true, "a4": true}

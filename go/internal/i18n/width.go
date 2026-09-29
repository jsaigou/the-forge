// SPDX-License-Identifier: Apache-2.0

package i18n

import "github.com/mattn/go-runewidth"

// PadRight pads s with spaces on the right so its terminal *display* width
// (not its rune or byte count) is at least w — go-runewidth's FillRight.
// Go's own "%-Ns" Sprintf verb pads by rune count, so a translated Japanese
// label (2 display columns per character on any real terminal) would run
// twice as wide as an ASCII label of the same rune count and break every
// fixed-width column in the TUI/CLI. Use this wherever a padded value might
// ever be translated text; identifier columns (slot IDs, config names) can
// keep the plain %-Ns verb since those are never translated.
//
// The plan (docs/adr/0016-localization.md Phase 4) placed this helper in
// internal/tui; it lives here in internal/i18n instead so internal/cli and
// cmd/forge's CLI subcommands can use it too without an import cycle
// (internal/tui already imports internal/cli, so internal/cli cannot import
// internal/tui back).
func PadRight(s string, w int) string {
	return runewidth.FillRight(s, w)
}

// PadLeft pads s with spaces on the left so its terminal display width is
// at least w (go-runewidth's FillLeft) — the display-width-safe equivalent
// of Go's right-justifying "%Ns" Sprintf verb, for the same reason PadRight
// replaces "%-Ns".
func PadLeft(s string, w int) string {
	return runewidth.FillLeft(s, w)
}

// Truncate shortens s to fit within w terminal display columns, appending
// "…" if it had to cut — go-runewidth's Truncate. Replaces the old
// rune-counting truncate() in internal/tui/tui.go, which let a wide (CJK)
// string run past its intended column budget since it counted runes, not
// display columns.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.Truncate(s, w, "…")
}

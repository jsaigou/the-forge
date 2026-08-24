// SPDX-License-Identifier: Apache-2.0

package compress

import "github.com/dlclark/regexp2"

// mustKeepPattern is Kompress's own must-keep safety net
// (_KOMPRESS_MUST_KEEP_RE in headroom-ai's kompress_compressor.py — numbers,
// ALLCAPS identifiers, dotted paths, unix paths, extensions, CLI flags, and
// CamelCase names carry meaning an agent can't reconstruct from context),
// with Sprint 1's fix applied to the "standalone number" branch: the
// original used a \w-boundary lookaround, and Python's \w is
// Unicode-aware, so it silently failed to protect a number glued to CJK
// text with no separating whitespace — normal Japanese price/count/date
// formatting ("104件", "2,100円", "2024年8月22日"), i.e. this fleet's
// primary content shape. Fixed to a digit-only boundary: a number is
// protected unless it's glued to *other digits*, regardless of adjacent
// script. Verified in the original bake-off that this doesn't regress the
// other patterns (hex/ALLCAPS/paths/flags/CamelCase all still match their
// test cases unchanged) — see
// .sweep/headroom-bakeoff-2026-08-19.md's "Follow-up" section.
//
// Go's stdlib regexp (RE2) has no lookaround support at all, so this uses
// dlclark/regexp2 (a pure-Go, .NET-style engine with lookaround) instead —
// a deliberate, narrow dependency for exactly this one matcher; nothing
// else in this package needs it.
var mustKeepPattern = regexp2.MustCompile(
	`\b0x[0-9A-Fa-f]+\b`+ // hex addresses/IDs: 0x7fff2038
		`|(?<!\d)\d+(?:\.\d+)?(?!\d)`+ // standalone numbers, digit-only boundary (the fix): 42, 3.14, 104件, 2,100円
		`|[A-Z_]{2,}`+ // ALLCAPS: SIGILL, HTTP, EOF, ERROR
		`|[a-z_][a-z0-9_]*\.[a-z0-9_]+`+ // dotted.paths: libsystem_kernel.dylib
		`|/[a-z0-9/._-]{2,}`+ // unix paths: /usr/lib/python3.so
		`|\.[a-z]{2,4}\b`+ // extensions: .py .so .json
		`|--?[a-z][\w-]*`+ // flags: --verbose, -n
		`|\b[A-Z][a-z]+[A-Z]\w*`, // CamelCase: EXC_BAD_INSTRUCTION, IndexError
	regexp2.None,
)

// mustKeep reports whether word matches the must-keep pattern and should
// survive regardless of the model's own keep-mask. Applied per
// already-whitespace-split word, never across word boundaries — mirrors
// _add_kompress_must_keep_words's per-word .search(word) call.
func mustKeep(word string) (bool, error) {
	m, err := mustKeepPattern.FindStringMatch(word)
	if err != nil {
		return false, err
	}
	return m != nil, nil
}

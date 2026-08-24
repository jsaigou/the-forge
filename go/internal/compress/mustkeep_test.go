// SPDX-License-Identifier: Apache-2.0

package compress

import "testing"

func TestMustKeep_CJKGluedNumbers(t *testing.T) {
	// The exact bug Sprint 1 found: these are real content shapes from
	// the fleet's primary source material (kakaku/yahoo-auctions listings)
	// where a number sits directly against CJK text with no separating
	// whitespace or punctuation. The original \w-boundary regex silently
	// failed to protect any of these; the digit-only boundary must.
	cases := []string{
		"104件",
		"2,100円",
		"2024年8月22日発売",
		"62,200円",
		"979件",
		"144,999円",
	}
	for _, word := range cases {
		ok, err := mustKeep(word)
		if err != nil {
			t.Fatalf("mustKeep(%q): %v", word, err)
		}
		if !ok {
			t.Errorf("mustKeep(%q) = false, want true (CJK-glued number must be protected)", word)
		}
	}
}

func TestMustKeep_OriginalPatternsStillMatch(t *testing.T) {
	// The fix must not regress any of the original safety net's other
	// patterns — verified in the bake-off, reproduced here as a
	// regression guard.
	cases := map[string]string{
		"0x7fff2038":             "hex address",
		"SIGILL":                 "ALLCAPS",
		"HTTP":                   "ALLCAPS",
		"libsystem_kernel.dylib": "dotted path",
		"/usr/lib/python3.so":    "unix path",
		".json":                  "extension",
		"--verbose":              "long flag",
		"-n":                     "short flag",
		"IndexError":             "CamelCase",
		"EXC_BAD_INSTRUCTION":    "ALLCAPS with underscore",
		"42":                     "bare number",
		"3.14":                   "decimal number",
	}
	for word, desc := range cases {
		ok, err := mustKeep(word)
		if err != nil {
			t.Fatalf("mustKeep(%q): %v", word, err)
		}
		if !ok {
			t.Errorf("mustKeep(%q) [%s] = false, want true", word, desc)
		}
	}
}

func TestMustKeep_OrdinaryWordsNotForceKept(t *testing.T) {
	for _, word := range []string{"the", "hello", "です", "auction", "件名"} {
		ok, err := mustKeep(word)
		if err != nil {
			t.Fatalf("mustKeep(%q): %v", word, err)
		}
		if ok {
			t.Errorf("mustKeep(%q) = true, want false (ordinary word, not must-keep shaped)", word)
		}
	}
}

func TestMustKeep_DigitBoundaryDoesNotOverProtect(t *testing.T) {
	// A number glued to *other digits* (not CJK/letters) is not a
	// "standalone number" and should not itself trigger this branch in
	// isolation from its neighbor — e.g. "12000" is one standalone number
	// (matches as a whole), not evidence of a boundary bug. This test just
	// guards that the fix didn't accidentally become "always true".
	ok, err := mustKeep("plainword")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("mustKeep(%q) = true, want false", "plainword")
	}
}

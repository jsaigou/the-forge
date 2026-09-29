// SPDX-License-Identifier: Apache-2.0

package i18n

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// verbRe matches one Go fmt verb, including an explicit argument index
// (%[2]s) or a literal %% — used to count substitution slots per template
// so en/ja can be compared for interpolation parity without requiring
// identical verb *order* (Japanese word order often differs — a template
// may reorder via %[n] explicit indices; see key_and_verb_parity below).
var verbRe = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[vTtbcdoOqxXUeEfFgGsp%]`)

func countVerbs(tmpl string) (total int, literalPercent int) {
	for _, m := range verbRe.FindAllString(tmpl, -1) {
		if m == "%%" {
			literalPercent++
			continue
		}
		total++
	}
	return total, literalPercent
}

// TestKeyAndVerbParity mirrors web/scripts/check-locales.mjs's job for this
// package's own catalog: every key in en must exist in ja (and vice versa),
// and every key's substitution-slot count must match between languages —
// a mismatch means a ja template that would either drop an argument or
// panic fmt.Sprintf with "too many/few arguments".
func TestKeyAndVerbParity(t *testing.T) {
	en, ja := catalogs["en"], catalogs["ja"]

	var missingInJa, missingInEn []string
	for k := range en {
		if _, ok := ja[k]; !ok {
			missingInJa = append(missingInJa, k)
		}
	}
	for k := range ja {
		if _, ok := en[k]; !ok {
			missingInEn = append(missingInEn, k)
		}
	}
	sort.Strings(missingInJa)
	sort.Strings(missingInEn)
	if len(missingInJa) > 0 {
		t.Errorf("keys in en.json missing from ja.json: %v", missingInJa)
	}
	if len(missingInEn) > 0 {
		t.Errorf("keys in ja.json missing from en.json: %v", missingInEn)
	}

	for k, enTmpl := range en {
		jaTmpl, ok := ja[k]
		if !ok {
			continue // already reported above
		}
		enN, _ := countVerbs(enTmpl)
		jaN, _ := countVerbs(jaTmpl)
		if enN != jaN {
			t.Errorf("key %q: en has %d substitution slot(s), ja has %d — en=%q ja=%q", k, enN, jaN, enTmpl, jaTmpl)
		}
		// TParams-style "{{name}}" keys (the errors.* sub-catalog mirroring
		// web/src/locales/*/errors.json) — same param *names* must appear on
		// both sides, not just a matching count, since TParams looks values
		// up by name.
		enParams := namedParams(enTmpl)
		jaParams := namedParams(jaTmpl)
		if !equalStringSets(enParams, jaParams) {
			t.Errorf("key %q: en {{params}} %v != ja {{params}} %v", k, enParams, jaParams)
		}
	}

	if len(en) == 0 {
		t.Error("en catalog is empty — expected at least the foundation keys")
	}
}

func namedParams(tmpl string) []string {
	matches := paramRe.FindAllStringSubmatch(tmpl, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLang covers the FORGE_LANG > LC_ALL > LANG priority and the ja-prefix
// detection rule.
func TestLang(t *testing.T) {
	for _, envVar := range []string{"FORGE_LANG", "LC_ALL", "LANG"} {
		t.Setenv(envVar, "")
		os.Unsetenv(envVar)
	}

	cases := []struct {
		name                   string
		forgeLang, lcAll, lang string
		want                   string
	}{
		{"nothing set", "", "", "", "en"},
		{"LANG ja", "", "", "ja_JP.UTF-8", "ja"},
		{"LANG non-ja", "", "", "en_US.UTF-8", "en"},
		{"LC_ALL overrides LANG", "", "ja_JP.UTF-8", "en_US.UTF-8", "ja"},
		{"FORGE_LANG overrides everything", "en", "ja_JP.UTF-8", "ja_JP.UTF-8", "en"},
		{"FORGE_LANG=ja wins", "ja", "", "en_US.UTF-8", "ja"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FORGE_LANG", tc.forgeLang)
			t.Setenv("LC_ALL", tc.lcAll)
			t.Setenv("LANG", tc.lang)
			if tc.forgeLang == "" {
				os.Unsetenv("FORGE_LANG")
			}
			if tc.lcAll == "" {
				os.Unsetenv("LC_ALL")
			}
			if tc.lang == "" {
				os.Unsetenv("LANG")
			}
			if got := Lang(); got != tc.want {
				t.Errorf("Lang() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestT_FallbackChain proves a key missing from ja falls back to en, and a
// key missing from both falls back to the raw key — never a panic.
func TestT_FallbackChain(t *testing.T) {
	catalogs["en"]["__test_only_key"] = "hello %s"
	defer delete(catalogs["en"], "__test_only_key")

	if got := t2("ja", "__test_only_key", "world"); got != "hello world" {
		t.Errorf("fallback to en = %q, want %q", got, "hello world")
	}
	if got := t2("en", "__totally_unknown_key__"); got != "__totally_unknown_key__" {
		t.Errorf("fallback to raw key = %q, want the key itself", got)
	}
}

// t2 is a test-local alias for the unexported t() so this file reads
// clearly (t is also the *testing.T parameter name in every test func).
func t2(lang, key string, args ...any) string {
	return t(lang, key, args...)
}

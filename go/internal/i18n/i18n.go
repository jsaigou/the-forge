// SPDX-License-Identifier: Apache-2.0

// Package i18n is the forge TUI/CLI's translation catalog — Phase 4 of the
// multilanguage plan (docs/adr/0016-localization.md), a small hand-rolled
// equivalent of the web PWA's i18next setup (web/src/lib/i18n.ts) scoped to
// what a terminal client needs: an embedded JSON catalog per language,
// Sprintf-style interpolation (Go %-verbs, not i18next's {{}} — this matches
// how every existing CLI/TUI string was already built with fmt.Sprintf), and
// a three-level fallback (requested lang -> en -> the key itself, so a
// missing/typo'd key degrades to visibly-wrong text rather than panicking a
// terminal session).
//
// Identifiers — slot IDs, config/model names, flag names, command names,
// --json output — are never routed through T(); only human-facing prose
// (headers, status words, help text) is.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

//go:embed locales/en.json locales/ja.json
var localeFS embed.FS

var catalogs map[string]map[string]string

func init() {
	catalogs = make(map[string]map[string]string, 2)
	for _, lang := range []string{"en", "ja"} {
		b, err := localeFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			panic("i18n: embedded locale missing: " + err.Error())
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			panic("i18n: malformed locale " + lang + ": " + err.Error())
		}
		catalogs[lang] = m
	}
}

// Lang resolves the active language: FORGE_LANG takes priority, then
// LC_ALL, then LANG (a "ja"-prefixed value, case-insensitive — e.g.
// "ja_JP.UTF-8" — maps to "ja"; anything else maps to "en"). The first of
// the three that is set at all is decisive; an unset var is skipped, not
// treated as "en" itself, so FORGE_LANG="" doesn't shadow a real LC_ALL/LANG
// value. Defaults to "en" when none are set. Mirrors the web PWA's
// detectInitialLang (lib/i18n.ts) at the environment-variable layer.
func Lang() string {
	for _, envVar := range []string{"FORGE_LANG", "LC_ALL", "LANG"} {
		v := os.Getenv(envVar)
		if v == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(v), "ja") {
			return "ja"
		}
		return "en"
	}
	return "en"
}

// T looks up key in the resolved language's catalog (Lang()), applying args
// via fmt.Sprintf when given. Falls back to the English catalog, then to
// the raw key itself, for a key missing from the target language or from
// both — this must never panic a running TUI/CLI session over a catalog
// gap.
func T(key string, args ...any) string {
	return t(Lang(), key, args...)
}

// t is T with an explicit language, split out so tests can exercise the
// fallback chain without mutating process environment variables.
func t(lang, key string, args ...any) string {
	tmpl, ok := catalogs[lang][key]
	if !ok {
		tmpl, ok = catalogs["en"][key]
	}
	if !ok {
		tmpl = key
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

// Exists reports whether key has a catalog entry (in the resolved language
// or the English fallback) — the CLI's backend-error path
// (internal/cli/client.go) uses this to decide between a translated
// "errors.<code>" lookup and the server's raw (always-English) error text,
// the same exists()-gated pattern web/src/lib/api.ts's apiErrorMessage uses.
func Exists(key string) bool {
	lang := Lang()
	if _, ok := catalogs[lang][key]; ok {
		return true
	}
	_, ok := catalogs["en"][key]
	return ok
}

// paramRe matches a "{{name}}" placeholder.
var paramRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// TParams looks up key like T, but interpolates "{{name}}" placeholders
// from a named params map instead of applying Sprintf %-verbs positionally.
// This exists specifically for the "errors.*" sub-catalog, which mirrors
// web/src/locales/{en,ja}/errors.json verbatim (including its "{{resource}}"-
// style placeholders) so the two catalogs stay directly comparable/copyable
// — the backend's writeErrorCode JSON body already carries a named `params`
// object, not a positional argument list. A placeholder with no matching
// param is left as literal text (visibly wrong rather than silently
// dropped). Falls back exactly like T: target lang -> en -> raw key.
func TParams(key string, params map[string]any) string {
	lang := Lang()
	tmpl, ok := catalogs[lang][key]
	if !ok {
		tmpl, ok = catalogs["en"][key]
	}
	if !ok {
		return key
	}
	return paramRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		name := m[2 : len(m)-2]
		if v, ok := params[name]; ok {
			return fmt.Sprintf("%v", v)
		}
		return m
	})
}

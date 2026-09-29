// SPDX-License-Identifier: Apache-2.0

package httpapi

// errors_codes_test.go — the i18n Phase 2 coverage guard
// (docs/adr/0016-localization.md). Every stable error `code` this package
// emits via writeErrorCode/writeValidationErrorCodes must have a matching
// entry in BOTH web/src/locales/en/errors.json and .../ja/errors.json, so
// the frontend's apiErrorMessage never silently falls back to English for a
// code that was meant to be translatable. This scans the actual source
// (go/parser over every non-test .go file in this package) rather than
// hand-maintaining a list — a new call site with a new code fails the build
// until both catalogs gain an entry, the same way check-locales.mjs guards
// the frontend's own namespaces.
//
// Only literal string codes are supported (a code built from a variable or
// constant expression is invisible to this scan) — every call site added so
// far uses a literal, and that's the intended style: a `code` is a fixed
// wire identifier, never computed.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// scanErrorCodes walks every non-test .go file in this package's directory
// and collects every literal `code` argument passed to writeErrorCode or
// literal value in a writeValidationErrorCodes `codes` map literal.
func scanErrorCodes(t *testing.T) map[string]bool {
	t.Helper()
	codes := map[string]bool{}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch fn.Name {
			case "writeErrorCode":
				// writeErrorCode(w, status, code, params, msg) — code is arg index 2.
				if len(call.Args) > 2 {
					if lit, ok := call.Args[2].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := stringLitValue(lit); err == nil {
							codes[s] = true
						}
					}
				}
			case "writeValidationErrorCodes":
				// writeValidationErrorCodes(w, fields, codes) — codes is arg index 2,
				// a map[string]string composite literal (or nil).
				if len(call.Args) > 2 {
					collectMapLiteralValues(call.Args[2], codes)
				}
			}
			return true
		})
	}
	return codes
}

func stringLitValue(lit *ast.BasicLit) (string, error) {
	return strconv.Unquote(lit.Value)
}

// collectMapLiteralValues extracts every string literal value from a
// map[string]string{...} composite literal expression (skips non-literal
// entries silently — a computed value can't be statically checked here).
func collectMapLiteralValues(expr ast.Expr, out map[string]bool) {
	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if s, err := stringLitValue(lit); err == nil {
			out[s] = true
		}
	}
}

// loadErrorCatalogKeys reads a flat {code: "translated text"} JSON catalog
// (web/src/locales/{en,ja}/errors.json) and returns its key set. The repo
// layout is fixed (go/internal/httpapi is always 3 levels under the repo
// root, which always has a sibling web/ directory) — no env var or flag
// needed.
func loadErrorCatalogKeys(t *testing.T, lang string) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "..", "web", "src", "locales", lang, "errors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// TestErrorCodesHaveTranslations is the coverage guard referenced in
// writeErrorCode/writeValidationErrorCodes's own doc comments: every code
// literal used anywhere in this package must exist in both locale catalogs.
func TestErrorCodesHaveTranslations(t *testing.T) {
	used := scanErrorCodes(t)
	if len(used) == 0 {
		t.Fatal("scanned zero error codes — the AST scan is probably broken (expected at least \"internal\")")
	}
	en := loadErrorCatalogKeys(t, "en")
	ja := loadErrorCatalogKeys(t, "ja")

	var missingEn, missingJa []string
	for code := range used {
		if !en[code] {
			missingEn = append(missingEn, code)
		}
		if !ja[code] {
			missingJa = append(missingJa, code)
		}
	}
	sort.Strings(missingEn)
	sort.Strings(missingJa)
	if len(missingEn) > 0 {
		t.Errorf("codes used in go/internal/httpapi with no en/errors.json entry: %v", missingEn)
	}
	if len(missingJa) > 0 {
		t.Errorf("codes used in go/internal/httpapi with no ja/errors.json entry: %v", missingJa)
	}
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

// ADR-0020 F6: every error body carries a stable `code`. The bare writeError helper (message only, no code) was
// removed; this keeps it from coming back, and pins the shape of the helpers that replaced it.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNoBareWriteErrorCalls(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			if c, ok := node.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "writeError" {
					t.Errorf("%s: bare writeError call; use writeErrorCode with a code in web/src/locales/{en,ja}/errors.json", fset.Position(c.Pos()))
				}
			}
			return true
		})
	}
}

func TestErrorHelpersAlwaysCarryACode(t *testing.T) {
	cases := map[string]func(w *httptest.ResponseRecorder){
		"coded": func(w *httptest.ResponseRecorder) {
			writeErrorCode(w, 404, "not_found", map[string]any{"resource": "x"}, "x not found")
		},
		"validation": func(w *httptest.ResponseRecorder) { writeValidationError(w, map[string]string{"name": "required"}) },
		"operation": func(w *httptest.ResponseRecorder) {
			writeOpError(w, 409, "slot_loading", map[string]any{"slot": "a1"}, "Slot a1 already loading")
		},
	}
	for name, fn := range cases {
		w := httptest.NewRecorder()
		fn(w)
		var b map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c, _ := b["code"].(string); c == "" {
			t.Errorf("%s: body has no code: %s", name, w.Body.String())
		}
		if m, _ := b["error"].(string); m == "" {
			t.Errorf("%s: body has no error message: %s", name, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	writeOpError(w, 409, "slot_loading", nil, "m")
	var b map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if _, has := b["success"]; has || b["message"] != "m" {
		t.Errorf("operation error must keep message and no longer carry success: %s", w.Body.String())
	}
}

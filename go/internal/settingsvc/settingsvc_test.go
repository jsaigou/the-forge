// SPDX-License-Identifier: Apache-2.0

package settingsvc

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// A settings key used anywhere in the source must be registered here, or `forge config set` would refuse it.
func TestKnownKeysCoverSource(t *testing.T) {
	re := regexp.MustCompile(`^(auth|billing|catalog|compressor|dashboard|infra|metrics|router|scheduler|usage|wizard|smith)\.[a-z_][a-z_0-9.]*$`)
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(path, "settingsvc") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, uerr := strconv.Unquote(bl.Value); uerr == nil && re.MatchString(s) && !Known(s) {
					missing = append(missing, s+" ("+fset.Position(bl.Pos()).String()+")")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range missing {
		t.Errorf("settings key not registered in settingsvc.knownKeys: %s", m)
	}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		force bool
		ok    bool
	}{
		{"unknown key refused", "smith.binaries.trackd", `[]`, false, false},
		{"unknown key forced", "test.scratch", `"x"`, true, true},
		{"open family accepted", "ui.theme", `"dark"`, false, true},
		{"not json", "ui.theme", `dark`, false, false},
		{"not json even when forced", "test.scratch", `dark`, true, false},
		{"bool ok", "catalog.require_if_match", `true`, false, true},
		{"bool wrong type", "catalog.require_if_match", `"yes"`, false, false},
		{"wrong shape is refused even when forced", "catalog.require_if_match", `1`, true, false},
		{"tracked ok", "smith.binaries.tracked", `[{"name":"llama.cpp","kind":"llama_build","path":"/opt/x/bin/llama-server","source_kind":"git","source_ref":"/opt/x","upstream_ref":"origin/master"}]`, false, true},
		{"tracked empty ok", "smith.binaries.tracked", `[]`, false, true},
		{"tracked not an array", "smith.binaries.tracked", `{"name":"x"}`, false, false},
		{"tracked bad kind", "smith.binaries.tracked", `[{"name":"a","kind":"binary","path":"/x"}]`, false, false},
		{"tracked relative path", "smith.binaries.tracked", `[{"name":"a","kind":"runtime","path":"bin/x"}]`, false, false},
		{"tracked shell metachar", "smith.binaries.tracked", `[{"name":"a","kind":"runtime","path":"/x;rm -rf /"}]`, false, false},
		{"tracked git without tree", "smith.binaries.tracked", `[{"name":"a","kind":"runtime","path":"/x","source_kind":"git"}]`, false, false},
		{"tracked unknown field", "smith.binaries.tracked", `[{"name":"a","kind":"runtime","path":"/x","colour":"red"}]`, false, false},
		{"tracked duplicate", "smith.binaries.tracked", `[{"name":"a","kind":"runtime","path":"/x"},{"name":"a","kind":"runtime","path":"/y"}]`, false, false},
		{"forks ok", "smith.build_refresh.forks", `[{"source_ref":"/opt/x","remote":"origin","upstream_ref":"origin/master","backends":{}}]`, false, true},
		{"forks missing remote", "smith.build_refresh.forks", `[{"source_ref":"/opt/x","upstream_ref":"origin/master"}]`, false, false},
		{"build roots ok", "catalog.build_roots", `["/opt/forge","/opt/forge"]`, false, true},
		{"build roots empty", "catalog.build_roots", `[]`, false, false},
		{"build roots relative", "catalog.build_roots", `["opt/forgehost"]`, false, false},
		{"launch env roots ok", "catalog.launch_env_roots", `["/opt/forge/rocm-therock-10.1"]`, false, true},
		{"launch env roots relative", "catalog.launch_env_roots", `["opt/rocm"]`, false, false},
		{"build roots filesystem root", "catalog.build_roots", `["/"]`, false, false},
		{"watchlist ok", "smith.build_refresh.watchlist", `["gfx1151"]`, false, true},
		{"watchlist wrong", "smith.build_refresh.watchlist", `"gfx1151"`, false, false},
	}
	for _, c := range cases {
		err := Check(c.key, []byte(c.value), c.force)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

type memSettings map[string][]byte

func (m memSettings) Get(_ context.Context, k string) ([]byte, error) {
	if v, ok := m[k]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}
func (m memSettings) Set(_ context.Context, k string, v []byte) error { m[k] = v; return nil }

// Every writer behind the wrapper gets the shape checks; an unknown key is still allowed (only the CLI refuses it).
func TestValidatingStoreChecksShapeOnEveryWrite(t *testing.T) {
	inner := memSettings{}
	s := Validating(inner)
	ctx := context.Background()
	if err := s.Set(ctx, "smith.binaries.tracked", []byte(`[{"name":"x","kind":"binary","path":"/x"}]`)); err == nil {
		t.Error("a mis-shaped smith.binaries.tracked was written through the wrapper")
	}
	if _, ok := inner["smith.binaries.tracked"]; ok {
		t.Error("a refused write reached the store")
	}
	if err := s.Set(ctx, "smith.build_refresh.watchlist", []byte(`["a"]`)); err != nil {
		t.Errorf("valid write refused: %v", err)
	}
	if err := s.Set(ctx, "some.runtime.key", []byte(`1`)); err != nil {
		t.Errorf("an unknown key was refused behind the wrapper: %v", err)
	}
	if got, err := s.Get(ctx, "smith.build_refresh.watchlist"); err != nil || string(got) != `["a"]` {
		t.Errorf("Get = %q, %v", got, err)
	}
}

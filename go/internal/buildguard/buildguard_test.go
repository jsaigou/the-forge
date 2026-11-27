// SPDX-License-Identifier: Apache-2.0

package buildguard

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func TestCheckBinary(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "t", "bin", "llama-server")
	_ = os.MkdirAll(filepath.Dir(good), 0o755)
	_ = os.WriteFile(good, []byte("x"), 0o755)
	noexec := filepath.Join(root, "noexec")
	_ = os.WriteFile(noexec, []byte("x"), 0o644)
	outside := filepath.Join(t.TempDir(), "llama-server")
	_ = os.WriteFile(outside, []byte("x"), 0o755)
	link := filepath.Join(root, "link")
	_ = os.Symlink(outside, link)
	roots := Roots([]string{root})
	for name, c := range map[string]struct {
		path string
		ok   bool
	}{
		"good": {good, true}, "relative": {"bin/x", false}, "metachar": {good + ";x", false},
		"outside": {outside, false}, "missing": {filepath.Join(root, "nope"), false}, "dir": {root, false},
		"not executable": {noexec, false}, "symlink out": {link, false},
	} {
		if got := CheckBinary(c.path, roots); (got == "") != c.ok {
			t.Errorf("%s: CheckBinary = %q, want ok=%v", name, got, c.ok)
		}
	}
}

func TestRootsDefaultsAndDropsRelative(t *testing.T) {
	if !reflect.DeepEqual(Roots(nil), DefaultRoots) {
		t.Errorf("Roots(nil) = %v, want the defaults", Roots(nil))
	}
	if got := Roots([]string{"/opt/a/", "rel"}); !reflect.DeepEqual(got, []string{"/opt/a"}) {
		t.Errorf("Roots = %v", got)
	}
}

func TestConfigsUsing(t *testing.T) {
	cfgs := []store.Config{{Name: "a", BuildID: 1}, {Name: "b", BuildID: 2}, {Name: "c", BuildID: 1}}
	if got := ConfigsUsing(cfgs, 1); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("ConfigsUsing = %v", got)
	}
}

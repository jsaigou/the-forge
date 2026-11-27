// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDestRelPathNeverEscapesAndFilenamesAreValidated(t *testing.T) {
	for _, name := range []string{"..", ".", "/", "", "a/..", "x/\x00"} {
		got := DestRelPath("", name)
		if got == ".." || strings.HasPrefix(got, "../") || filepath.IsAbs(got) || strings.Contains(got, "..") {
			t.Errorf("DestRelPath(%q) = %q escapes", name, got)
		}
	}
	for _, d := range []string{"../../etc", "/abs/dir", "a/../../b"} {
		if got := DestRelPath(d, "m.gguf"); strings.Contains(got, "..") || filepath.IsAbs(got) {
			t.Errorf("DestRelPath(%q) = %q escapes", d, got)
		}
	}
	for name, want := range map[string]bool{"m.gguf": true, "Q4_K_M/m-00001-of-00003.gguf": true, "": false, "..": false, "../x": false, "a/../b": false, "/etc/passwd": false, "a//b": false, `a\b`: false, "a\x00b": false} {
		if got := ValidFilename(name); got != want {
			t.Errorf("ValidFilename(%q) = %v, want %v", name, got, want)
		}
	}
	if got := escapePath("dir/a b?c#d%e.gguf"); got != "dir/a%20b%3Fc%23d%25e.gguf" {
		t.Errorf("escapePath = %q", got)
	}
}

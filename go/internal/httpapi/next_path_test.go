// SPDX-License-Identifier: Apache-2.0

package httpapi

import "testing"

func TestSanitizeNextPath(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/": "/", "/dashboard": "/dashboard", "/settings?tab=a&x=1": "/settings?tab=a&x=1", "/a/b#frag": "/a/b#frag",
		"//evil.com": "/", "/\\evil.com": "/", "/\\/evil.com": "/", "/\t/evil.com": "/", "/\n/evil.com": "/", "/\r/evil.com": "/",
		"https://evil.com": "/", "http://evil.com/x": "/", "javascript:alert(1)": "/", "evil.com": "/", "/a\x00b": "/", "/a\x7fb": "/",
		"/a\\b": "/", "/@evil.com": "/@evil.com",
	} {
		if got := sanitizeNextPath(in); got != want {
			t.Errorf("sanitizeNextPath(%q) = %q, want %q", in, got, want)
		}
	}
}

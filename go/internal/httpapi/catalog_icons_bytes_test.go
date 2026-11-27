// SPDX-License-Identifier: Apache-2.0

package httpapi

import "testing"

func TestIconBytesMatchDeclaredType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n....")
	jpg := []byte("\xff\xd8\xff\xe0....JFIF")
	webp := []byte("RIFF\x24\x00\x00\x00WEBPVP8 ....")
	webm := []byte("\x1a\x45\xdf\xa3....webm")
	cases := []struct {
		name string
		ct   string
		data []byte
		want bool
	}{
		{"png", "image/png", png, true}, {"jpeg", "image/jpeg", jpg, true}, {"webp", "image/webp", webp, true}, {"webm", "video/webm", webm, true},
		{"html as png", "image/png", []byte("<html><script>alert(1)</script></html>"), false},
		{"png as jpeg", "image/jpeg", png, false},
		{"empty", "image/png", nil, false},
		{"svg clean", "image/svg+xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><circle r="4"/></svg>`), true},
		{"svg script", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), false},
		{"svg onload", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), false},
		{"svg js href", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><rect/></a></svg>`), false},
		{"svg foreignObject", "image/svg+xml", []byte(`<svg><foreignObject><div/></foreignObject></svg>`), false},
		{"html as svg", "image/svg+xml", []byte(`<html><body>hi</body></html>`), false},
		// bypasses a regex denylist misses
		{"svg entity-encoded js href", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="&#106;avascript:alert(1)"><rect/></a></svg>`), false},
		{"svg data href", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a href="data:text/html,x"><rect/></a></svg>`), false},
		{"svg external use", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><use href="https://evil.example/x.svg#a"/></svg>`), false},
		{"svg xlink external use", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="https://evil.example/x.svg#a"/></svg>`), false},
		{"svg smil set", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><a><set attributeName="href" to="javascript:alert(1)"/><rect/></a></svg>`), false},
		{"svg animate", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect><animate attributeName="x" from="0" to="9"/></rect></svg>`), false},
		{"svg image element", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/t.png"/></svg>`), false},
		{"svg style element", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><style>@import url(https://evil.example/x.css);</style></svg>`), false},
		{"svg css url external", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect fill="url(https://evil.example/x)"/></svg>`), false},
		{"svg doctype entity", "image/svg+xml", []byte(`<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"><text>&x;</text></svg>`), false},
		{"svg stylesheet pi", "image/svg+xml", []byte(`<?xml-stylesheet href="x.css"?><svg xmlns="http://www.w3.org/2000/svg"/>`), false},
		{"svg malformed", "image/svg+xml", []byte(`<svg><rect></svg>`), false},
		{"svg local fragment refs ok", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><defs><linearGradient id="g"><stop offset="0" stop-color="#fff"/></linearGradient></defs><use href="#g"/><rect fill="url(#g)" style="stroke:#000;stroke-width:2"/></svg>`), true},
		{"unknown type", "text/html", []byte("<html>"), false},
	}
	for _, c := range cases {
		if got := iconBytesMatch(c.ct, c.data); got != c.want {
			t.Errorf("%s: iconBytesMatch(%s) = %v, want %v", c.name, c.ct, got, c.want)
		}
	}
}

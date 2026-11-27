package httpapi

import "testing"

// The title font (Archivo Black) is a .ttf that Vite emits under /assets; it must not be served as a generic download.
func TestContentTypeForFonts(t *testing.T) {
	for name, want := range map[string]string{
		"ArchivoBlack-Regular-abc123.ttf": "font/ttf",
		"x.woff2":                         "font/woff2",
		"x.woff":                          "font/woff",
		"x.unknown":                       "application/octet-stream",
	} {
		if got := contentTypeFor(name); got != want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", name, got, want)
		}
	}
}

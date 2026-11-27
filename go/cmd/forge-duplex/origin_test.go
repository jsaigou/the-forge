package main

import (
	"net/http/httptest"
	"testing"
)

func TestOriginAllowedAcceptsFullOriginEntries(t *testing.T) {
	for _, entry := range []string{"talk.example.ts.net", "https://talk.example.ts.net", "https://talk.example.ts.net/"} {
		s := &Server{opts: Options{AllowedOrigins: []string{entry}}}
		r := httptest.NewRequest("GET", "http://duplex.example.ts.net/v1/audio/duplex", nil)
		r.Header.Set("Origin", "https://talk.example.ts.net")
		if !s.originAllowed(r) {
			t.Errorf("entry %q should allow its own origin", entry)
		}
		r.Header.Set("Origin", "https://evil.example.com")
		if s.originAllowed(r) {
			t.Errorf("entry %q must not allow another origin", entry)
		}
	}
}

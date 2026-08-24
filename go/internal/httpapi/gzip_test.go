// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGzipCompressesWhenAccepted(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	req := httptest.NewRequest("GET", "/login", nil)
	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, req)
	if plain.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200", plain.Code)
	}
	if enc := plain.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("without Accept-Encoding: Content-Encoding=%q, want empty", enc)
	}

	req = httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login with gzip: got %d, want 200", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding=%q, want gzip", enc)
	}
	if rec.Header().Get("Vary") == "" {
		t.Fatal("missing Vary header on compressed response")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(body, plain.Body.Bytes()) {
		t.Fatal("gunzipped body differs from plain response")
	}
}

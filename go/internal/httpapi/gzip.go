// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// withGzip compresses responses for clients that accept it. Mobile F6:
// forge served the PWA shell and JSON uncompressed, so a fresh phone
// load paid the full raw JS price over tailnet/cellular links. The
// compress decision is deferred until WriteHeader/first Write so the
// handler's Content-Type wins; SSE and binary assets pass through
// untouched, and Flush is forwarded so streaming endpoints keep streaming.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gzw := &gzipResponseWriter{ResponseWriter: w}
		defer gzw.closeGzip()
		next.ServeHTTP(gzw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool
	compress bool
}

// decide must run before the status line is sent — header mutations after
// a forwarded WriteHeader would gzip the body without Content-Encoding.
func (g *gzipResponseWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	if !compressible(g.ResponseWriter.Header().Get("Content-Type")) {
		return
	}
	g.compress = true
	h := g.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	g.decide()
	if g.compress {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) closeGzip() {
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

// compressible reports whether a Content-Type benefits from gzip. The
// event-stream exclusion is load-bearing: SSE payloads are small
// server-pushed frames where gzip framing adds latency for zero savings.
func compressible(ct string) bool {
	if strings.HasPrefix(ct, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(ct, "text/") ||
		strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "application/javascript") ||
		strings.HasPrefix(ct, "application/manifest+json") ||
		strings.HasPrefix(ct, "image/svg+xml")
}

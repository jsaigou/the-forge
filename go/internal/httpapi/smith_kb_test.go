// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
)

func TestSmithKBSearch_ReturnsRankedResults(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/search?q=gtt+ceiling", nil))
	if w.Code != 200 {
		t.Fatalf("kb/search = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp smithKBSearchResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Count == 0 || len(resp.Results) != resp.Count {
		t.Fatalf("count = %d, results = %d, want >0 and equal", resp.Count, len(resp.Results))
	}
	if resp.Results[0].Ref != "pitfalls:gtt-ceiling" {
		t.Errorf("top result ref = %q, want pitfalls:gtt-ceiling", resp.Results[0].Ref)
	}
}

func TestSmithKBSearch_MissingQueryIsValidationError(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/search", nil))
	if w.Code != 422 {
		t.Errorf("kb/search with no q = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestSmithKBSearch_Unwired(t *testing.T) {
	s := newTestServer(t) // no Smith wired
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/search?q=gtt", nil))
	if w.Code != 503 {
		t.Errorf("kb/search unwired = %d, want 503", w.Code)
	}
}

func TestSmithKBRef_ResolvesKnownRef(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/pitfalls:gtt-ceiling", nil))
	if w.Code != 200 {
		t.Fatalf("kb/{ref} = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp smithKBRefResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Chunk.Ref != "pitfalls:gtt-ceiling" || resp.Chunk.Body == "" {
		t.Errorf("chunk = %+v, want a populated pitfalls:gtt-ceiling chunk", resp.Chunk)
	}
}

func TestSmithKBRef_UnknownRefIs404(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/pitfalls:does-not-exist", nil))
	if w.Code != 404 {
		t.Errorf("kb/{unknown-ref} = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestSmithKBRoutes_LiteralsWinOverWildcard confirms /kb/search and
// /kb/blocked resolve to their own handlers rather than falling through to
// the /kb/{ref} wildcard (net/http ServeMux precedence, not something to
// take on faith given all three share one path prefix).
func TestSmithKBRoutes_LiteralsWinOverWildcard(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/search?q=gtt", nil))
	if w.Code != 200 {
		t.Errorf("kb/search = %d, want 200 (not falling through to kb/{ref})", w.Code)
	}
	var search smithKBSearchResponse
	decodeJSON(t, w.Body, &search)

	w = do(t, s, authedRequest("GET", "/api/v1/smith/kb/blocked", nil))
	if w.Code != 200 {
		t.Errorf("kb/blocked = %d, want 200 (not falling through to kb/{ref})", w.Code)
	}
}

func TestSmithKBBlocked_ReturnsItems(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/kb/blocked", nil))
	if w.Code != 200 {
		t.Fatalf("kb/blocked = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp smithKBBlockedResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Count == 0 || len(resp.Items) != resp.Count {
		t.Fatalf("count = %d, items = %d, want >0 and equal", resp.Count, len(resp.Items))
	}
	for _, it := range resp.Items {
		if it.Number == 0 || it.Title == "" || it.Status == "" {
			t.Errorf("item missing required field: %+v", it)
		}
	}
}

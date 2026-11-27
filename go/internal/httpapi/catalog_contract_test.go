// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// viewBodyPairs lists every catalog resource's GET representation and its writable body.
var viewBodyPairs = []struct {
	name string
	view any
	body any
}{
	{"benchmarks", benchmarkJSON{}, benchmarkBody{}},
	{"capability-tiers", capabilityTierJSON{}, capabilityTierBody{}},
	{"configs", configJSON{}, configBody{}},
	{"model-aliases", modelAliasJSON{}, modelAliasBody{}},
	{"models", modelJSON{}, modelBody{}},
	{"notes", noteJSON{}, noteBody{}},
	{"offerings", offeringJSON{}, offeringBody{}},
	{"services", serviceJSON{}, serviceBody{}},
	{"families", familyJSON{}, familyBody{}},
	{"genealogies", genealogyJSON{}, genealogyBody{}},
	{"variants", variantJSON{}, variantBody{}},
	{"virtual-models", virtualModelJSON{}, virtualModelBody{}},
}

// The guard that makes "a GET body is a valid PUT body" true: every key a GET adds on top of the writable body must
// be listed as read-only, and no writable field may be missing from the GET (a PUT round-trip would reset it).
func TestCatalogReadOnlyKeysCoverViews(t *testing.T) {
	for _, p := range viewBodyPairs {
		view, body := jsonFieldNames(reflect.TypeOf(p.view)), jsonFieldNames(reflect.TypeOf(p.body))
		var viewOnly, bodyOnly []string
		for k := range view {
			if !body[k] {
				viewOnly = append(viewOnly, k)
			}
		}
		for k := range body {
			if !view[k] {
				bodyOnly = append(bodyOnly, k)
			}
		}
		sort.Strings(viewOnly)
		sort.Strings(bodyOnly)
		for _, k := range viewOnly {
			if !catalogReadOnlyKeys[k] {
				t.Errorf("%s: GET returns %q, which the body does not accept and catalogReadOnlyKeys does not list: a GET body would be rejected on PUT", p.name, k)
			}
		}
		for _, k := range bodyOnly {
			if k != "reason" { // reason is an audit note, never stored
				t.Errorf("%s: body field %q is writable but not echoed by GET: a GET-then-PUT round-trip would silently reset it", p.name, k)
			}
		}
	}
}

func TestCatalogETagIgnoresProbeFieldsButNotContent(t *testing.T) {
	base := `{"id":1,"name":"a","n_ctx":4096,"chat_template_caps":{"tools":true},"chat_template_caps_probed_at":100}`
	probed := `{"id":1,"name":"a","n_ctx":4096,"chat_template_caps":{"tools":false},"chat_template_caps_probed_at":999}`
	edited := `{"id":1,"name":"a","n_ctx":8192,"chat_template_caps":{"tools":true},"chat_template_caps_probed_at":100}`
	reordered := `{"chat_template_caps_probed_at":100,"n_ctx":4096,"name":"a","id":1,"chat_template_caps":{"tools":true}}`
	e1, ok1 := catalogETag([]byte(base))
	e2, ok2 := catalogETag([]byte(probed))
	e3, ok3 := catalogETag([]byte(edited))
	e4, ok4 := catalogETag([]byte(reordered))
	if !ok1 || !ok2 || !ok3 || !ok4 {
		t.Fatal("catalogETag rejected a valid JSON object")
	}
	if e1 != e2 {
		t.Errorf("a probe update changed the ETag: %s vs %s", e1, e2)
	}
	if e1 == e3 {
		t.Errorf("an n_ctx edit did not change the ETag: %s", e1)
	}
	if e1 != e4 {
		t.Errorf("key order changed the ETag: %s vs %s", e1, e4)
	}
	if !strings.HasPrefix(e1, `W/"`) {
		t.Errorf("ETag %q is not a weak tag", e1)
	}
	if _, ok := catalogETag([]byte(`[1,2]`)); ok {
		t.Error("a non-object body must not get an ETag")
	}
}

func TestMergePatchSemantics(t *testing.T) {
	var target, patch any
	_ = json.Unmarshal([]byte(`{"a":1,"b":{"c":2,"d":3},"e":[1,2,3],"f":"x"}`), &target)
	_ = json.Unmarshal([]byte(`{"b":{"c":null,"z":9},"e":[7],"f":null,"g":true}`), &patch)
	got, _ := json.Marshal(mergePatch(target, patch))
	var want, gotAny any
	_ = json.Unmarshal([]byte(`{"a":1,"b":{"d":3,"z":9},"e":[7],"g":true}`), &want)
	_ = json.Unmarshal(got, &gotAny)
	if !reflect.DeepEqual(want, gotAny) {
		t.Errorf("mergePatch = %s", got)
	}
}

// newTestConfig creates a config through the real POST route and returns its representation and ETag.
func newTestConfig(t *testing.T, s *Server, pq catalogPrereqs, name string) (configJSON, string) {
	t.Helper()
	body := `{"name":"` + name + `","variant_id":` + itoa(pq.variantID) + `,"weight_artifact_id":` + itoa(pq.artifactID) +
		`,"engine_id":` + itoa(pq.engineID) + `,"build_id":` + itoa(pq.buildID) +
		`,"n_ctx":131072,"parallel":1,"extra_args":["--jinja","--parallel","1"]}`
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs", bytes.NewBufferString(body)))
	if w.Code != 201 {
		t.Fatalf("create %s = %d: %s", name, w.Code, w.Body.String())
	}
	var c configJSON
	decodeJSON(t, w.Body, &c)
	g := do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	if g.Code != 200 {
		t.Fatalf("get %s = %d", name, g.Code)
	}
	return c, g.Header().Get("ETag")
}

func withHeader(r *http.Request, k, v string) *http.Request {
	r.Header.Set(k, v)
	return r
}

func TestCatalogGetBodyRoundTripsThroughPut(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	c, etag := newTestConfig(t, s, pq, "roundtrip")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("GET /configs/{id} ETag = %q, want a weak tag", etag)
	}

	for _, tc := range []struct{ name, path string }{
		{"config", "/api/v1/catalog/configs/" + itoa(c.ID)},
		{"model", "/api/v1/catalog/models/" + itoa(pq.modelID)},
		{"variant", "/api/v1/catalog/variants/" + itoa(pq.variantID)},
		{"family", "/api/v1/catalog/families/" + itoa(pq.familyID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := do(t, s, authedRequest("GET", tc.path, nil))
			if g.Code != 200 {
				t.Fatalf("GET = %d: %s", g.Code, g.Body.String())
			}
			before := g.Body.String()
			// The exact GET body, id and all, goes straight back as a PUT.
			p := do(t, s, authedRequest("PUT", tc.path, bytes.NewBufferString(before)))
			if p.Code != 200 {
				t.Fatalf("PUT of the GET body = %d: %s", p.Code, p.Body.String())
			}
			after := do(t, s, authedRequest("GET", tc.path, nil)).Body.String()
			var a, b map[string]any
			_ = json.Unmarshal([]byte(before), &a)
			_ = json.Unmarshal([]byte(after), &b)
			if !reflect.DeepEqual(a, b) {
				t.Errorf("a GET-then-PUT changed the record:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestCatalogPutStillRejectsTyposAndUnknownKeys(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	c, _ := newTestConfig(t, s, pq, "typo")
	g := do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	var m map[string]any
	_ = json.Unmarshal(g.Body.Bytes(), &m)
	m["n_ctxx"] = 1 // a typo is not a read-only key
	b, _ := json.Marshal(m)
	w := do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), bytes.NewReader(b)))
	if w.Code != 422 {
		t.Errorf("PUT with an unknown, non-read-only key = %d, want 422: %s", w.Code, w.Body.String())
	}
}

func TestCatalogPatchChangesOnlyWhatItNames(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	c, _ := newTestConfig(t, s, pq, "patchme")
	path := "/api/v1/catalog/configs/" + itoa(c.ID)

	read := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal(do(t, s, authedRequest("GET", path, nil)).Body.Bytes(), &m)
		return m
	}
	before := read()

	w := do(t, s, authedRequest("PATCH", path, bytes.NewBufferString(`{"visibility":"visible","reason":"patch test"}`)))
	if w.Code != 200 {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") == "" {
		t.Error("PATCH response carries no ETag")
	}
	after := read()
	if after["visibility"] != "visible" {
		t.Errorf("visibility = %v, want visible", after["visibility"])
	}
	for k, v := range before {
		if k == "visibility" {
			continue
		}
		if !reflect.DeepEqual(v, after[k]) {
			t.Errorf("PATCH of visibility also changed %q: %v -> %v", k, v, after[k])
		}
	}

	// Arrays are replaced wholesale (merge-patch semantics), not merged.
	w = do(t, s, authedRequest("PATCH", path, bytes.NewBufferString(`{"extra_args":["--flash-attn","on"]}`)))
	if w.Code != 200 {
		t.Fatalf("PATCH extra_args = %d: %s", w.Code, w.Body.String())
	}
	if got := read()["extra_args"]; !reflect.DeepEqual(got, []any{"--flash-attn", "on"}) {
		t.Errorf("extra_args = %v, want the patch's array only", got)
	}

	// A patch that fails validation is rejected and changes nothing.
	snapshot := read()
	w = do(t, s, authedRequest("PATCH", path, bytes.NewBufferString(`{"visibility":"bogus"}`)))
	if w.Code != 422 {
		t.Errorf("PATCH with an invalid value = %d, want 422: %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(snapshot, read()) {
		t.Error("a rejected PATCH changed the record")
	}

	for _, tc := range []struct {
		name, body, path string
		want             int
	}{
		{"not an object", `[1]`, path, 400},
		{"not json", `nope`, path, 400},
		{"missing record", `{"visibility":"visible"}`, "/api/v1/catalog/configs/999999", 404},
		{"bad id", `{"visibility":"visible"}`, "/api/v1/catalog/configs/abc", 422},
	} {
		w := do(t, s, authedRequest("PATCH", tc.path, bytes.NewBufferString(tc.body)))
		if w.Code != tc.want {
			t.Errorf("PATCH %s = %d, want %d: %s", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestCatalogIfMatch(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	c, etag1 := newTestConfig(t, s, pq, "ifmatch")
	path := "/api/v1/catalog/configs/" + itoa(c.ID)
	get := func() *httptest.ResponseRecorder { return do(t, s, authedRequest("GET", path, nil)) }
	put := func(ifMatch, body string) *httptest.ResponseRecorder {
		r := authedRequest("PUT", path, bytes.NewBufferString(body))
		if ifMatch != "" {
			r = withHeader(r, "If-Match", ifMatch)
		}
		return do(t, s, r)
	}
	edit := func(nctx int) string {
		var m map[string]any
		_ = json.Unmarshal(get().Body.Bytes(), &m)
		m["n_ctx"] = nctx
		b, _ := json.Marshal(m)
		return string(b)
	}

	// A correct If-Match succeeds and the response carries the NEW etag, which equals a fresh GET's.
	w := put(etag1, edit(65536))
	if w.Code != 200 {
		t.Fatalf("PUT with the current ETag = %d: %s", w.Code, w.Body.String())
	}
	etag2 := w.Header().Get("ETag")
	if etag2 == "" || etag2 == etag1 {
		t.Fatalf("ETag after an edit = %q (before %q), want a different one", etag2, etag1)
	}
	if fresh := get().Header().Get("ETag"); fresh != etag2 {
		t.Errorf("PUT response ETag %q != fresh GET ETag %q", etag2, fresh)
	}

	// A stale copy is refused with 412 and the record is untouched.
	w = put(etag1, edit(8192))
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT with a stale ETag = %d, want 412: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "precondition_failed") {
		t.Errorf("412 body lacks the error code: %s", w.Body.String())
	}
	var cur configJSON
	decodeJSON(t, get().Body, &cur)
	if cur.NCtx != 65536 {
		t.Errorf("n_ctx = %d after a refused write, want 65536", cur.NCtx)
	}

	// PATCH honours it too.
	w = do(t, s, withHeader(authedRequest("PATCH", path, bytes.NewBufferString(`{"n_ctx":4096}`)), "If-Match", etag1))
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("PATCH with a stale ETag = %d, want 412", w.Code)
	}
	// "*" matches any existing record; the header is optional by default.
	if w := put("*", edit(32768)); w.Code != 200 {
		t.Errorf("PUT with If-Match: * = %d: %s", w.Code, w.Body.String())
	}
	if w := put("", edit(16384)); w.Code != 200 {
		t.Errorf("PUT without If-Match = %d, want 200 (opt-in): %s", w.Code, w.Body.String())
	}

	// DELETE with a stale ETag is refused and the record survives; with the current one it goes.
	w = do(t, s, withHeader(authedRequest("DELETE", path, nil), "If-Match", etag1))
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("DELETE with a stale ETag = %d, want 412", w.Code)
	}
	if get().Code != 200 {
		t.Fatal("a refused DELETE removed the record")
	}
	fresh := get().Header().Get("ETag")
	if w := do(t, s, withHeader(authedRequest("DELETE", path, nil), "If-Match", fresh)); w.Code != 200 && w.Code != 204 {
		t.Errorf("DELETE with the current ETag = %d: %s", w.Code, w.Body.String())
	}
	if get().Code != 404 {
		t.Error("DELETE with the current ETag did not remove the record")
	}
}

func TestCatalogRequireIfMatchSetting(t *testing.T) {
	s, db := newCatalogTestServer(t)
	s.deps.Settings = db.Settings()
	pq := seedCatalogPrereqs(t, db)
	c, etag := newTestConfig(t, s, pq, "required")
	path := "/api/v1/catalog/configs/" + itoa(c.ID)
	body := do(t, s, authedRequest("GET", path, nil)).Body.String()

	if err := db.Settings().Set(t.Context(), "catalog.require_if_match", []byte("true")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if w := do(t, s, authedRequest("PUT", path, bytes.NewBufferString(body))); w.Code != http.StatusPreconditionRequired {
		t.Errorf("PUT without If-Match while required = %d, want 428: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, withHeader(authedRequest("PUT", path, bytes.NewBufferString(body)), "If-Match", etag)); w.Code != 200 {
		t.Errorf("PUT with If-Match while required = %d: %s", w.Code, w.Body.String())
	}
	if err := db.Settings().Set(t.Context(), "catalog.require_if_match", []byte("false")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if w := do(t, s, authedRequest("PUT", path, bytes.NewBufferString(body))); w.Code != 200 {
		t.Errorf("PUT without If-Match after switching it off = %d: %s", w.Code, w.Body.String())
	}
}

func TestCatalogValidateAnUpdate(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)
	a, _ := newTestConfig(t, s, pq, "alpha")
	_, _ = newTestConfig(t, s, pq, "beta")

	var m map[string]any
	_ = json.Unmarshal(do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(a.ID), nil)).Body.Bytes(), &m)
	own, _ := json.Marshal(m)
	m["name"] = "beta"
	taken, _ := json.Marshal(m)

	// As a create, a record's own name is a duplicate (the old behaviour, kept).
	if w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs/validate", bytes.NewReader(own))); w.Code != 422 {
		t.Errorf("validate-as-create with an existing name = %d, want 422", w.Code)
	}
	// As an update of that record, its own name is fine ...
	if w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs/"+itoa(a.ID)+"/validate", bytes.NewReader(own))); w.Code != 200 {
		t.Errorf("validate-as-update with its own name = %d, want 200: %s", w.Code, w.Body.String())
	}
	// ... but another record's name still is not.
	if w := do(t, s, authedRequest("POST", "/api/v1/catalog/configs/"+itoa(a.ID)+"/validate", bytes.NewReader(taken))); w.Code != 422 {
		t.Errorf("validate-as-update with another record's name = %d, want 422", w.Code)
	}
}

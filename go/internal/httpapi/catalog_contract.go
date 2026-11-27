// SPDX-License-Identifier: Apache-2.0

package httpapi

// Catalog write contract (ADR-0020 findings F2, F3 and F9; docs/api-contract-implementation-plan.md phase 1).
//
//   - A GET body is a valid PUT/POST body: keys the GET representation adds on top of the writable body
//     (id, the probed chat-template caps, timestamps) are ignored on write instead of failing with 422.
//   - PATCH applies an RFC 7396 JSON merge patch to the current record and then goes through the exact PUT path,
//     so there is one validation path and a partial update never resets the fields it does not mention.
//   - GET returns a weak ETag (a hash of the representation without volatile probe fields); PUT, PATCH and DELETE
//     honour If-Match and answer 412 when the record changed since it was read. The header is opt-in unless the
//     setting catalog.require_if_match is true.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// catalogReadOnlyKeys are the keys a catalog GET returns that are not writable. A key is dropped from a write body
// only if it is in this set AND the resource's body struct has no such field, so a resource that does accept `id`
// or `created_at` as input is unaffected. TestCatalogReadOnlyKeysCoverViews keeps this list in step with the view
// structs, and fails if a writable field is ever left out of the GET representation (which would make a PUT
// round-trip lossy).
var catalogReadOnlyKeys = map[string]bool{
	"id":                           true,
	"chat_template_caps":           true,
	"chat_template_caps_probed_at": true,
	"created_at":                   true,
	"updated_at":                   true,
	"load_mode_supported":          true,
	"parallel":                     true, // dropped in phase 5; an older client's body may still carry it
	"missing":                      true,
	"file_size_bytes":              true,
	"gguf_arch":                    true,
	"gguf_trained_ctx":             true,
	"gguf_parameter_count":         true,
	"gguf_quant_type":              true,
}

// catalogETagVolatileKeys change without an operator edit (the engine's post-load probe rewrites them), so they are
// left out of the ETag; otherwise loading a model would make every open edit form stale.
var catalogETagVolatileKeys = []string{"chat_template_caps", "chat_template_caps_probed_at", "load_mode_supported"}

// catalogWriteMu serializes catalog writes that go through this contract so the If-Match check and the write are
// atomic against each other. It does not cover writers outside the API (the registrar, smith, the CLI); for those
// the hash ETag still changes, so a later If-Match fails instead of overwriting silently.
var catalogWriteMu sync.Mutex

var jsonFieldNameCache sync.Map // reflect.Type -> map[string]bool

// jsonFieldNames returns the JSON key names a struct type accepts (struct tags, falling back to the Go name).
func jsonFieldNames(t reflect.Type) map[string]bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if v, ok := jsonFieldNameCache.Load(t); ok {
		return v.(map[string]bool)
	}
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	jsonFieldNameCache.Store(t, out)
	return out
}

// stripReadOnly removes read-only keys that the destination type does not itself accept. Anything that is not a
// JSON object, or that has nothing to strip, is returned untouched so the strict decoder reports it as before.
func stripReadOnly(raw []byte, dst any) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	accepted := jsonFieldNames(reflect.TypeOf(dst))
	removed := false
	for k := range obj {
		if catalogReadOnlyKeys[k] && !accepted[k] {
			delete(obj, k)
			removed = true
		}
	}
	if !removed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// decodeCatalogBody is decodeJSONBody that tolerates the read-only keys a GET returns. Unknown keys that are not
// read-only still fail with 422, so typos are still caught.
func decodeCatalogBody(r *http.Request, dst any) map[string]string {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return decodeJSONBody(r, dst)
	}
	r.Body = io.NopCloser(bytes.NewReader(stripReadOnly(raw, dst)))
	return decodeJSONBody(r, dst)
}

// validateExcludeID is the record id for a validate-an-update request (POST /catalog/<resource>/{id}/validate), so
// the record being edited is not reported as a duplicate of itself. 0 means validate as a create.
func validateExcludeID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// bufWriter captures a handler's response so a wrapper can inspect it, add headers, or drop it.
type bufWriter struct {
	h     http.Header
	code  int
	body  bytes.Buffer
	wrote bool
}

func newBufWriter() *bufWriter { return &bufWriter{h: http.Header{}, code: http.StatusOK} }

func (b *bufWriter) Header() http.Header { return b.h }
func (b *bufWriter) WriteHeader(c int) {
	if !b.wrote {
		b.code, b.wrote = c, true
	}
}
func (b *bufWriter) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}

func (b *bufWriter) flushTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range b.h {
		dst[k] = vs
	}
	w.WriteHeader(b.code)
	_, _ = w.Write(b.body.Bytes())
}

// catalogETag derives the weak ETag of a JSON object body: a hash of its canonical form (sorted keys, numbers kept
// verbatim) without the volatile probe keys.
func catalogETag(body []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil || v == nil {
		return "", false
	}
	for _, k := range catalogETagVolatileKeys {
		delete(v, k)
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(canon)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`, true
}

// ifMatchSatisfied compares an If-Match header value with the current ETag. Our ETags are weak, so (unlike RFC 7232's
// strong comparison) a weak match is accepted; "*" matches any existing record.
func ifMatchSatisfied(header, current string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	cur := strings.TrimPrefix(current, "W/")
	for _, p := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(p), "W/") == cur {
			return true
		}
	}
	return false
}

// catalogGet adds the ETag to a GET-by-id response.
func (s *Server) catalogGet(get http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bw := newBufWriter()
		get(bw, r)
		if bw.code == http.StatusOK {
			if tag, ok := catalogETag(bw.body.Bytes()); ok {
				bw.h.Set("ETag", tag)
			}
		}
		bw.flushTo(w)
	}
}

// catalogRequireIfMatch reads the opt-in setting that turns a missing If-Match into 428.
func (s *Server) catalogRequireIfMatch(ctx context.Context) bool {
	if s.deps.Settings == nil {
		return false
	}
	raw, err := s.deps.Settings.Get(ctx, "catalog.require_if_match")
	if err != nil {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

// catalogWrite wraps a PUT, PATCH or DELETE handler: it enforces If-Match against the current representation
// (fetched with the resource's own GET handler) and puts the new ETag on a successful response.
func (s *Server) catalogWrite(get, write http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		catalogWriteMu.Lock()
		defer catalogWriteMu.Unlock()
		if hdr := r.Header.Get("If-Match"); hdr != "" {
			gw := newBufWriter()
			get(gw, r)
			// A record that does not exist is left to the write handler to report as 404.
			if gw.code == http.StatusOK {
				if cur, ok := catalogETag(gw.body.Bytes()); ok && !ifMatchSatisfied(hdr, cur) {
					writeErrorCode(w, http.StatusPreconditionFailed, "precondition_failed", map[string]any{"etag": cur},
						"the record changed since it was read; fetch it again and retry")
					return
				}
			}
		} else if s.catalogRequireIfMatch(r.Context()) {
			writeErrorCode(w, http.StatusPreconditionRequired, "precondition_required", nil,
				"this server requires an If-Match header on catalog writes")
			return
		}
		ww := newBufWriter()
		write(ww, r)
		if ww.code == http.StatusOK {
			if tag, ok := catalogETag(ww.body.Bytes()); ok {
				ww.h.Set("ETag", tag)
			}
		}
		ww.flushTo(w)
	}
}

// mergePatch applies an RFC 7396 JSON merge patch: objects merge key by key, null deletes a key, and any other
// value (including an array such as extra_args) replaces the target wholesale.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

// catalogPatch turns a merge patch into a full update: read the current record with the resource's GET handler,
// merge, and hand the result to the resource's own PUT handler (validation, audit and cache invalidation included).
func (s *Server) catalogPatch(get, update http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "invalid_patch", nil, "could not read the request body")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var patch map[string]any
		if err := dec.Decode(&patch); err != nil || patch == nil {
			writeErrorCode(w, http.StatusBadRequest, "invalid_patch", nil, "the body must be a JSON object (a merge patch)")
			return
		}
		gw := newBufWriter()
		get(gw, r)
		if gw.code != http.StatusOK {
			gw.flushTo(w) // 404, 400 on a bad id, ...: relay the GET handler's own error
			return
		}
		cdec := json.NewDecoder(&gw.body)
		cdec.UseNumber()
		var cur map[string]any
		if err := cdec.Decode(&cur); err != nil {
			writeInternalError(w, err)
			return
		}
		merged, err := json.Marshal(mergePatch(cur, patch))
		if err != nil {
			writeInternalError(w, err)
			return
		}
		r2 := r.Clone(r.Context())
		r2.Method = http.MethodPut
		r2.Body = io.NopCloser(bytes.NewReader(merged))
		r2.ContentLength = int64(len(merged))
		r2.Header.Set("Content-Type", "application/json")
		update(w, r2)
	}
}

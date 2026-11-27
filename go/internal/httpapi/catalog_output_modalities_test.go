package httpapi

import (
	"bytes"
	"reflect"
	"testing"
)

// TestCatalogOutputModalitiesRoundTrip is the output-side twin of TestCatalogModalitiesRoundTrip (ADR-0021): a field
// that one read or write path forgets is silently dropped, so cover create, get, PUT, merge PATCH and validation for
// both the model-level set and the nullable per-config override.
func TestCatalogOutputModalitiesRoundTrip(t *testing.T) {
	s, db := newCatalogTestServer(t)
	pq := seedCatalogPrereqs(t, db)

	// A model written without output_modalities reads back as ["text"].
	w := do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(`{"name":"PlainModel"}`)))
	if w.Code != 201 {
		t.Fatalf("create plain model = %d: %s", w.Code, w.Body.String())
	}
	var plain modelJSON
	decodeJSON(t, w.Body, &plain)
	if !reflect.DeepEqual(plain.OutputModalities, []string{"text"}) {
		t.Fatalf("default output_modalities = %v, want [text]", plain.OutputModalities)
	}

	// Decision model: create, get, then PUT a different set, then PATCH only the outputs back.
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(`{"name":"D1Model","output_modalities":["decisions"]}`)))
	if w.Code != 201 {
		t.Fatalf("create d1 model = %d: %s", w.Code, w.Body.String())
	}
	var d1 modelJSON
	decodeJSON(t, w.Body, &d1)
	if !reflect.DeepEqual(d1.OutputModalities, []string{"decisions"}) {
		t.Fatalf("created outputs = %v, want [decisions]", d1.OutputModalities)
	}
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/models/"+itoa(d1.ID), nil))
	var got modelJSON
	decodeJSON(t, w.Body, &got)
	if !reflect.DeepEqual(got.OutputModalities, []string{"decisions"}) {
		t.Fatalf("get outputs = %v", got.OutputModalities)
	}
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/models/"+itoa(d1.ID), bytes.NewBufferString(`{"name":"D1Model","output_modalities":["text","audio"]}`)))
	if w.Code != 200 {
		t.Fatalf("put model = %d: %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &got)
	if !reflect.DeepEqual(got.OutputModalities, []string{"text", "audio"}) {
		t.Fatalf("after PUT outputs = %v", got.OutputModalities)
	}
	w = do(t, s, authedRequest("PATCH", "/api/v1/catalog/models/"+itoa(d1.ID), bytes.NewBufferString(`{"output_modalities":["decisions"]}`)))
	if w.Code != 200 {
		t.Fatalf("patch model = %d: %s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &got)
	if !reflect.DeepEqual(got.OutputModalities, []string{"decisions"}) || got.Name != "D1Model" {
		t.Fatalf("after PATCH = %+v (the merge must keep the other fields)", got)
	}

	// Unknown output value is a 422 naming the field.
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(`{"name":"Bad","output_modalities":["text","telepathy"]}`)))
	if w.Code != 422 {
		t.Fatalf("unknown output = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, w.Body, &resp)
	if _, ok := resp["fields"].(map[string]any)["output_modalities"]; !ok {
		t.Errorf("expected an output_modalities field error, got %v", resp)
	}
	// "vision" is an input value, not an output one.
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/models", bytes.NewBufferString(`{"name":"Bad2","output_modalities":["vision"]}`)))
	if w.Code != 422 {
		t.Fatalf("input value as output = %d, want 422", w.Code)
	}

	// Config: derive mode omits the key entirely; an explicit override round-trips; null puts it back to derive.
	cfgBody := func(extra string) *bytes.Buffer {
		return bytes.NewBufferString(`{"name":"d1-cfg","variant_id":` + itoa(pq.variantID) +
			`,"weight_artifact_id":` + itoa(pq.artifactID) + `,"engine_id":` + itoa(pq.engineID) +
			`,"build_id":` + itoa(pq.buildID) + extra + `}`)
	}
	w = do(t, s, authedRequest("POST", "/api/v1/catalog/configs", cfgBody("")))
	if w.Code != 201 {
		t.Fatalf("create config = %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"output_modalities"`)) {
		t.Errorf("derive-mode config body should omit output_modalities, got: %s", w.Body.String())
	}
	var c configJSON
	decodeJSON(t, w.Body, &c)
	if c.OutputModalities != nil {
		t.Fatalf("derive-mode config OutputModalities = %v, want nil", *c.OutputModalities)
	}
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), cfgBody(`,"output_modalities":["decisions"]`)))
	if w.Code != 200 {
		t.Fatalf("put config = %d: %s", w.Code, w.Body.String())
	}
	var c2 configJSON
	decodeJSON(t, w.Body, &c2)
	if c2.OutputModalities == nil || !reflect.DeepEqual(*c2.OutputModalities, []string{"decisions"}) {
		t.Fatalf("overridden config outputs = %v", c2.OutputModalities)
	}
	w = do(t, s, authedRequest("GET", "/api/v1/catalog/configs/"+itoa(c.ID), nil))
	var c3 configJSON
	decodeJSON(t, w.Body, &c3)
	if c3.OutputModalities == nil || !reflect.DeepEqual(*c3.OutputModalities, []string{"decisions"}) {
		t.Fatalf("get config after override = %v (dead-column regression)", c3.OutputModalities)
	}
	w = do(t, s, authedRequest("PATCH", "/api/v1/catalog/configs/"+itoa(c.ID), bytes.NewBufferString(`{"output_modalities":null}`)))
	if w.Code != 200 {
		t.Fatalf("patch null = %d: %s", w.Code, w.Body.String())
	}
	var c4 configJSON
	decodeJSON(t, w.Body, &c4)
	if c4.OutputModalities != nil {
		t.Fatalf("null patch should return to derive mode, got %v", *c4.OutputModalities)
	}
	w = do(t, s, authedRequest("PUT", "/api/v1/catalog/configs/"+itoa(c.ID), cfgBody(`,"output_modalities":["telepathy"]`)))
	if w.Code != 422 {
		t.Fatalf("unknown output on config = %d, want 422", w.Code)
	}
}

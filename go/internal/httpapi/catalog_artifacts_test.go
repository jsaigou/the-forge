// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// minimalGGUF is a valid GGUF header with the metadata the catalog reads and no tensors.
func minimalGGUF() []byte {
	var kv bytes.Buffer
	w := func(v any) { _ = binary.Write(&kv, binary.LittleEndian, v) }
	str := func(s string) { w(uint64(len(s))); kv.WriteString(s) }
	str("general.architecture")
	w(uint32(8))
	str("qwen3")
	str("general.file_type")
	w(uint32(4))
	w(uint32(18)) // Q6_K
	str("general.parameter_count")
	w(uint32(10))
	w(uint64(8_190_000_000))
	str("qwen3.context_length")
	w(uint32(4))
	w(uint32(262144))
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(0x46554747))
	_ = binary.Write(&out, binary.LittleEndian, uint32(3))
	_ = binary.Write(&out, binary.LittleEndian, uint64(0))
	_ = binary.Write(&out, binary.LittleEndian, uint64(4))
	out.Write(kv.Bytes())
	return out.Bytes()
}

type artifactsFixture struct {
	*buildsFixture
	models string
}

func newArtifactsFixture(t *testing.T) *artifactsFixture {
	t.Helper()
	f := &artifactsFixture{buildsFixture: newBuildsFixture(t), models: t.TempDir()}
	cfg, _ := config.New(config.Config{Paths: config.Paths{ModelsDir: f.models}})
	f.s.deps.Config = func() *config.Config { return cfg }
	return f
}

func (f *artifactsFixture) file(t *testing.T, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(f.models, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *artifactsFixture) body(path, typ string) string {
	b, _ := json.Marshal(map[string]any{"variant_id": f.pre.variantID, "format_id": f.pre.formatID,
		"quantization_id": f.pre.quantID, "file_path": path, "artifact_type": typ})
	return string(b)
}

func TestArtifactCreateReadsTheFileAndGuardsThePath(t *testing.T) {
	f := newArtifactsFixture(t)
	f.file(t, "qwen/q6.gguf", minimalGGUF())
	f.file(t, "junk/not.gguf", []byte("this is not a gguf file at all"))

	code, out := f.send(t, "POST", "/api/v1/catalog/artifacts", f.body("qwen/q6.gguf", "weight"))
	if code != 201 || out["gguf_arch"] != "qwen3" || out["gguf_trained_ctx"] != float64(262144) ||
		out["gguf_parameter_count"] != "8190000000" || out["file_size_bytes"] == float64(0) || out["missing"] != false {
		t.Fatalf("create: %d %v, want the file's own facts", code, out)
	}
	// The request cannot override what the file says.
	b, _ := json.Marshal(map[string]any{"variant_id": f.pre.variantID, "format_id": f.pre.formatID, "file_path": "qwen/q6.gguf",
		"artifact_type": "mmproj", "gguf_arch": "llama", "file_size_bytes": 1})
	if code, _ := f.send(t, "POST", "/api/v1/catalog/artifacts", string(b)); code != 201 {
		t.Errorf("a GET-shaped body with read-only keys was refused: %d", code)
	}

	// A perfectly valid GGUF that lives outside the models directory must still be refused: only the location is wrong.
	outside := filepath.Join(t.TempDir(), "valid.gguf")
	if err := os.WriteFile(outside, minimalGGUF(), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(f.models, outside)
	cases := []struct{ name, path, typ, field string }{
		{"valid GGUF outside, absolute", outside, "weight", "file_path"},
		{"valid GGUF outside, via ..", rel, "weight", "file_path"},
		{"escapes the models dir", "../outside.gguf", "weight", "file_path"},
		{"absolute outside", "/etc/passwd", "weight", "file_path"},
		{"missing file", "qwen/nope.gguf", "weight", "file_path"},
		{"not a GGUF", "junk/not.gguf", "weight", "file_path"},
		{"a directory as GGUF", "qwen", "weight", "file_path"},
		{"bad type", "qwen/q6.gguf", "adapter", "artifact_type"},
		{"duplicate file for the variant", "qwen/q6.gguf", "weight", "file_path"},
	}
	for _, c := range cases {
		code, body := f.send(t, "POST", "/api/v1/catalog/artifacts", f.body(c.path, c.typ))
		fields, _ := body["fields"].(map[string]any)
		if code != 422 || fields[c.field] == nil {
			t.Errorf("%s: %d %v, want 422 on %s", c.name, code, body, c.field)
		}
	}
}

func TestArtifactDeleteIsRefusedWhileAConfigUsesIt(t *testing.T) {
	f := newArtifactsFixture(t)
	f.file(t, "a.gguf", minimalGGUF())
	code, art := f.send(t, "POST", "/api/v1/catalog/artifacts", f.body("a.gguf", "weight"))
	if code != 201 {
		t.Fatal(code, art)
	}
	aid := int64(art["id"].(float64))
	if _, err := f.db.Catalog().CreateConfig(context.Background(), store.Config{
		Name: "uses-a", VariantID: f.pre.variantID, WeightArtifactID: aid, EngineID: f.engine,
	}); err != nil {
		t.Fatal(err)
	}
	code, body := f.send(t, "DELETE", "/api/v1/catalog/artifacts/"+idOf(art), "")
	if code != 409 || body["code"] != "artifact_in_use" || !strings.Contains(body["error"].(string), "uses-a") {
		t.Fatalf("delete while referenced: %d %v", code, body)
	}
	if _, err := f.db.Catalog().GetArtifact(context.Background(), aid); err != nil {
		t.Errorf("a refused delete removed the row: %v", err)
	}
	// An unreferenced one deletes, and the file stays.
	f.file(t, "b.gguf", minimalGGUF())
	_, b := f.send(t, "POST", "/api/v1/catalog/artifacts", f.body("b.gguf", "weight"))
	if code, _ := f.send(t, "DELETE", "/api/v1/catalog/artifacts/"+idOf(b), ""); code != 200 {
		t.Errorf("delete of an unused artifact = %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.models, "b.gguf")); err != nil {
		t.Errorf("the file on disk was touched: %v", err)
	}
}

func TestArtifactGetBodyRoundTripsAndPatchRereadsTheFile(t *testing.T) {
	f := newArtifactsFixture(t)
	f.file(t, "a.gguf", minimalGGUF())
	_, art := f.send(t, "POST", "/api/v1/catalog/artifacts", f.body("a.gguf", "weight"))
	w := do(t, f.s, authedRequest("GET", "/api/v1/catalog/artifacts/"+idOf(art), nil))
	if w.Code != 200 || w.Header().Get("ETag") == "" {
		t.Fatalf("GET: %d", w.Code)
	}
	if code, body := f.send(t, "PUT", "/api/v1/catalog/artifacts/"+idOf(art), w.Body.String()); code != 200 {
		t.Fatalf("PUT of the GET body: %d %v", code, body)
	}
	// PATCH one field; the rest (and the file facts) survive.
	code, body := f.send(t, "PATCH", "/api/v1/catalog/artifacts/"+idOf(art), `{"sha256":"abc123"}`)
	if code != 200 || body["sha256"] != "abc123" || body["gguf_arch"] != "qwen3" {
		t.Errorf("PATCH: %d %v", code, body)
	}
	// Replace the file with junk: the next write notices.
	f.file(t, "a.gguf", []byte("junk"))
	if code, _ := f.send(t, "PATCH", "/api/v1/catalog/artifacts/"+idOf(art), `{"sha256":"def"}`); code != 422 {
		t.Errorf("PATCH after the file stopped being a GGUF = %d, want 422", code)
	}
}

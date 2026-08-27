package tts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRegistryRejectsPathTraversalIDs guards a real vulnerability: every
// method here used to join id straight into a filename (id+".wav",
// id+"_sample.wav") with no validation, so an id like
// "../../../../tmp/evil" could read, write, or delete files outside
// AudioDir — reachable even for an id that was never legitimately created
// via Put, since GetSample/SetSample/Delete never checked registry
// membership either.
func TestRegistryRejectsPathTraversalIDs(t *testing.T) {
	dir, _ := os.MkdirTemp("", "reg-traversal-")
	defer os.RemoveAll(dir)
	reg := NewRegistry(dir)

	outside := filepath.Join(filepath.Dir(dir), "escaped_sample.wav")
	defer os.Remove(outside)
	traversalID := "../" + filepath.Base(filepath.Dir(dir)) + "/escaped"

	if err := reg.SetSample(traversalID, []byte("payload")); !errors.Is(err, ErrInvalidVoiceID) {
		t.Fatalf("SetSample(%q) = %v, want ErrInvalidVoiceID", traversalID, err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("SetSample wrote outside AudioDir despite rejection")
	}

	if _, err := reg.GetSample(traversalID); !errors.Is(err, ErrInvalidVoiceID) {
		t.Fatalf("GetSample(%q) = %v, want ErrInvalidVoiceID", traversalID, err)
	}

	if err := reg.Delete(traversalID); !errors.Is(err, ErrInvalidVoiceID) {
		t.Fatalf("Delete(%q) = %v, want ErrInvalidVoiceID", traversalID, err)
	}

	if err := reg.Put(VoiceEntry{ID: traversalID, Name: "x", Type: "design"}); !errors.Is(err, ErrInvalidVoiceID) {
		t.Fatalf("Put(id=%q) = %v, want ErrInvalidVoiceID", traversalID, err)
	}

	// A normal id must still work end to end.
	if err := reg.SetSample("legit-voice_1", []byte("wav-bytes")); err != nil {
		t.Fatalf("SetSample(legit id) unexpectedly failed: %v", err)
	}
	got, err := reg.GetSample("legit-voice_1")
	if err != nil || string(got) != "wav-bytes" {
		t.Fatalf("GetSample(legit id) = %q, %v", got, err)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	dir, _ := os.MkdirTemp("", "reg-")
	defer os.RemoveAll(dir)

	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, err := reg.List(); err != nil || len(got) != 0 {
		t.Fatalf("empty registry expected, got %d err %v", len(got), err)
	}

	v := VoiceEntry{ID: "v1", Name: "Test", Type: "design", Language: "English", Design: &DesignSpec{Instruct: "warm"}}
	if err := reg.Put(v); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := reg.Get("v1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Design.Instruct != "warm" {
		t.Fatalf("instruct mismatch: %q", got.Design.Instruct)
	}

	list, err := reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}

	if err := reg.Delete("v1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := reg.Get("v1"); ok {
		t.Fatalf("expected deleted")
	}
}

func TestRegistryPersistsAcrossLoads(t *testing.T) {
	dir, _ := os.MkdirTemp("", "reg-")
	defer os.RemoveAll(dir)

	reg := NewRegistry(dir)
	_ = reg.Put(VoiceEntry{ID: "persist", Name: "P", Type: "clone", Language: "English", Clone: &CloneSpec{RefAudio: "persist.wav", RefText: "hi"}})

	reg2 := NewRegistry(dir)
	if err := reg2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok, _ := reg2.Get("persist"); !ok {
		t.Fatalf("entry not persisted")
	}
	if _, err := os.Stat(filepath.Join(dir, "voices.json")); err != nil {
		t.Fatalf("voices.json missing: %v", err)
	}
}

func TestRegistrySamples(t *testing.T) {
	dir, _ := os.MkdirTemp("", "reg-")
	defer os.RemoveAll(dir)
	reg := NewRegistry(dir)

	if err := reg.SetSample("v1", []byte("wavedata")); err != nil {
		t.Fatalf("setsample: %v", err)
	}
	got, err := reg.GetSample("v1")
	if err != nil {
		t.Fatalf("getsample: %v", err)
	}
	if string(got) != "wavedata" {
		t.Fatalf("sample mismatch")
	}
	if _, err := reg.GetSample("missing"); err == nil {
		t.Fatalf("expected error for missing sample")
	}
}

func TestRegistryWrappedFormat(t *testing.T) {
	dir, _ := os.MkdirTemp("", "reg-")
	defer os.RemoveAll(dir)
	wrapped := `{"voices":[{"id":"bmo","name":"BMO","type":"clone","language":"English","clone":{"ref_audio":"bmo.wav","ref_text":"hi","x_vector_only":false},"sample_text":"Beemo chop."}]}`
	if err := os.WriteFile(filepath.Join(dir, "voices.json"), []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	if err := reg.Load(); err != nil {
		t.Fatalf("load wrapped: %v", err)
	}
	v, ok, err := reg.Get("bmo")
	if err != nil || !ok {
		t.Fatalf("get bmo: ok=%v err=%v", ok, err)
	}
	if v.SampleText != "Beemo chop." {
		t.Fatalf("sample_text not preserved: %q", v.SampleText)
	}
	if v.Clone == nil || v.Clone.RefText != "hi" {
		t.Fatalf("clone spec wrong: %+v", v.Clone)
	}

	// mutating + flushing must keep the {"voices":[...]} wrapper and preserve sample_text
	if err := reg.Put(VoiceEntry{ID: "new", Name: "New", Type: "design", Language: "English", Design: &DesignSpec{Instruct: "warm"}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "voices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var check struct {
		Voices []VoiceEntry `json:"voices"`
	}
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("rewritten file is not wrapped: %v (%s)", err, raw)
	}
	foundBMO, foundNew := false, false
	for _, e := range check.Voices {
		if e.ID == "bmo" {
			foundBMO = true
			if e.SampleText != "Beemo chop." {
				t.Fatalf("bmo sample_text lost on rewrite: %q", e.SampleText)
			}
		}
		if e.ID == "new" {
			foundNew = true
		}
	}
	if !foundBMO || !foundNew {
		t.Fatalf("rewritten voices missing entries: bmo=%v new=%v", foundBMO, foundNew)
	}
}

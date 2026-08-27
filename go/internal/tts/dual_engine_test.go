package tts

import (
	"context"
	"strings"
	"testing"
)

// TestDualEngineNilKokoro: Sprint 2 (Voice & Speech settings) lets the
// operator disable the Kokoro engine entirely, which is wired by simply
// never constructing a kokoroBackend and passing nil into NewDualEngine.
// Every method must degrade gracefully instead of nil-dereferencing.
func TestDualEngineNilKokoro(t *testing.T) {
	fb := &fakeBackend{}
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	qwen := NewQwenTTS(fb, reg, nil)
	dual := NewDualEngine(qwen, nil, "billie", "af_heart")

	ctx := context.Background()

	if got := dual.Models(); len(got) != 1 || got[0].ID == "kokoro-82m" {
		t.Fatalf("Models() with nil kokoro = %+v, want just the qwen model, no kokoro-82m", got)
	}

	voices, err := dual.ListVoices(ctx)
	if err != nil {
		t.Fatalf("ListVoices: %v", err)
	}
	for _, v := range voices {
		if v.Tier == "fast" {
			t.Fatalf("ListVoices with nil kokoro returned a fast-tier voice: %+v", v)
		}
	}

	health, err := dual.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if _, ok := health["kokoro"]; ok {
		t.Fatalf("Health with nil kokoro should omit the kokoro key, got %+v", health)
	}

	if _, ok, err := dual.GetVoice(ctx, "af_heart"); err != nil || ok {
		t.Fatalf("GetVoice(%q) with nil kokoro = ok=%v err=%v, want not-found (falls through to qwen's registry, which doesn't have it either)", "af_heart", ok, err)
	}

	// Speech explicitly requesting kokoro must not panic, and must fail
	// clearly rather than silently serving premium instead.
	_, err = dual.Speech(ctx, SpeechRequest{Model: "kokoro", Voice: "af_heart", Input: "hi"})
	if err == nil {
		t.Fatalf("Speech(model=kokoro) with nil kokoro should error, got success")
	}
}

func TestDualEngineDisabledMode(t *testing.T) {
	fb := &fakeBackend{}
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	reg.Put(VoiceEntry{ID: "bmo", Name: "Bmo", Type: "customvoice"})
	qwen := NewQwenTTS(fb, reg, map[VoiceMode]bool{ModeVoiceDesign: true})
	dual := NewDualEngine(qwen, nil, "billie", "af_heart")

	if _, err := dual.Speech(context.Background(), SpeechRequest{Voice: "billie", Input: "hi"}); err == nil {
		t.Fatalf("Speech for a disabled mode (voicedesign) should be refused, got success")
	} else if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %v, want it to mention the mode is disabled", err)
	}

	// A different, non-disabled mode must still work.
	if _, err := dual.Speech(context.Background(), SpeechRequest{Voice: "bmo", Input: "hi"}); err != nil {
		t.Fatalf("Speech for an enabled mode (customvoice) should succeed, got: %v", err)
	}
}

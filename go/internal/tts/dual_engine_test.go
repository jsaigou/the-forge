package tts

import (
	"context"
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
	qwen := NewQwenTTS(fb, reg)
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

// modeGateBackend refuses the modes in refuse the way the daemon does for an engine that is turned off (a hard EngineError), and serves the others.
type modeGateBackend struct {
	fakeBackend
	refuse map[VoiceMode]bool
}

func (b *modeGateBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	if b.refuse[spec.Mode] {
		return nil, &EngineError{Code: "speech_engine_disabled", Message: "engine for " + string(spec.Mode) + " is turned off"}
	}
	return b.fakeBackend.Speech(ctx, spec, text)
}

func TestDualEngineEngineTurnedOffByTheDaemon(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	reg.Put(VoiceEntry{ID: "kit", Name: "Kit", Type: "customvoice"})
	dual := NewDualEngine(NewQwenTTS(&modeGateBackend{refuse: map[VoiceMode]bool{ModeVoiceDesign: true}}, reg), nil, "billie", "af_heart")

	_, err := dual.Speech(context.Background(), SpeechRequest{Voice: "billie", Input: "hi"})
	if ee, ok := IsEngineError(err); !ok || ee.Code != "speech_engine_disabled" || ee.Transient {
		t.Fatalf("Speech for a mode whose engine is turned off = %v, want the daemon's hard refusal", err)
	}
	// A different mode must still work.
	if _, err := dual.Speech(context.Background(), SpeechRequest{Voice: "kit", Input: "hi"}); err != nil {
		t.Fatalf("Speech for another mode should succeed, got: %v", err)
	}
}

package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKokoroLangCodeFromVoice(t *testing.T) {
	cases := map[string]string{
		"af_heart":      "en",
		"am_michael":    "en",
		"bf_emma":       "en",
		"bm_daniel":     "en",
		"ef_dora":       "es",
		"ff_siwis":      "fr",
		"hf_alpha":      "hi",
		"if_sara":       "it",
		"jf_alpha":      "ja",
		"jm_kumo":       "ja",
		"jf_gongitsune": "ja",
		"pf_dora":       "pt",
		"zf_xiaobei":    "zh",
		// Outside the Kokoro namespace: never invent a language.
		"billie": "",
		"reina":    "",
		"":       "",
	}
	for voice, want := range cases {
		if got := kokoroLangCodeFromVoice(voice); got != want {
			t.Errorf("kokoroLangCodeFromVoice(%q) = %q, want %q", voice, got, want)
		}
	}
}

// TestKokoroSpeechSendsLanguageFromVoice pins the regression where a Japanese
// voice with no language on the request was sent to the Kokoro service with an
// empty language, which that service treats as English (wrong phonemes, then an
// HTTP 500 past 510 phonemes). Go through the dual engine, as a0's
// /v1/audio/speech does, so the whole path is covered.
func TestKokoroSpeechSendsLanguageFromVoice(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("WAV"))
	}))
	defer ts.Close()

	dual := NewDualEngine(NewQwenTTS(&fakeBackend{}, NewRegistry(t.TempDir())), staticKokoroBackend(ts.URL, ""), "billie", "af_heart")

	cases := []struct {
		name     string
		req      SpeechRequest
		wantLang string
	}{
		{"japanese voice, no language", SpeechRequest{Voice: "jf_alpha", Input: "こんにちは"}, "ja"},
		{"english voice, no language", SpeechRequest{Voice: "af_heart", Input: "hello"}, "en"},
		{"explicit language wins over the voice", SpeechRequest{Voice: "jf_alpha", Language: "en", Input: "hello"}, "en"},
		{"whitespace-only language is treated as unset", SpeechRequest{Voice: "jm_kumo", Language: "  ", Input: "こんにちは"}, "ja"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := dual.Speech(context.Background(), c.req); err != nil {
				t.Fatalf("speech: %v", err)
			}
			if got["language"] != c.wantLang {
				t.Fatalf("language sent to kokoro = %v, want %q (payload %v)", got["language"], c.wantLang, got)
			}
		})
	}
}

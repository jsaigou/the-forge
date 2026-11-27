package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// kokoroBackend proxies the resident Kokoro-FastAPI service (ONNX Runtime, CPU)
// that fronts Kokoro-82M. It is the "fast" tier: always loaded, low-latency,
// CPU-only. Voice IDs follow the Kokoro namespace (e.g. af_heart, am_michael),
// which the dual engine routes here; everything else goes to the Qwen backend.
type kokoroBackend struct {
	client   *http.Client
	resolver Resolver
	token    string

	// Listing voices has to resolve (and so possibly start) Kokoro; the 54 built-in voices hardly ever change, so the list is
	// kept for a while and a polling caller does not reload or touch the engine on every read. A stale list is served if Kokoro cannot be reached.
	listMu    sync.Mutex
	listCache []VoiceEntry
	listAt    time.Time
}

const kokoroListCacheFor = 10 * time.Minute

// kokoroVoiceID matches Kokoro voice ids: a language code (single lowercase
// letter) followed by a gender (m/f) and an underscore, e.g. af_heart, jm_kumo.
// Registry (Qwen) voice ids do not use this shape, so this cleanly separates
// the two namespaces.
var kokoroVoiceID = regexp.MustCompile(`^[a-z][mf]_`)

// IsKokoroVoice reports whether id belongs to the Kokoro (fast) namespace.
func IsKokoroVoice(id string) bool {
	return kokoroVoiceID.MatchString(id)
}

// kokoroLangFromVoice maps a Kokoro voice id to a human language label for the
// voice listing. The leading letter encodes the language.
func kokoroLangFromVoice(id string) string {
	if id == "" {
		return "Unknown"
	}
	langs := map[string]string{
		"a": "English (US)", "b": "English (UK)", "e": "Spanish",
		"f": "French", "h": "Hindi", "i": "Italian",
		"j": "Japanese", "p": "Portuguese (BR)", "z": "Chinese",
	}
	if l, ok := langs[id[:1]]; ok {
		return l
	}
	return "Unknown"
}

// kokoroLangCodeFromVoice maps a Kokoro voice id to the language code the
// Kokoro service understands (its alias table: en, es, fr, hi, it, ja, pt, zh).
// The leading letter of a voice id encodes its language. An id outside the
// Kokoro namespace returns "" so the caller does not invent a language.
//
// Why it exists: the Kokoro service picks its G2P purely from the request's
// "language" field and falls back to English when it is empty. An OpenAI-shaped
// /v1/audio/speech request carries no language, so every jf_*/jm_* voice ran
// English phonemization over Japanese text: wrong audio (a 2 s sentence came
// back as 14.6 s of letter names) and, past the 510-phoneme limit, an HTTP 500.
func kokoroLangCodeFromVoice(id string) string {
	if !IsKokoroVoice(id) {
		return ""
	}
	switch id[0] {
	case 'a', 'b':
		return "en"
	case 'e':
		return "es"
	case 'f':
		return "fr"
	case 'h':
		return "hi"
	case 'i':
		return "it"
	case 'j':
		return "ja"
	case 'p':
		return "pt"
	case 'z':
		return "zh"
	}
	return ""
}

// NewKokoroBackend finds Kokoro through resolver (the daemon, which starts it on demand).
func NewKokoroBackend(resolver Resolver, token string) *kokoroBackend {
	return &kokoroBackend{client: &http.Client{Timeout: 5 * time.Minute}, resolver: resolver, token: token}
}

// address finds Kokoro for one request; the caller must call release when it is done.
func (b *kokoroBackend) address(ctx context.Context) (string, func(), error) {
	res, err := b.resolver.Resolve(ctx, ModeKokoro, "")
	if err != nil {
		return "", func() {}, err
	}
	return res.URL, res.Release, nil
}

func (b *kokoroBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	base, release, err := b.address(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	voice := spec.ID
	if voice == "" {
		voice = spec.Speaker
	}
	speed := spec.Speed
	if speed == 0 {
		speed = 1.0
	}
	// An explicit language from the caller always wins; otherwise take it
	// from the voice so a Japanese voice is never phonemized as English.
	language := strings.TrimSpace(spec.Language)
	if language == "" {
		language = kokoroLangCodeFromVoice(voice)
	}
	reqBody := map[string]any{
		"model":           "kokoro",
		"voice":           voice,
		"input":           text,
		"speed":           speed,
		"response_format": "wav",
		"language":        language,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		if ctx.Err() == nil {
			return nil, &EngineError{Code: "model_not_ready", Transient: true, RetryAfter: 2 * time.Second, Message: "the kokoro engine did not answer: " + err.Error()}
		}
		return nil, fmt.Errorf("kokoro speech: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kokoro speech: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return &SpeechResult{Audio: data, Format: "wav"}, nil
}

func (b *kokoroBackend) Health(ctx context.Context) (map[string]any, error) {
	// Asking the daemon for Kokoro's address may start it: health only reports what the resolver knows.
	return map[string]any{"status": "ok", "engine": "kokoro (resolved per request by the daemon)"}, nil
}

func (b *kokoroBackend) Close() error { return nil }

// ListVoices returns the Kokoro voices available from the resident service,
// each tagged tier=fast so the dual engine can merge them with the Qwen
// registry voices.
func (b *kokoroBackend) ListVoices(ctx context.Context) ([]VoiceEntry, error) {
	b.listMu.Lock()
	defer b.listMu.Unlock()
	if b.listCache != nil && time.Since(b.listAt) < kokoroListCacheFor {
		return append([]VoiceEntry(nil), b.listCache...), nil
	}
	voices, err := b.fetchVoices(ctx)
	if err != nil {
		if b.listCache != nil {
			return append([]VoiceEntry(nil), b.listCache...), nil // stale beats missing
		}
		return nil, err
	}
	b.listCache, b.listAt = voices, time.Now()
	return append([]VoiceEntry(nil), voices...), nil
}

func (b *kokoroBackend) fetchVoices(ctx context.Context) ([]VoiceEntry, error) {
	base, release, err := b.address(ctx)
	if err != nil {
		return nil, fmt.Errorf("kokoro list voices: %w", err)
	}
	defer release()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/audio/voices", nil)
	if err != nil {
		return nil, err
	}
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kokoro list voices: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kokoro list voices: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("kokoro list voices decode: %w", err)
	}
	out := make([]VoiceEntry, 0, len(parsed.Data))
	for _, v := range parsed.Data {
		id := v.ID
		out = append(out, VoiceEntry{
			ID:       id,
			Name:     id,
			Type:     "kokoro",
			Language: kokoroLangFromVoice(id),
			Tier:     "fast",
		})
	}
	return out, nil
}

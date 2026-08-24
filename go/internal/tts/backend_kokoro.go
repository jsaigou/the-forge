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
	"time"
)

// kokoroBackend proxies the resident Kokoro-FastAPI service (ONNX Runtime, CPU)
// that fronts Kokoro-82M. It is the "fast" tier: always loaded, low-latency,
// CPU-only. Voice IDs follow the Kokoro namespace (e.g. af_heart, am_michael),
// which the dual engine routes here; everything else goes to the Qwen backend.
type kokoroBackend struct {
	client *http.Client
	url    string
	token  string
}

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

func NewKokoroBackend(url, token string) *kokoroBackend {
	if url == "" {
		url = "http://127.0.0.1:8880"
	}
	return &kokoroBackend{
		client: &http.Client{Timeout: 5 * time.Minute},
		url:    strings.TrimRight(url, "/"),
		token:  token,
	}
}

func (b *kokoroBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	voice := spec.ID
	if voice == "" {
		voice = spec.Speaker
	}
	speed := spec.Speed
	if speed == 0 {
		speed = 1.0
	}
	reqBody := map[string]any{
		"model":           "kokoro",
		"voice":           voice,
		"input":           text,
		"speed":           speed,
		"response_format": "wav",
		"language":        spec.Language,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+"/health", nil)
	if err != nil {
		return nil, err
	}
	if b.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kokoro health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kokoro health: %s", resp.Status)
	}
	return map[string]any{"status": "ok", "engine": "kokoro", "backend_url": b.url}, nil
}

func (b *kokoroBackend) Close() error { return nil }

// ListVoices returns the Kokoro voices available from the resident service,
// each tagged tier=fast so the dual engine can merge them with the Qwen
// registry voices.
func (b *kokoroBackend) ListVoices(ctx context.Context) ([]VoiceEntry, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+"/v1/audio/voices", nil)
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

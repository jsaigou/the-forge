package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// serverBackend proxies a resident tts-server process (qwentts.cpp/build/tts-server),
// which keeps one model loaded in GPU RAM for its lifetime and exposes an
// OpenAI-compatible API plus in-memory voice registration. One tts-server
// instance is expected per model variant; the backend routes by SynthSpec.Mode.
//
// tts-server API (src/tts-server.h):
//   POST /v1/audio/speech  {input, voice, instructions, response_format}
//   POST /v1/audio/voices  {name, ref_text, wav_b64}  (register/replace clone)
//   GET  /health
type serverBackend struct {
	client    *http.Client
	customURL string
	designURL string
	baseURL   string
	fallback  Backend
}

// NewServerBackend routes synthesis to resident tts-server instances, one per
// model variant. Any mode without a configured URL (or whose server is
// unreachable) falls back to fallback, so a partial deployment (e.g. only the
// customvoice server running) still serves every request.
func NewServerBackend(customURL, designURL, baseURL string, fallback Backend) *serverBackend {
	return &serverBackend{
		client:    &http.Client{Timeout: 30 * time.Minute},
		customURL: strings.TrimRight(customURL, "/"),
		designURL: strings.TrimRight(designURL, "/"),
		baseURL:   strings.TrimRight(baseURL, "/"),
		fallback:  fallback,
	}
}

func (b *serverBackend) urlFor(mode VoiceMode) (string, error) {
	switch mode {
	case ModeCustomVoice:
		if b.customURL == "" {
			return "", fmt.Errorf("no tts-server configured for customvoice")
		}
		return b.customURL, nil
	case ModeVoiceDesign:
		if b.designURL == "" {
			return "", fmt.Errorf("no tts-server configured for voicedesign")
		}
		return b.designURL, nil
	case ModeBase:
		if b.baseURL == "" {
			return "", fmt.Errorf("no tts-server configured for base/clone")
		}
		return b.baseURL, nil
	default:
		return "", fmt.Errorf("unsupported mode %s", mode)
	}
}

func (b *serverBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	base, err := b.urlFor(spec.Mode)
	if err != nil {
		if b.fallback != nil {
			return b.fallback.Speech(ctx, spec, text)
		}
		return nil, err
	}

	if spec.Mode == ModeBase && len(spec.RefAudio) > 0 {
		if err := b.registerVoice(ctx, base, spec); err != nil {
			if b.fallback != nil {
				return b.fallback.Speech(ctx, spec, text)
			}
			return nil, err
		}
	}

	voice := spec.Speaker
	if spec.Mode == ModeBase {
		voice = spec.ID
	} else if spec.Mode == ModeVoiceDesign {
		voice = ""
	}
	reqBody := map[string]any{
		"input":           text,
		"voice":           voice,
		"instructions":    spec.Instruct,
		"response_format": "wav",
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
	resp, err := b.client.Do(httpReq)
	if err != nil {
		if b.fallback != nil {
			return b.fallback.Speech(ctx, spec, text)
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts-server speech: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return &SpeechResult{Audio: data, Format: "wav"}, nil
}

func (b *serverBackend) registerVoice(ctx context.Context, base string, spec SynthSpec) error {
	payload := map[string]any{
		"name":     spec.ID,
		"ref_text": spec.RefText,
		"wav_b64":  base64.StdEncoding.EncodeToString(spec.RefAudio),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/voices", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("tts-server register voice: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (b *serverBackend) Health(ctx context.Context) (map[string]any, error) {
	url := b.customURL
	if url == "" {
		url = b.baseURL
	}
	if url == "" {
		if b.fallback != nil {
			return b.fallback.Health(ctx)
		}
		return nil, fmt.Errorf("no tts-server configured")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts-server health: %s", resp.Status)
	}
	return map[string]any{"status": "ok", "engine": "tts-server", "backend_url": url}, nil
}

func (b *serverBackend) Close() error { return nil }

// Package tts implements the forge-tts service: an OpenAI-compatible
// Qwen3-TTS server that replaces scripts/tts_server.py with a pure-Go binary.
//
// Inference is delegated to qwentts.cpp (ggml, Vulkan/ROCm) via its qwen-tts
// CLI for now; the long-term path is the qt_* C99 ABI through purego (no cgo).
// See docs/tts-inference-core-research.md for the engine-selection rationale.
package tts

import (
	"context"
	"encoding/json"
)

type Engine interface {
	Health(ctx context.Context) (map[string]any, error)
	Models() []ModelInfo
	Speech(ctx context.Context, req SpeechRequest) (*SpeechResult, error)
	Preview(ctx context.Context, p PreviewRequest) (*SpeechResult, error)
	GenerateSample(ctx context.Context, id string) error
	ListVoices(ctx context.Context) ([]VoiceEntry, error)
	GetVoice(ctx context.Context, id string) (VoiceEntry, bool, error)
	SaveVoice(ctx context.Context, v VoiceEntry) error
	DeleteVoice(ctx context.Context, id string) error
	GetSample(ctx context.Context, id string) ([]byte, error)
	SetSample(ctx context.Context, id string, wav []byte) error
}

// PreviewRequest synthesizes a voice from inline parameters without persisting
// it. Mode "clone" supplies RefAudio bytes + RefText; Mode "design" supplies an
// Instruct string.
type PreviewRequest struct {
	Mode     string
	Instruct string
	RefAudio []byte
	RefText  string
	Text     string
	Language string
	Format   string
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type SpeechRequest struct {
	Model          string  `json:"model"`
	Voice          string  `json:"voice"`
	Input          string  `json:"input"`
	ResponseFormat string  `json:"response_format"`
	Language       string  `json:"language"`
	Instruct       string  `json:"instruct"`
	RefWAV         string  `json:"ref_wav"`
	RefText        string  `json:"ref_text"`
	Speed          float64 `json:"speed"`
	Seed           int     `json:"seed"`
}

type SpeechResult struct {
	Audio  []byte
	Format string
}

type VoiceMode string

const (
	ModeCustomVoice VoiceMode = "customvoice"
	ModeVoiceDesign VoiceMode = "voicedesign"
	ModeBase        VoiceMode = "base"
	ModeKokoro      VoiceMode = "kokoro"
)

// SynthSpec is a voice fully resolved to a synthesis mode plus the mode-specific
// parameters needed by a Backend. The Engine builds it from a SpeechRequest
// (built-in speaker, or a registry entry of type design/clone/customvoice).
type SynthSpec struct {
	Mode     VoiceMode
	ID       string // stable voice id, used for server-backend clone registration
	Speaker  string // ModeCustomVoice: speaker name
	Instruct string // ModeVoiceDesign: style instruction
	RefAudio []byte // ModeBase: reference WAV bytes
	RefText  string // ModeBase: reference transcript (enables ICL clone)
	Language string
	Format   string // "wav" (default) or "pcm"
	Speed    float64
	Tier     string // "fast" (Kokoro) or "premium" (Qwen)
}

// Backend performs raw synthesis for a resolved SynthSpec. Two implementations
// exist: cliBackend (shells the qwen-tts CLI, model reloaded per call) and
// serverBackend (proxies a resident tts-server process, model kept in GPU RAM).
type Backend interface {
	Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error)
	Health(ctx context.Context) (map[string]any, error)
	Close() error
}

type DesignSpec struct {
	Instruct string `json:"instruct"`
}

type CloneSpec struct {
	RefAudio     string `json:"ref_audio"`
	RefText      string `json:"ref_text"`
	XVectorOnly  bool   `json:"x_vector_only"`
}

type VoiceEntry struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Language   string            `json:"language"`
	Tier       string            `json:"tier,omitempty"`
	Design     *DesignSpec       `json:"design,omitempty"`
	Clone      *CloneSpec        `json:"clone,omitempty"`
	SampleText string            `json:"sample_text,omitempty"`
	Extra      map[string]json.RawMessage `json:"-"`
}

func (v *VoiceEntry) UnmarshalJSON(b []byte) error {
	type alias VoiceEntry
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*v = VoiceEntry(a)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	extra := map[string]json.RawMessage{}
	for k, val := range m {
		switch k {
		case "id", "name", "type", "language", "design", "clone", "sample_text":
		default:
			extra[k] = val
		}
	}
	if len(extra) > 0 {
		v.Extra = extra
	}
	return nil
}

func (v VoiceEntry) MarshalJSON() ([]byte, error) {
	type alias VoiceEntry
	a := alias(v)
	base, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if len(v.Extra) == 0 {
		return base, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	for k, val := range v.Extra {
		obj[k] = val
	}
	return json.Marshal(obj)
}

func (v VoiceEntry) MarshalBinary() ([]byte, error) { return json.Marshal(v) }
func (v *VoiceEntry) UnmarshalBinary(b []byte) error { return json.Unmarshal(b, v) }

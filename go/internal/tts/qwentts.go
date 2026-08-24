package tts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type QwenTTS struct {
	Backend    Backend
	Registry   *Registry
	modelInfos []ModelInfo
}

func NewQwenTTS(backend Backend, reg *Registry) *QwenTTS {
	return &QwenTTS{
		Backend:  backend,
		Registry: reg,
		modelInfos: []ModelInfo{
			{ID: "tts-1", Object: "model", OwnedBy: "forge"},
		},
	}
}

func (q *QwenTTS) Health(ctx context.Context) (map[string]any, error) { return q.Backend.Health(ctx) }
func (q *QwenTTS) Models() []ModelInfo                                { return q.modelInfos }

func (q *QwenTTS) resolveSpec(req SpeechRequest) (SynthSpec, error) {
	lang := req.Language
	if lang == "" {
		lang = "English"
	}
	spec := SynthSpec{Language: lang, Format: req.ResponseFormat}
	if spec.Format == "" {
		spec.Format = "wav"
	}

	v, ok, err := q.Registry.Get(req.Voice)
	if err != nil {
		return spec, err
	}
	if !ok {
		return spec, fmt.Errorf("unknown voice %q", req.Voice)
	}
	spec.ID = v.ID

	switch v.Type {
	case "design":
		instruct := req.Instruct
		if instruct == "" && v.Design != nil {
			instruct = v.Design.Instruct
		}
		spec.Mode = ModeVoiceDesign
		spec.Speaker = v.Name
		spec.Instruct = instruct
	case "clone":
		if v.Clone == nil {
			return spec, fmt.Errorf("clone voice %s missing clone spec", v.ID)
		}
		data, rerr := os.ReadFile(filepath.Join(q.Registry.AudioDir(), v.Clone.RefAudio))
		if rerr != nil {
			return spec, fmt.Errorf("read ref audio: %w", rerr)
		}
		spec.Mode = ModeBase
		spec.RefAudio = data
		spec.RefText = v.Clone.RefText
	case "customvoice":
		spec.Mode = ModeCustomVoice
		spec.Speaker = v.Name
	default:
		return spec, fmt.Errorf("unsupported voice type %q", v.Type)
	}
	return spec, nil
}

func (q *QwenTTS) Speech(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	spec, err := q.resolveSpec(req)
	if err != nil {
		return nil, err
	}
	return q.Backend.Speech(ctx, spec, req.Input)
}

func (q *QwenTTS) Preview(ctx context.Context, p PreviewRequest) (*SpeechResult, error) {
	lang := p.Language
	if lang == "" {
		lang = "English"
	}
	format := p.Format
	if format == "" {
		format = "wav"
	}
	var spec SynthSpec
	switch p.Mode {
	case "clone", "base":
		if len(p.RefAudio) == 0 {
			return nil, fmt.Errorf("clone preview requires reference audio")
		}
		spec = SynthSpec{Mode: ModeBase, RefAudio: p.RefAudio, RefText: p.RefText, Language: lang, Format: format}
	case "customvoice":
		if p.Instruct == "" {
			return nil, fmt.Errorf("customvoice preview requires a speaker")
		}
		spec = SynthSpec{Mode: ModeCustomVoice, Speaker: p.Instruct, Language: lang, Format: format}
	default:
		if p.Instruct == "" {
			return nil, fmt.Errorf("design preview requires an instruct string")
		}
		spec = SynthSpec{Mode: ModeVoiceDesign, Instruct: p.Instruct, Language: lang, Format: format}
	}
	return q.Backend.Speech(ctx, spec, p.Text)
}

func (q *QwenTTS) GenerateSample(ctx context.Context, id string) error {
	v, ok, err := q.Registry.Get(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("voice %q not found", id)
	}
	text := v.SampleText
	if text == "" {
		text = "This is a sample of the voice."
	}
	spec, err := q.resolveSpec(SpeechRequest{Voice: id, Input: text, Language: v.Language})
	if err != nil {
		return err
	}
	res, err := q.Backend.Speech(ctx, spec, text)
	if err != nil {
		return err
	}
	return q.Registry.SetSample(id, res.Audio)
}

func (q *QwenTTS) ListVoices(ctx context.Context) ([]VoiceEntry, error) { return q.Registry.List() }
func (q *QwenTTS) GetVoice(ctx context.Context, id string) (VoiceEntry, bool, error) {
	return q.Registry.Get(id)
}
func (q *QwenTTS) SaveVoice(ctx context.Context, v VoiceEntry) error    { return q.Registry.Put(v) }
func (q *QwenTTS) DeleteVoice(ctx context.Context, id string) error    { return q.Registry.Delete(id) }
func (q *QwenTTS) GetSample(ctx context.Context, id string) ([]byte, error) {
	return q.Registry.GetSample(id)
}
func (q *QwenTTS) SetSample(ctx context.Context, id string, wav []byte) error {
	return q.Registry.SetSample(id, wav)
}

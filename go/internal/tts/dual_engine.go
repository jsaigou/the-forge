package tts

import (
	"context"
	"fmt"
	"sort"
)

// dualEngine is the Orchestrator described in docs/adr-003-dual-model-tts.md.
// It implements the Engine interface and routes synthesis by voice namespace:
//
//   - Kokoro IDs (e.g. af_heart)   -> kokoroBackend (fast tier, CPU-resident)
//   - Registry clones/designs       -> QwenTTS (premium tier, cold CLI / server)
//
// ListVoices merges both sources and tags each with a tier.
type dualEngine struct {
	qwen             *QwenTTS
	kokoro           *kokoroBackend
	defaultVoice     string // premium default when none requested
	defaultFastVoice string // fast default (Kokoro)
}

func NewDualEngine(qwen *QwenTTS, kokoro *kokoroBackend, defaultVoice, defaultFastVoice string) *dualEngine {
	if defaultVoice == "" {
		defaultVoice = "billie"
	}
	if defaultFastVoice == "" {
		defaultFastVoice = "af_heart"
	}
	return &dualEngine{
		qwen:             qwen,
		kokoro:           kokoro,
		defaultVoice:     defaultVoice,
		defaultFastVoice: defaultFastVoice,
	}
}

func (d *dualEngine) routeKokoro(req SpeechRequest) bool {
	if req.Model == "kokoro" {
		return true
	}
	if req.Voice == "" {
		return false
	}
	return IsKokoroVoice(req.Voice)
}

func (d *dualEngine) Speech(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	if req.Voice == "" {
		// Default to premium; the UI's quality toggle sends an explicit id.
		req.Voice = d.defaultVoice
	}
	if d.routeKokoro(req) {
		spec := SynthSpec{
			Mode:     ModeKokoro,
			ID:       req.Voice,
			Language: req.Language,
			Format:   req.ResponseFormat,
			Speed:    req.Speed,
			Tier:     "fast",
		}
		return d.kokoro.Speech(ctx, spec, req.Input)
	}
	return d.qwen.Speech(ctx, req)
}

func (d *dualEngine) Preview(ctx context.Context, p PreviewRequest) (*SpeechResult, error) {
	// Previews are design/clone only -> premium engine.
	return d.qwen.Preview(ctx, p)
}

func (d *dualEngine) GenerateSample(ctx context.Context, id string) error {
	if IsKokoroVoice(id) {
		return nil // fast voices are built-in; nothing to bake
	}
	return d.qwen.GenerateSample(ctx, id)
}

func (d *dualEngine) ListVoices(ctx context.Context) ([]VoiceEntry, error) {
	var out []VoiceEntry
	if d.kokoro != nil {
		kv, err := d.kokoro.ListVoices(ctx)
		if err != nil {
			// Kokoro down should not hide premium voices.
			fmt.Printf("warn: kokoro list voices: %v\n", err)
		} else {
			out = append(out, kv...)
		}
	}
	pv, err := d.qwen.ListVoices(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range pv {
		if v.Tier == "" {
			v.Tier = "premium"
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d *dualEngine) GetVoice(ctx context.Context, id string) (VoiceEntry, bool, error) {
	if IsKokoroVoice(id) && d.kokoro != nil {
		if kv, err := d.kokoro.ListVoices(ctx); err == nil {
			for _, v := range kv {
				if v.ID == id {
					return v, true, nil
				}
			}
		}
	}
	return d.qwen.GetVoice(ctx, id)
}

func (d *dualEngine) SaveVoice(ctx context.Context, v VoiceEntry) error {
	return d.qwen.SaveVoice(ctx, v)
}
func (d *dualEngine) DeleteVoice(ctx context.Context, id string) error {
	return d.qwen.DeleteVoice(ctx, id)
}
func (d *dualEngine) GetSample(ctx context.Context, id string) ([]byte, error) {
	return d.qwen.GetSample(ctx, id)
}
func (d *dualEngine) SetSample(ctx context.Context, id string, wav []byte) error {
	return d.qwen.SetSample(ctx, id, wav)
}

func (d *dualEngine) Health(ctx context.Context) (map[string]any, error) {
	out := map[string]any{"status": "ok", "model_ready": true}
	if d.kokoro != nil {
		if h, err := d.kokoro.Health(ctx); err != nil {
			out["kokoro"] = map[string]any{"status": "error", "detail": err.Error()}
		} else {
			out["kokoro"] = h
		}
	}
	if h, err := d.qwen.Health(ctx); err != nil {
		out["qwen"] = map[string]any{"status": "error", "detail": err.Error()}
	} else {
		out["qwen"] = h
	}
	return out, nil
}

func (d *dualEngine) Models() []ModelInfo {
	models := []ModelInfo{
		{ID: "kokoro-82m", Object: "model", OwnedBy: "forge"},
	}
	return append(models, d.qwen.Models()...)
}

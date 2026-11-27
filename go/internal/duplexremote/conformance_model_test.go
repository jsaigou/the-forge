package duplexremote

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"unicode/utf8"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexcard"
)

// wavMono16 is a mono 16-bit PCM WAV of a sine tone. samples is deliberately not a whole number of
// 3200-sample frames in the callers below: a reference clip has an arbitrary length.
func wavMono16(rate, samples int, hz float64) []byte {
	var b bytes.Buffer
	data := make([]byte, 2*samples)
	for i := 0; i < samples; i++ {
		v := int16(0.3 * 32767 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

// modelInput is the user's audio for step i: a quiet tone, so the model has something to hear.
func modelInput(i int) duplex.Frame {
	f := make(duplex.Frame, duplex.FrameSamples)
	for j := range f {
		f[j] = 0.1 * float32(math.Sin(2*math.Pi*330*float64(i*duplex.FrameSamples+j)/duplex.SampleRate))
	}
	return f
}

type modelRun struct {
	audio [][]float32
	text  []string
}

// runModel opens one lane with the given session id and steps it n times.
func (c *conf) runModel(t *testing.T, model, session string, text bool, n int) modelRun {
	t.Helper()
	ctx := context.Background()
	tn := New(Config{BaseURL: c.tg.BaseURL, Tenant: model, Token: c.tg.Token,
		StepTimeout: 20e9, StartTimeout: 120e9, CardRenderer: func(card map[string]any) (any, error) {
			return duplexcard.Render(card)
		}})
	s, err := tn.Start(ctx, duplex.TenantConfig{
		SessionID: session, SampleRate: duplex.SampleRate, Text: text,
		Card:      map[string]any{"name": "Haru", "language": "en"},
		Reference: duplex.Reference{ID: "conf-voice", Audio: wavMono16(24000, 36000, 220), Transcript: "hello"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	var r modelRun
	for i := 0; i < n; i++ {
		out, err := s.Step(ctx, modelInput(i), uint32(i))
		if err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
		r.audio = append(r.audio, out.Audio)
		r.text = append(r.text, out.Text)
	}
	return r
}

func runModelConformance(t *testing.T, c *conf, model string) {
	t.Run("model_lockstep_range_and_text", func(t *testing.T) {
		r := c.runModel(t, model, "conf-model-a", true, 12)
		var spoke bool
		for i, a := range r.audio {
			if len(a) != 0 && len(a) != duplex.FrameSamples {
				t.Fatalf("step %d: audio has %d samples, want 0 or %d", i, len(a), duplex.FrameSamples)
			}
			for j, x := range a {
				if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) || x < -1 || x > 1 {
					t.Fatalf("step %d sample %d = %v: outside [-1, 1] or not finite", i, j, x)
				}
			}
			if len(a) > 0 {
				spoke = true
			}
			if !utf8.ValidString(r.text[i]) {
				t.Fatalf("step %d: text %q is not valid UTF-8", i, r.text[i])
			}
		}
		if !spoke {
			t.Fatalf("12 steps produced no audio frame at all")
		}
	})
	t.Run("model_same_session_replays_and_other_session_differs", func(t *testing.T) {
		a := c.runModel(t, model, "conf-seed-1", true, 8)
		b := c.runModel(t, model, "conf-seed-1", true, 8)
		o := c.runModel(t, model, "conf-seed-2", true, 8)
		same := func(x, y modelRun) bool {
			for i := range x.audio {
				if len(x.audio[i]) != len(y.audio[i]) || x.text[i] != y.text[i] {
					return false
				}
				for j := range x.audio[i] {
					if x.audio[i][j] != y.audio[i][j] {
						return false
					}
				}
			}
			return true
		}
		if !same(a, b) {
			t.Fatalf("the same session id and input did not replay the same output")
		}
		if same(a, o) {
			t.Fatalf("a different session id produced bit-identical audio and text; the seed is not used")
		}
	})
	t.Run("model_text_off_sends_no_text", func(t *testing.T) {
		r := c.runModel(t, model, "conf-notext", false, 6)
		for i, s := range r.text {
			if s != "" {
				t.Fatalf("step %d: text %q with text=false", i, s)
			}
		}
	})
	t.Run("model_lane_released_after_session", func(t *testing.T) {
		// Opening and closing more sessions than the engine has lanes proves lanes are released.
		for i := 0; i < 4; i++ {
			c.runModel(t, model, fmt.Sprintf("conf-lane-%d", i), true, 1)
		}
	})
}

package duplexremote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexcard"
)

// TestRefEvalExternalEngine runs one short session per job against a real engine and records what the model says, so a reference clip, a card and a
// seed can be compared by what comes out (the reference clip's own words coming back, names, a made-up third person, silence).
//
// It is skipped unless FORGE_DUPLEX_TEST_ENGINE_URL, FORGE_DUPLEX_TEST_MODEL, FORGE_DUPLEX_REFEVAL_JOBS and FORGE_DUPLEX_REFEVAL_OUT are set.
//   - FORGE_DUPLEX_REFEVAL_JOBS: a JSON file, a list of {"name","ref","card","session"}: ref is a 24 kHz mono PCM16 WAV path, card the card object
//     (as the page sends it), session the session id (the engine seeds its sampling from it, so the same id replays the same run).
//   - FORGE_DUPLEX_SOAK_USER: the 24 kHz mono PCM16 WAV the "user" says, looped (the same format as the soak test).
//   - FORGE_DUPLEX_REFEVAL_FRAMES: steps per job (default 150, about 20 s of conversation); FORGE_DUPLEX_REFEVAL_OUT: the JSON-lines results file.
//   - FORGE_DUPLEX_REFEVAL_WAV_DIR: optional; each job's model audio is written there as <name>.wav.
func TestRefEvalExternalEngine(t *testing.T) {
	base, model := os.Getenv("FORGE_DUPLEX_TEST_ENGINE_URL"), os.Getenv("FORGE_DUPLEX_TEST_MODEL")
	jobsPath, outPath := os.Getenv("FORGE_DUPLEX_REFEVAL_JOBS"), os.Getenv("FORGE_DUPLEX_REFEVAL_OUT")
	if base == "" || model == "" || jobsPath == "" || outPath == "" {
		t.Skip("FORGE_DUPLEX_TEST_ENGINE_URL, FORGE_DUPLEX_TEST_MODEL, FORGE_DUPLEX_REFEVAL_JOBS and FORGE_DUPLEX_REFEVAL_OUT are required")
	}
	frames := 150
	if v := os.Getenv("FORGE_DUPLEX_REFEVAL_FRAMES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("FORGE_DUPLEX_REFEVAL_FRAMES=%q", v)
		}
		frames = n
	}
	tenant := "haru"
	if v := os.Getenv("FORGE_DUPLEX_TEST_TENANT"); v != "" {
		tenant = v
	}
	var jobs []struct {
		Name    string         `json:"name"`
		Ref     string         `json:"ref"`
		Card    map[string]any `json:"card"`
		Session string         `json:"session"`
	}
	raw, err := os.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		t.Fatalf("jobs file: %v", err)
	}
	user, err := readWavMono16(os.Getenv("FORGE_DUPLEX_SOAK_USER"))
	if err != nil {
		t.Fatal(err)
	}
	nUser := len(user) / duplex.FrameSamples
	if nUser == 0 {
		t.Fatal("the user clip is shorter than one frame")
	}
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	wavDir := os.Getenv("FORGE_DUPLEX_REFEVAL_WAV_DIR")

	for _, j := range jobs {
		ref, err := os.ReadFile(j.Ref)
		if err != nil {
			t.Fatalf("%s: reference: %v", j.Name, err)
		}
		tn := New(Config{BaseURL: base, Tenant: tenant, Token: os.Getenv("FORGE_DUPLEX_TEST_ENGINE_TOKEN"), StepTimeout: 20 * time.Second, StartTimeout: 180 * time.Second,
			CardRenderer: func(card map[string]any) (any, error) { return duplexcard.Render(card) }})
		s, err := tn.Start(context.Background(), duplex.TenantConfig{SessionID: j.Session, SampleRate: duplex.SampleRate, Text: true, Card: j.Card,
			Reference: duplex.Reference{ID: "refeval-" + j.Name, Audio: ref, Transcript: ""}})
		if err != nil {
			t.Errorf("%s: Start: %v", j.Name, err)
			continue
		}
		var text strings.Builder
		var pcm []int16
		voiced := 0
		for i := 0; i < frames; i++ {
			f := make(duplex.Frame, duplex.FrameSamples)
			copy(f, user[(i%nUser)*duplex.FrameSamples:(i%nUser+1)*duplex.FrameSamples])
			tf, err := s.Step(context.Background(), f, uint32(i))
			if err != nil {
				t.Errorf("%s: step %d: %v", j.Name, i, err)
				break
			}
			text.WriteString(tf.Text)
			peak := float32(0)
			for _, a := range tf.Audio {
				if a < 0 {
					a = -a
				}
				if a > peak {
					peak = a
				}
			}
			if peak > 0.02 {
				voiced++
			}
			if wavDir != "" {
				if len(tf.Audio) == 0 {
					pcm = append(pcm, make([]int16, duplex.FrameSamples)...)
				}
				for _, a := range tf.Audio {
					v := float64(a) * 32767
					if v > 32767 {
						v = 32767
					} else if v < -32768 {
						v = -32768
					}
					pcm = append(pcm, int16(v))
				}
			}
		}
		s.Close()
		if wavDir != "" {
			if err := os.WriteFile(fmt.Sprintf("%s/%s.wav", wavDir, j.Name), wavBytes(pcm), 0o644); err != nil {
				t.Errorf("%s: write wav: %v", j.Name, err)
			}
		}
		line, _ := json.Marshal(map[string]any{"name": j.Name, "session": j.Session, "frames": frames, "voiced_frames": voiced, "text": text.String()})
		if _, err := out.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d voiced frames, %d chars of text", j.Name, voiced, text.Len())
	}
}

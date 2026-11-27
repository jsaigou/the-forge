package duplexremote

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexcard"
)

// readWavMono16 reads a canonical 44-byte-header mono 16-bit WAV (the format the listening and goldens tooling writes) as float frames' worth of samples.
func readWavMono16(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[36:40]) != "data" {
		return nil, fmt.Errorf("%s: not a canonical 44-byte-header PCM16 WAV", path)
	}
	n := (len(b) - 44) / 2
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[44+2*i:]))) / 32768
	}
	return out, nil
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// TestSoakExternalEngine holds real sessions on a real engine for FORGE_DUPLEX_SOAK_FRAMES steps per lane and reports the step latency (send to
// reply) per interval, so a slowdown as the context grows shows up. It is skipped unless FORGE_DUPLEX_TEST_ENGINE_URL, FORGE_DUPLEX_TEST_MODEL,
// FORGE_DUPLEX_SOAK_FRAMES, FORGE_DUPLEX_SOAK_REF (24 kHz mono PCM16 WAV, the voice) and FORGE_DUPLEX_SOAK_USER (the same format, looped) are set.
// FORGE_DUPLEX_SOAK_PACE=real sends one frame per 133.3 ms (a live conversation); anything else sends the next frame as soon as the last reply
// arrives (the context-growth sweep). FORGE_DUPLEX_SOAK_LANES (default 1). FORGE_DUPLEX_SOAK_INTERVAL (frames per report row, default 1000).
func TestSoakExternalEngine(t *testing.T) {
	base, model := os.Getenv("FORGE_DUPLEX_TEST_ENGINE_URL"), os.Getenv("FORGE_DUPLEX_TEST_MODEL")
	framesS := os.Getenv("FORGE_DUPLEX_SOAK_FRAMES")
	if base == "" || model == "" || framesS == "" {
		t.Skip("FORGE_DUPLEX_TEST_ENGINE_URL, FORGE_DUPLEX_TEST_MODEL and FORGE_DUPLEX_SOAK_FRAMES are required")
	}
	frames, err := strconv.Atoi(framesS)
	if err != nil || frames <= 0 {
		t.Fatalf("FORGE_DUPLEX_SOAK_FRAMES=%q", framesS)
	}
	// FORGE_DUPLEX_TEST_TENANT is the tenant name sent in start when it differs from the model id (a pool engine such as haru-4b serves tenant haru).
	tenantName := model
	if v := os.Getenv("FORGE_DUPLEX_TEST_TENANT"); v != "" {
		tenantName = v
	}
	// FORGE_DUPLEX_SOAK_SESSION_PREFIX fixes the session ids (the engine seeds its sampling from the id), so two runs of the same inputs replay
	// byte for byte and a digest difference is a real difference (B7). Unset, ids are unique per run.
	sessionID := func(l int) string {
		if p := os.Getenv("FORGE_DUPLEX_SOAK_SESSION_PREFIX"); p != "" {
			return fmt.Sprintf("%s-%d", p, l)
		}
		return fmt.Sprintf("soak-%d-%d", time.Now().UnixNano(), l)
	}
	lanes, interval := 1, 1000
	if v := os.Getenv("FORGE_DUPLEX_SOAK_LANES"); v != "" {
		lanes, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("FORGE_DUPLEX_SOAK_INTERVAL"); v != "" {
		interval, _ = strconv.Atoi(v)
	}
	// FORGE_DUPLEX_SOAK_WAV_OUT=<prefix> writes each lane's model audio to <prefix>-lane<N>.wav (24 kHz mono PCM16) so a change can be judged by transcript.
	wavOut := os.Getenv("FORGE_DUPLEX_SOAK_WAV_OUT")
	real := os.Getenv("FORGE_DUPLEX_SOAK_PACE") == "real"
	ref, err := os.ReadFile(os.Getenv("FORGE_DUPLEX_SOAK_REF"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := readWavMono16(os.Getenv("FORGE_DUPLEX_SOAK_USER"))
	if err != nil {
		t.Fatal(err)
	}
	nUser := len(user) / duplex.FrameSamples
	if nUser == 0 {
		t.Fatal("user audio shorter than one frame")
	}
	budget := float64(duplex.FrameSamples) / duplex.SampleRate * 1000 // ms per frame

	type sample struct {
		lane, i int
		ms      float64
	}
	// Output digests (B7): one FNV-64a per lane per FORGE_DUPLEX_SOAK_DIGEST_EVERY steps (default 100) over the exact audio samples and text of
	// every reply, so two runs with the same inputs and seeds can be compared window by window and a divergence located.
	digestEvery := 100
	if v := os.Getenv("FORGE_DUPLEX_SOAK_DIGEST_EVERY"); v != "" {
		digestEvery, _ = strconv.Atoi(v)
	}
	digests := make([][]string, lanes)
	var mu sync.Mutex
	var all []sample
	var wg sync.WaitGroup
	start := time.Now()
	for l := 0; l < lanes; l++ {
		wg.Add(1)
		go func(l int) {
			defer wg.Done()
			var pcm []int16 // the model's output, written as a WAV when FORGE_DUPLEX_SOAK_WAV_OUT is set
			ctx := context.Background()
			tn := New(Config{BaseURL: base, Tenant: tenantName, Token: os.Getenv("FORGE_DUPLEX_TEST_ENGINE_TOKEN"), StepTimeout: 20 * time.Second, StartTimeout: 180 * time.Second,
				CardRenderer: func(card map[string]any) (any, error) { return duplexcard.Render(card) }})
			s, err := tn.Start(ctx, duplex.TenantConfig{SessionID: sessionID(l), SampleRate: duplex.SampleRate, Text: true,
				Card:      map[string]any{"name": "Haru", "language": "ja"},
				Reference: duplex.Reference{ID: "soak-voice", Audio: ref, Transcript: ""}})
			if err != nil {
				t.Errorf("lane %d Start: %v", l, err)
				return
			}
			defer s.Close()
			defer func() {
				if wavOut != "" {
					if err := os.WriteFile(fmt.Sprintf("%s-lane%d.wav", wavOut, l), wavBytes(pcm), 0o644); err != nil {
						t.Errorf("write wav: %v", err)
					}
				}
			}()
			h := fnv.New64a()
			tick := time.NewTicker(time.Duration(budget * float64(time.Millisecond)))
			defer tick.Stop()
			for i := 0; i < frames; i++ {
				if real && i > 0 {
					<-tick.C
				}
				f := make(duplex.Frame, duplex.FrameSamples)
				copy(f, user[(i%nUser)*duplex.FrameSamples:(i%nUser+1)*duplex.FrameSamples])
				t0 := time.Now()
				tf, err := s.Step(ctx, f, uint32(i))
				if err != nil {
					t.Errorf("lane %d step %d: %v", l, i, err)
					return
				}
				if wavOut != "" {
					if len(tf.Audio) == 0 {
						pcm = append(pcm, make([]int16, duplex.FrameSamples)...) // the model said nothing this frame: keep the timeline
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
				var b4 [4]byte
				for _, a := range tf.Audio {
					u := math.Float32bits(a)
					b4[0], b4[1], b4[2], b4[3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
					h.Write(b4[:])
				}
				h.Write([]byte(tf.Text))
				if (i+1)%digestEvery == 0 {
					digests[l] = append(digests[l], fmt.Sprintf("%016x", h.Sum64()))
					h.Reset()
				}
				ms := float64(time.Since(t0).Microseconds()) / 1000
				mu.Lock()
				all = append(all, sample{l, i, ms})
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	wall := time.Since(start)
	// One row per interval of steps, over all lanes.
	fmt.Printf("SOAK model=%s lanes=%d pace=%v frames/lane=%d wall=%.0fs budget=%.1f ms\n", model, lanes, map[bool]string{true: "real", false: "flat-out"}[real], frames, wall.Seconds(), budget)
	fmt.Printf("%-13s %7s %7s %7s %7s %9s\n", "steps", "p50", "p95", "p99", "max", "over-budget")
	for lo := 0; lo < frames; lo += interval {
		var ms []float64
		over := 0
		for _, s := range all {
			if s.i >= lo && s.i < lo+interval {
				ms = append(ms, s.ms)
				if s.ms > budget {
					over++
				}
			}
		}
		if len(ms) == 0 {
			continue
		}
		sort.Float64s(ms)
		fmt.Printf("%5d-%-7d %7.1f %7.1f %7.1f %7.1f %9d\n", lo, lo+interval-1, pct(ms, .5), pct(ms, .95), pct(ms, .99), ms[len(ms)-1], over)
	}
	for l := range digests {
		fmt.Printf("DIGEST lane %d every %d: %v\n", l, digestEvery, digests[l])
	}
	var ms []float64
	over := 0
	for _, s := range all {
		ms = append(ms, s.ms)
		if s.ms > budget {
			over++
		}
	}
	sort.Float64s(ms)
	for _, s := range all {
		if s.ms > budget {
			fmt.Printf("OVER BUDGET lane %d step %d: %.1f ms\n", s.lane, s.i, s.ms)
		}
	}
	fmt.Printf("ALL %d steps: p50 %.1f p95 %.1f p99 %.1f max %.1f ms; over budget %d (%.2f%%)\n", len(ms), pct(ms, .5), pct(ms, .95), pct(ms, .99), ms[len(ms)-1], over, 100*float64(over)/float64(len(ms)))
}

// wavBytes wraps mono 24 kHz PCM16 in a canonical 44-byte-header WAV.
func wavBytes(pcm []int16) []byte {
	var b bytes.Buffer
	n := len(pcm) * 2
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(duplex.SampleRate), uint32(duplex.SampleRate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	_ = binary.Write(&b, binary.LittleEndian, pcm)
	return b.Bytes()
}

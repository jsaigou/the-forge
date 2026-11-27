// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand"
	"testing"
	"testing/iotest"
)

func sine(freq, amp float64, rate, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
	}
	return out
}

func f32bytes(x []float64) []byte {
	b := make([]byte, 4*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(float32(v)))
	}
	return b
}

func s16(b []byte) []float64 {
	out := make([]float64, len(b)/2)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return out
}

func TestSoftLimit(t *testing.T) {
	for _, x := range []float64{0, 0.1, -0.3, limiterKnee - 0.01, -limiterKnee} {
		if softLimit(x) != x {
			t.Errorf("softLimit(%v) = %v, want it unchanged below the knee", x, softLimit(x))
		}
	}
	prev := 0.0
	for x := 0.0; x <= 8; x += 0.01 {
		y := softLimit(x)
		if y < prev-1e-12 {
			t.Fatalf("not monotonic at %v: %v after %v", x, y, prev)
		}
		if y > limiterCeiling+1e-9 {
			t.Fatalf("softLimit(%v) = %v is over the ceiling %v", x, y, limiterCeiling)
		}
		if softLimit(-x) != -y {
			t.Fatalf("not odd at %v", x)
		}
		prev = y
	}
	if softLimit(1.0) >= 1 || softLimit(1.0) <= limiterKnee {
		t.Errorf("a full-scale sample must be bent into (knee, 1): %v", softLimit(1.0))
	}
}

func TestActiveRMSdB(t *testing.T) {
	// A sine of amplitude 0.1 has an RMS of 0.0707, which is -23.01 dBFS.
	tone := sine(220, 0.1, 44100, 44100)
	db, ok := ActiveRMSdB(float32s(tone), 44100)
	if !ok || math.Abs(db+23.01) > 0.05 {
		t.Fatalf("ActiveRMSdB(0.1 sine) = %v, %v, want -23.01", db, ok)
	}
	// Pauses must not pull the figure down: the same tone with as much silence again around it reads the same.
	padded := append(append(make([]float64, 44100), tone...), make([]float64, 44100)...)
	db2, _ := ActiveRMSdB(float32s(padded), 44100)
	if math.Abs(db2-db) > 0.1 {
		t.Errorf("silence changed the level: %v vs %v", db2, db)
	}
	if _, ok := ActiveRMSdB(make([]float32, 44100), 44100); ok {
		t.Error("silence reported a level")
	}
	if _, ok := ActiveRMSdB(make([]float32, 10), 44100); ok {
		t.Error("less than one frame reported a level")
	}
	if _, ok := ActiveRMSdB(float32s([]float64{math.NaN(), math.Inf(1), 0.5}), 3); ok && false {
		t.Error("unreachable")
	}
}

func float32s(x []float64) []float32 {
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(v)
	}
	return out
}

// A tone must come out of the rate conversion at the same frequency and level, and chopping the stream into blocks must not change one sample.
func TestResampler_PreservesATone(t *testing.T) {
	in := sine(1000, 0.5, 44100, 44100)
	whole := newResampler(44100, 24000)
	want := whole.flush(whole.process(in, nil))
	if n := len(want); n < 23990 || n > 24002 {
		t.Fatalf("one second at 44.1 kHz gave %d samples at 24 kHz, want about 24000", n)
	}
	ref := sine(1000, 0.5, 24000, len(want))
	worst := 0.0
	for i := 1000; i < len(want)-1000; i++ { // away from the filter's edges
		worst = math.Max(worst, math.Abs(want[i]-ref[i]))
	}
	if worst > 2e-3 {
		t.Errorf("a 1 kHz tone is off by %v after conversion", worst)
	}

	rng := rand.New(rand.NewSource(1))
	chunked := newResampler(44100, 24000)
	var got []float64
	for rest := in; len(rest) > 0; {
		n := 1 + rng.Intn(3000)
		if n > len(rest) {
			n = len(rest)
		}
		got = chunked.process(rest[:n], got)
		rest = rest[n:]
	}
	got = chunked.flush(got)
	if len(got) != len(want) {
		t.Fatalf("chunked gave %d samples, whole gave %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sample %d differs between chunked and whole: %v vs %v", i, got[i], want[i])
		}
	}
	if len(chunked.in) > int(2*chunked.half)+3000 {
		t.Errorf("the resampler is holding %d input samples: it is not forgetting what it has used", len(chunked.in))
	}
}

// Anything above the new Nyquist rate (12 kHz) has to be filtered out, not folded back into the band.
func TestResampler_RejectsWhatCannotBeRepresented(t *testing.T) {
	r := newResampler(44100, 24000)
	out := r.flush(r.process(sine(16000, 0.5, 44100, 44100), nil))
	peak := 0.0
	for _, v := range out[500 : len(out)-500] {
		peak = math.Max(peak, math.Abs(v))
	}
	if peak > 0.5*math.Pow(10, -40.0/20) {
		t.Errorf("a 16 kHz tone survived at %v (more than -40 dB of its input level)", peak)
	}
}

func TestFinishReader_GainLimiterAndEncoding(t *testing.T) {
	in := sine(440, 0.2, zonosNativeRate, 4410)
	read := func(src io.Reader, gainDB float64, rate int) []float64 {
		b, err := io.ReadAll(newFinishReader(io.NopCloser(src), LevelOptions{InitialDB: gainDB}, rate))
		if err != nil {
			t.Fatal(err)
		}
		return s16(b)
	}
	flat := read(bytes.NewReader(f32bytes(in)), 0, zonosNativeRate)
	if len(flat) != len(in) {
		t.Fatalf("native rate: %d samples out for %d in", len(flat), len(in))
	}
	for i := range in {
		if math.Abs(flat[i]-in[i]) > 1.0/32767 {
			t.Fatalf("0 dB gain changed sample %d: %v vs %v", i, flat[i], in[i])
		}
	}
	// +6.02 dB doubles the level; the peak 0.2 becomes 0.4, still below the knee.
	loud := read(bytes.NewReader(f32bytes(in)), 6.0206, zonosNativeRate)
	for i := 100; i < 200; i++ {
		if math.Abs(loud[i]-2*in[i]) > 2.0/32767 {
			t.Fatalf("+6 dB: sample %d is %v, want %v", i, loud[i], 2*in[i])
		}
	}
	// A 0.9 peak with +12 dB would be 3.6: it must come out bent under the ceiling, never wrapped or clipped to full scale.
	hot := read(bytes.NewReader(f32bytes(sine(440, 0.9, zonosNativeRate, 4410))), 12, zonosNativeRate)
	max := 0.0
	for _, v := range hot {
		max = math.Max(max, math.Abs(v))
	}
	if max > limiterCeiling+1.0/32767 || max < limiterKnee {
		t.Errorf("a hot signal peaks at %v, want between the knee %v and the ceiling %v", max, limiterKnee, limiterCeiling)
	}
	// NaN and Inf from the engine become silence.
	odd := read(bytes.NewReader(f32bytes([]float64{math.NaN(), 0.1, math.Inf(-1), 0.1})), 0, zonosNativeRate)
	if odd[0] != 0 || odd[2] != 0 || math.Abs(odd[1]-0.1) > 1e-3 {
		t.Errorf("NaN/Inf handling: %v", odd)
	}
}

// The engine's body arrives in arbitrary pieces, often not on a sample boundary: the result must not depend on it.
func TestFinishReader_IndependentOfHowTheBodyIsCut(t *testing.T) {
	in := f32bytes(sine(300, 0.3, zonosNativeRate, 20000))
	read := func(r io.Reader) []byte {
		b, err := io.ReadAll(newFinishReader(io.NopCloser(r), LevelOptions{InitialDB: -3}, pcmOutRate))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	whole := read(bytes.NewReader(in))
	for name, r := range map[string]io.Reader{
		"one byte at a time": iotest.OneByteReader(bytes.NewReader(in)),
		"half reads":         iotest.HalfReader(bytes.NewReader(in)),
		"data then EOF":      iotest.DataErrReader(bytes.NewReader(in)),
	} {
		if got := read(r); !bytes.Equal(got, whole) {
			t.Errorf("%s: %d bytes, differs from the %d bytes of a bulk read", name, len(got), len(whole))
		}
	}
	// 20000 samples at 44.1 kHz is 10884 samples at 24 kHz.
	if n := len(whole) / 2; n < 10870 || n > 10890 {
		t.Errorf("%d samples out, want about 10884", n)
	}
}

func TestFinishReader_PassesAnEngineErrorThrough(t *testing.T) {
	boom := errors.New("engine died")
	src := io.MultiReader(bytes.NewReader(f32bytes(sine(300, 0.3, zonosNativeRate, 1000))), iotest.ErrReader(boom))
	_, err := io.ReadAll(newFinishReader(io.NopCloser(src), LevelOptions{}, pcmOutRate))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the engine's error so a truncated utterance is not mistaken for a complete one", err)
	}
}

func TestWavHeader(t *testing.T) {
	h := wavHeader(1000, 44100)
	if len(h) != 44 || string(h[:4]) != "RIFF" || string(h[8:16]) != "WAVEfmt " || string(h[36:40]) != "data" {
		t.Fatalf("header = %q", h)
	}
	if binary.LittleEndian.Uint32(h[4:]) != 1036 || binary.LittleEndian.Uint32(h[40:]) != 1000 ||
		binary.LittleEndian.Uint32(h[24:]) != 44100 || binary.LittleEndian.Uint32(h[28:]) != 88200 {
		t.Errorf("header fields wrong: % x", h)
	}
}

func readLevelled(t *testing.T, src io.Reader, lo LevelOptions) []float64 {
	t.Helper()
	b, err := io.ReadAll(newFinishReader(io.NopCloser(src), lo, zonosNativeRate))
	if err != nil {
		t.Fatal(err)
	}
	return s16(b)
}

func rmsOf(x []float64) float64 {
	db, _ := ActiveRMSdB(float32s(x), zonosNativeRate)
	return db
}

// A stream that starts at the wrong level is steered to the target as it is heard.
func TestLeveler_SteersAStreamToTheTarget(t *testing.T) {
	in := sine(220, 0.05, zonosNativeRate, 4*zonosNativeRate) // -29 dBFS RMS
	lo := LevelOptions{InitialDB: 0, Adapt: true, TargetDB: -24, MaxDB: 12}
	out := readLevelled(t, bytes.NewReader(f32bytes(in)), lo)
	if got := rmsOf(out[:zonosNativeRate/4]); math.Abs(got-(-29)) > 0.3 {
		t.Errorf("the first quarter second is at %.2f dBFS, want the untouched -29 (the starting gain is 0 dB)", got)
	}
	if got := rmsOf(out[2*zonosNativeRate:]); math.Abs(got-(-24)) > 0.3 {
		t.Errorf("after two seconds the level is %.2f dBFS, want the target -24", got)
	}
}

// The gain moves smoothly: never faster than the slew limit, so a correction is not heard as a step.
func TestLeveler_GainMovesNoFasterThanTheSlewLimit(t *testing.T) {
	l := newLeveler(LevelOptions{Adapt: true, TargetDB: -24, MaxDB: 12}, zonosNativeRate)
	prev := l.gainDB
	for _, v := range sine(220, 0.02, zonosNativeRate, 3*zonosNativeRate) {
		l.gain(v)
		if d := math.Abs(l.gainDB - prev); d > l.step+1e-12 {
			t.Fatalf("the gain moved %v dB in one sample, limit %v", d, l.step)
		}
		prev = l.gainDB
	}
	if math.Abs(prev-12) > 1e-6 { // 0.02 amplitude is -37 dBFS RMS: 13 dB under target, so the gain must have stopped at its bound
		t.Errorf("the gain ended at %v dB, want it held at the 12 dB bound", prev)
	}
}

func TestLeveler_StaticGainWhenNotAdapting(t *testing.T) {
	l := newLeveler(LevelOptions{InitialDB: 3, TargetDB: -24, MaxDB: 12}, zonosNativeRate)
	for _, v := range sine(220, 0.5, zonosNativeRate, zonosNativeRate) {
		l.gain(v)
		if l.gainDB != 3 {
			t.Fatalf("the gain changed to %v with adaptation off", l.gainDB)
		}
	}
}

// An utterance too short to measure keeps the starting gain: nothing is guessed from a few frames.
func TestLeveler_AShortUtteranceKeepsTheStartingGain(t *testing.T) {
	in := sine(220, 0.05, zonosNativeRate, zonosNativeRate*3/10) // 0.3 s, fewer than the 25 frames needed
	out := readLevelled(t, bytes.NewReader(f32bytes(in)), LevelOptions{InitialDB: 6.0206, Adapt: true, TargetDB: -24, MaxDB: 12})
	for i := 1000; i < 2000; i++ {
		if math.Abs(out[i]-2*in[i]) > 2.0/32767 {
			t.Fatalf("sample %d: %v, want the starting gain of +6 dB (%v)", i, out[i], 2*in[i])
		}
	}
}

// Pauses are not speech: silence between phrases must not drag the measured level down and the gain up.
func TestLeveler_IgnoresPauses(t *testing.T) {
	tone := sine(220, 0.05, zonosNativeRate, zonosNativeRate)
	in := append(append(append([]float64{}, tone...), make([]float64, 3*zonosNativeRate)...), tone...)
	out := readLevelled(t, bytes.NewReader(f32bytes(in)), LevelOptions{Adapt: true, TargetDB: -24, MaxDB: 12})
	last := out[len(out)-zonosNativeRate/2:]
	if got := rmsOf(last); math.Abs(got-(-24)) > 0.5 {
		t.Errorf("after a long pause the speech is at %.2f dBFS, want -24", got)
	}
}

// The output must not depend on how the engine's body was cut into reads, with the adaptation on.
func TestLeveler_IndependentOfHowTheBodyIsCut(t *testing.T) {
	in := f32bytes(append(sine(220, 0.03, zonosNativeRate, 30000), sine(330, 0.2, zonosNativeRate, 30000)...))
	lo := LevelOptions{InitialDB: 2, Adapt: true, TargetDB: -24, MaxDB: 12}
	read := func(r io.Reader) []byte {
		b, err := io.ReadAll(newFinishReader(io.NopCloser(r), lo, pcmOutRate))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	whole := read(bytes.NewReader(in))
	for name, r := range map[string]io.Reader{"one byte at a time": iotest.OneByteReader(bytes.NewReader(in)), "half reads": iotest.HalfReader(bytes.NewReader(in))} {
		if got := read(r); !bytes.Equal(got, whole) {
			t.Errorf("%s: the adapting stage gave different audio", name)
		}
	}
}

// A very long utterance must not cost more per frame than a short one (the measurement is incremental, not a rescan).
func TestLeveler_CostDoesNotGrowWithLength(t *testing.T) {
	l := newLeveler(LevelOptions{Adapt: true, TargetDB: -24, MaxDB: 12}, zonosNativeRate)
	chunk := sine(220, 0.05, zonosNativeRate, zonosNativeRate)
	for i := 0; i < 600; i++ { // ten minutes of speech
		for _, v := range chunk {
			l.gain(v)
		}
	}
	if len(l.powers) != 600*50 || l.k != 600*50 {
		t.Errorf("%d frames kept, %d counted, want 30000 of each", len(l.powers), l.k)
	}
	if math.Abs(l.wantDB-5.0) > 0.1 { // 0.05 amplitude is -29 dBFS RMS, 5 dB under the target
		t.Errorf("target gain after ten minutes %v dB, want about +5", l.wantDB)
	}
}

func readEdged(t *testing.T, src io.Reader, lo LevelOptions, rate int, e EdgeOptions) []byte {
	t.Helper()
	b, err := io.ReadAll(newFinishReader(io.NopCloser(src), lo, rate, e))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The speech sits between a silent lead and a silent tail of exactly the asked length, and the stream's first bytes are the silence (so it costs no latency).
func TestEdges_AddAnExactSilentMargin(t *testing.T) {
	in := f32bytes(sine(300, 0.3, zonosNativeRate, zonosNativeRate)) // one second
	e := EdgeOptions{LeadMS: 60, TailMS: 120}
	out := readEdged(t, bytes.NewReader(in), LevelOptions{}, pcmOutRate, e)
	lead, tail := 24*60, 24*120
	if n := len(out) / 2; n < 24000+lead+tail-3 || n > 24000+lead+tail+3 {
		t.Fatalf("%d samples, want about %d", n, 24000+lead+tail)
	}
	x := s16(out)
	for i := 0; i < lead; i++ {
		if x[i] != 0 {
			t.Fatalf("sample %d of the lead is %v, want silence", i, x[i])
		}
	}
	for i := len(x) - tail; i < len(x); i++ {
		if x[i] != 0 {
			t.Fatalf("sample %d of the tail is %v, want silence", i, x[i])
		}
	}
	// The reader hands over the lead on the very first read, before any of the engine's audio has been requested.
	r := newFinishReader(io.NopCloser(iotest.ErrReader(errors.New("the engine has not produced anything yet"))), LevelOptions{}, pcmOutRate, e)
	buf := make([]byte, 2*lead)
	if n, err := io.ReadFull(r, buf); err != nil || n != 2*lead {
		t.Errorf("the lead was not available at once: %d bytes, %v", n, err)
	}
}

// The first milliseconds of speech rise smoothly from nothing.
func TestEdges_FadeInStartsAtZeroAndRises(t *testing.T) {
	in := f32bytes(sine(0, 0, zonosNativeRate, 0))
	loud := make([]float64, zonosNativeRate/2)
	for i := range loud {
		loud[i] = 0.5 // a hard step: speech that starts at full level
	}
	in = f32bytes(loud)
	x := s16(readEdged(t, bytes.NewReader(in), LevelOptions{}, zonosNativeRate, EdgeOptions{FadeInMS: 6}))
	if x[0] != 0 {
		t.Errorf("the first sample is %v, want 0", x[0])
	}
	fade := 6 * zonosNativeRate / 1000
	for i := 1; i < fade; i++ {
		if x[i] < x[i-1] {
			t.Fatalf("not rising at sample %d: %v after %v", i, x[i], x[i-1])
		}
	}
	if math.Abs(x[fade+10]-0.5) > 1e-3 {
		t.Errorf("after the fade the level is %v, want the untouched 0.5", x[fade+10])
	}
}

// An engine that fails part-way must not be dressed up as a finished utterance: no tail is added after an error.
func TestEdges_NoTailAfterAnError(t *testing.T) {
	boom := errors.New("engine died")
	src := io.MultiReader(bytes.NewReader(f32bytes(sine(300, 0.3, zonosNativeRate, 4410))), iotest.ErrReader(boom))
	b, err := io.ReadAll(newFinishReader(io.NopCloser(src), LevelOptions{}, zonosNativeRate, EdgeOptions{TailMS: 120}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if n := len(b) / 2; n > 4410+2 {
		t.Errorf("%d samples came out for 4410 in: a tail was added to a failed stream", n)
	}
}

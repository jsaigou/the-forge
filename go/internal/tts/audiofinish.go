// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
)

// Audio finishing for the Zonos clone engine.
//
// ZONOS3-Mini streams float32 little-endian mono PCM at 44.1 kHz. forge-tts promises its callers OpenAI's "pcm" (24 kHz mono signed 16-bit
// little-endian) and a WAV file, so the engine's audio is finished here: a per-voice gain, a soft limiter, a sample-rate conversion (pcm only) and the
// integer encoding. The gain exists because the engine's loudness follows the reference clip and varies from clip to clip (measured 2026-11-20: a loud
// reference gave peaks within 0.1 dB of full scale and a quiet embedding clone sat 25 dB under it), so every voice is brought to one level.

const (
	zonosNativeRate = 44100
	// pcmOutRate is the rate of the "pcm" response format: what OpenAI's format of that name is, and what every forge-tts caller already reads.
	pcmOutRate = 24000

	// limiterKnee is where the soft limiter starts to bend (-4.5 dBFS) and limiterCeiling is where it stops (-1.5 dBFS). Below the knee a sample is untouched.
	// The ceiling leaves room for the true peak, which an oversampling meter or a player's own resampling or mp3 decode puts up to about 0.7 dB above the sample
	// peak (the first ceilings, -0.45 and then -1 dBFS, still let two voices read -0.3 and -0.5 dBTP).
	limiterKnee    = 0.5957
	limiterCeiling = 0.8414
)

// softLimit leaves |x| <= limiterKnee unchanged and bends everything above it smoothly towards limiterCeiling, so a hot passage is rounded, not clipped.
func softLimit(x float64) float64 {
	a := math.Abs(x)
	if a <= limiterKnee {
		return x
	}
	over := (a - limiterKnee) / (limiterCeiling - limiterKnee)
	y := limiterKnee + (limiterCeiling-limiterKnee)*math.Tanh(over)
	if x < 0 {
		return -y
	}
	return y
}

// ActiveRMSdB is the level of the speech in samples, in dBFS: the mean power of the 20 ms frames that are within 35 dB of the loudest one, so pauses and
// the room do not pull the figure down. ok is false when there is no signal to measure (silence, or fewer samples than one frame).
func ActiveRMSdB(samples []float32, rate int) (db float64, ok bool) {
	frame := rate / 50
	if frame <= 0 || len(samples) < frame {
		return 0, false
	}
	n := len(samples) / frame
	power := make([]float64, n)
	peak := 0.0
	for i := 0; i < n; i++ {
		sum := 0.0
		for _, s := range samples[i*frame : (i+1)*frame] {
			v := float64(s)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			sum += v * v
		}
		power[i] = sum / float64(frame)
		if power[i] > peak {
			peak = power[i]
		}
	}
	if peak < 1e-12 { // below -120 dBFS: nothing there
		return 0, false
	}
	gate := peak * math.Pow(10, -3.5)
	total, count := 0.0, 0
	for _, p := range power {
		if p >= gate {
			total += p
			count++
		}
	}
	return 10 * math.Log10(total/float64(count)), true
}

// resampler converts a mono stream from one rate to another with a windowed-sinc low-pass filter (Blackman window, 16 lobes), one block at a time.
// It keeps just the input it still needs, so a long utterance costs a constant amount of memory.
type resampler struct {
	inRate, outRate float64
	scale           float64   // low-pass cutoff relative to the input rate (1 = no decimation)
	half            float64   // kernel half-width in input samples
	in              []float64 // input not yet consumed
	pos             int64     // absolute index of in[0]
	next            int64     // index of the next output sample
}

func newResampler(inRate, outRate int) *resampler {
	const lobes = 16.0
	scale := math.Min(1, float64(outRate)/float64(inRate)) * 0.97
	return &resampler{inRate: float64(inRate), outRate: float64(outRate), scale: scale, half: lobes / scale}
}

func (r *resampler) kernel(x float64) float64 {
	if math.Abs(x) >= r.half {
		return 0
	}
	sx := r.scale * x
	sinc := 1.0
	if sx != 0 {
		sinc = math.Sin(math.Pi*sx) / (math.Pi * sx)
	}
	w := 0.42 + 0.5*math.Cos(math.Pi*x/r.half) + 0.08*math.Cos(2*math.Pi*x/r.half)
	return r.scale * sinc * w
}

// process adds block to the stream and returns every output sample that is now complete.
func (r *resampler) process(block []float64, out []float64) []float64 {
	r.in = append(r.in, block...)
	return r.drain(out)
}

// flush ends the stream: the tail is treated as silence so the last samples are produced.
func (r *resampler) flush(out []float64) []float64 {
	r.in = append(r.in, make([]float64, int(r.half)+2)...)
	return r.drain(out)
}

func (r *resampler) drain(out []float64) []float64 {
	ratio := r.inRate / r.outRate
	end := r.pos + int64(len(r.in))
	for {
		c := float64(r.next) * ratio
		if c+r.half >= float64(end) {
			break
		}
		lo := int64(math.Ceil(c - r.half))
		hi := int64(math.Floor(c + r.half))
		sum := 0.0
		for i := lo; i <= hi; i++ {
			idx := i - r.pos
			if idx < 0 || idx >= int64(len(r.in)) {
				continue // before the start of the stream: silence
			}
			sum += r.in[idx] * r.kernel(float64(i)-c)
		}
		out = append(out, sum)
		r.next++
	}
	// Forget input no later output needs.
	keepFrom := int64(math.Floor(float64(r.next)*ratio-r.half)) - 1
	if keepFrom > r.pos {
		drop := keepFrom - r.pos
		if drop > int64(len(r.in)) {
			drop = int64(len(r.in))
		}
		r.in = append(r.in[:0], r.in[drop:]...)
		r.pos += drop
	}
	return out
}

// LevelOptions control the gain stage of a finishReader.
type LevelOptions struct {
	// InitialDB is the gain applied before anything has been measured: the voice's calibrated gain.
	InitialDB float64
	// Adapt makes the gain follow the speech measured so far in this utterance, steering it to TargetDB. Without it InitialDB is applied unchanged.
	Adapt bool
	// TargetDB is the level, in dBFS of active-speech RMS, an adapting stage steers to.
	TargetDB float64
	// MaxDB bounds the gain either way (an adapting stage included).
	MaxDB float64
}

const (
	levelFrameMS      = 20   // the length of one measured frame
	levelMinFrames    = 25   // frames of speech measured before the adaptation is trusted (0.5 s)
	levelSlewDBPerSec = 24.0 // how fast the gain may move; a 3 dB correction takes an eighth of a second, too fast to hear as a step and too slow to pump
	levelGateDB       = 35.0 // frames more than this far under the loudest so far are pauses, not speech
)

// leveler measures the speech of a stream as it goes and moves a gain towards the one that puts it at the target. It sees every sample in order, so its
// output does not depend on how the stream was cut into reads.
type leveler struct {
	opt      LevelOptions
	frameLen int
	acc      float64 // sum of squares in the frame being filled
	n        int
	powers   []float64
	peak     float64
	sum      float64 // power of the frames at or above the gate, and how many there are
	k        int
	gainDB   float64
	wantDB   float64
	step     float64 // the most the gain may move per sample, in dB
}

func newLeveler(o LevelOptions, rate int) *leveler {
	return &leveler{opt: o, frameLen: rate * levelFrameMS / 1000, gainDB: o.InitialDB, wantDB: o.InitialDB, step: levelSlewDBPerSec / float64(rate)}
}

// gain takes the next raw sample and returns the linear gain to apply to it.
func (l *leveler) gain(x float64) float64 {
	if l.opt.Adapt {
		l.acc += x * x
		l.n++
		if l.n == l.frameLen {
			p := l.acc / float64(l.frameLen)
			l.powers = append(l.powers, p)
			l.acc, l.n = 0, 0
			if p > l.peak {
				l.peak = p
				l.regate() // a new loudest frame moves the gate: the only time every frame is looked at again
			} else if p >= l.peak*math.Pow(10, -levelGateDB/10) {
				l.sum += p
				l.k++
			}
			l.retarget()
		}
		switch d := l.wantDB - l.gainDB; {
		case d > l.step:
			l.gainDB += l.step
		case d < -l.step:
			l.gainDB -= l.step
		default:
			l.gainDB = l.wantDB
		}
	}
	return math.Pow(10, l.gainDB/20)
}

// regate recounts the frames that are at or above the gate under the current peak.
func (l *leveler) regate() {
	gate := l.peak * math.Pow(10, -levelGateDB/10)
	l.sum, l.k = 0, 0
	for _, p := range l.powers {
		if p >= gate {
			l.sum += p
			l.k++
		}
	}
}

// retarget recomputes the gain that puts the speech heard so far at the target.
func (l *leveler) retarget() {
	if l.peak < 1e-12 || l.k < levelMinFrames {
		return
	}
	g := l.opt.TargetDB - 10*math.Log10(l.sum/float64(l.k))
	l.wantDB = math.Max(-l.opt.MaxDB, math.Min(l.opt.MaxDB, g))
}

// EdgeOptions give an utterance a silent margin and a gentle start. The engine starts speech on the first sample (measured 2026-11-20: 15 of 21 clips began
// within 15 dB of the speech level) and often ends mid-word-level, and anything downstream that treats the edges roughly, a lossy encoder or a player that trims
// padding, then chops the first and last phonemes. A short margin of digital silence and a few milliseconds of fade-in remove that; the end is faded by the engine
// itself (fade_out_ms), so only the margin is added there.
type EdgeOptions struct {
	LeadMS   int // silence before the speech; it is sent at once, so it delays the speech but not the first byte
	TailMS   int // silence after the speech
	FadeInMS int // raised-cosine fade over the first milliseconds of speech
}

// finishReader turns the engine's float32 PCM body into finished signed 16-bit little-endian PCM at outRate: fade-in, level, soft limiter, resample, with a
// silent margin at both ends.
type finishReader struct {
	src     io.ReadCloser
	lv      *leveler
	edge    EdgeOptions
	outRate int
	nIn     int        // input samples seen, for the fade-in
	rs      *resampler // nil when the output rate is the engine's
	pending []byte     // bytes of the sample that straddles two reads
	out     bytes.Buffer
	tmp     []byte
	done    bool
	err     error
}

func newFinishReader(src io.ReadCloser, lo LevelOptions, outRate int, edges ...EdgeOptions) *finishReader {
	f := &finishReader{src: src, lv: newLeveler(lo, zonosNativeRate), tmp: make([]byte, 16*1024), outRate: outRate}
	if len(edges) > 0 {
		f.edge = edges[0]
	}
	if outRate != zonosNativeRate {
		f.rs = newResampler(zonosNativeRate, outRate)
	}
	f.silence(f.edge.LeadMS)
	return f
}

// silence queues ms of digital silence.
func (f *finishReader) silence(ms int) {
	if ms > 0 {
		f.out.Write(make([]byte, 2*f.outRate*ms/1000))
	}
}

func (f *finishReader) encode(samples []float64) {
	var b [2]byte
	for _, v := range samples {
		v = softLimit(v)
		n := int(math.Round(v * 32767))
		if n > 32767 {
			n = 32767
		} else if n < -32768 {
			n = -32768
		}
		binary.LittleEndian.PutUint16(b[:], uint16(int16(n)))
		f.out.Write(b[:])
	}
}

func (f *finishReader) Read(p []byte) (int, error) {
	for f.out.Len() == 0 && !f.done {
		n, err := f.src.Read(f.tmp)
		if n > 0 {
			data := append(f.pending, f.tmp[:n]...)
			whole := len(data) / 4 * 4
			f.pending = append([]byte(nil), data[whole:]...)
			block := make([]float64, 0, whole/4)
			for i := 0; i < whole; i += 4 {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[i:])))
				if math.IsNaN(v) || math.IsInf(v, 0) {
					v = 0
				}
				if fn := f.edge.FadeInMS * zonosNativeRate / 1000; fn > 0 && f.nIn < fn {
					v *= 0.5 - 0.5*math.Cos(math.Pi*float64(f.nIn)/float64(fn))
				}
				f.nIn++
				block = append(block, v*f.lv.gain(v))
			}
			if f.rs != nil {
				block = f.rs.process(block, nil)
			}
			f.encode(block)
		}
		if err != nil {
			f.done = true
			if err != io.EOF {
				f.err = err
			}
			if f.rs != nil {
				f.encode(f.rs.flush(nil))
			}
			if f.err == nil {
				f.silence(f.edge.TailMS)
			}
		}
	}
	if f.out.Len() > 0 {
		return f.out.Read(p)
	}
	if f.err != nil {
		return 0, f.err
	}
	return 0, io.EOF
}

func (f *finishReader) Close() error { return f.src.Close() }

// wavHeader is the 44-byte header of a 16-bit mono PCM WAV file holding dataLen bytes of audio.
func wavHeader(dataLen, rate int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	return b.Bytes()
}

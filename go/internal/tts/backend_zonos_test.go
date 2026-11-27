// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jsaigou/the-forge/internal/specialist"
)

// kindResolver hands out one engine of a given kind and counts the leases it gave back.
type kindResolver struct {
	url      string
	kind     string
	released atomic.Int32
}

func (k *kindResolver) Resolve(_ context.Context, _ VoiceMode, _ string) (Resolved, error) {
	return Resolved{URL: k.url, Kind: k.kind, Engine: "zonos-clone", Release: func() { k.released.Add(1) }}, nil
}
func (k *kindResolver) Status() map[string]any { return map[string]any{} }

// fakeZonos stands in for zonos3-server: it caches clips per session, answers /v2/generate with a tone of a chosen amplitude, and can forget its voices.
type fakeZonos struct {
	mu        sync.Mutex
	ts        *httptest.Server
	amp       float64            // peak amplitude of the tone it generates
	ampBy     map[string]float64 // amplitude by speaker_mode, when it differs (a missing key uses amp)
	speakers  map[string]string
	registers int
	gens      []map[string]any
	sessions  []string
	failReg   int    // answer /v2/speakers with this status when non-zero
	failLabel string // refuse to cache the clip with this label
	// steering: the heads the engine reports, whether it has a steering bundle, whether it cuts the stream of a steered request, and how much a whisper span lowers the level
	laci       []string
	secs       int // seconds of audio each generation returns (0 means 1)
	noSteer    bool
	cutSteered bool
	n          int
}

func newFakeZonos(t *testing.T, amp float64) *fakeZonos {
	f := &fakeZonos{amp: amp, speakers: map[string]string{}}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v2/speakers":
			if r.Method == http.MethodGet {
				list := []map[string]any{}
				for id, label := range f.speakers {
					list = append(list, map[string]any{"id": id, "label": label})
				}
				json.NewEncoder(w).Encode(map[string]any{"speakers": list})
				return
			}
			if f.failReg != 0 {
				http.Error(w, "boom", f.failReg)
				return
			}
			var req map[string]any
			json.Unmarshal(body, &req)
			if f.failLabel != "" && req["label"] == f.failLabel {
				http.Error(w, "cannot decode this clip", http.StatusUnprocessableEntity)
				return
			}
			f.registers++
			f.n++
			id := "spk_" + string(rune('a'+f.n))
			f.speakers[id] = req["label"].(string)
			f.sessions = append(f.sessions, r.Header.Get("X-TTS-Session-ID"))
			json.NewEncoder(w).Encode(map[string]any{"id": id, "label": req["label"]})
		case "/v2/capabilities":
			json.NewEncoder(w).Encode(map[string]any{"laci_languages": f.laci})
		case "/v2/controls":
			if f.noSteer {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"sets":{}}`))
		case "/v2/generate":
			var req map[string]any
			json.Unmarshal(body, &req)
			f.gens = append(f.gens, req)
			f.sessions = append(f.sessions, r.Header.Get("X-TTS-Session-ID"))
			if _, ok := f.speakers[req["speaker_embedding_id"].(string)]; !ok {
				// The engine's real wording, measured on ForgeHost: {"detail":"Unknown cached speaker id 'spk_...'."} with status 400.
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"detail": "Unknown cached speaker id '" + req["speaker_embedding_id"].(string) + "'."})
				return
			}
			w.Header().Set("X-Audio-Sample-Rate", "44100")
			if f.cutSteered && (req["control_spans"] != nil || req["controls"] != nil) {
				// What the engine did on ForgeHost with one voice, sad x6, seed 7: HTTP 200, some audio, then the connection closes with no terminating chunk.
				conn, bw, err := w.(http.Hijacker).Hijack()
				if err == nil {
					bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n4e20\r\n")
					bw.Write(make([]byte, 20000))
					bw.WriteString("\r\n")
					bw.Flush()
					conn.Close()
				}
				return
			}
			amp := f.amp
			if spans, ok := req["control_spans"].([]any); ok && len(spans) > 0 {
				amp *= 0.2 // a steered line comes back quieter, as a whisper does
			}
			if a, ok := f.ampBy[fmt.Sprint(req["speaker_mode"])]; ok {
				amp = a
			}
			secs := f.secs
			if secs == 0 {
				secs = 1
			}
			out := make([]byte, 4*44100*secs)
			for i := 0; i < 44100*secs; i++ {
				binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(float32(amp*math.Sin(2*math.Pi*220*float64(i)/44100))))
			}
			w.Write(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeZonos) forgetVoices() {
	f.mu.Lock()
	f.speakers = map[string]string{}
	f.mu.Unlock()
}

func zonosBackend(f *fakeZonos) (*serverBackend, *kindResolver) {
	r := &kindResolver{url: f.ts.URL, kind: kindZonos3Server}
	return NewServerBackend(r), r
}

func cloneSpec(id string) SynthSpec {
	return SynthSpec{Mode: ModeBase, ID: id, RefAudio: []byte("ref-" + id), RefText: "words", Language: "English"}
}

func rmsDB(pcm []byte) float64 {
	s := make([]float32, len(pcm)/2)
	for i := range s {
		s[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
	}
	db, _ := ActiveRMSdB(s, pcmOutRate)
	return db
}

func TestZonos_RegistersAndPrimesOncePerVoice(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, r := zonosBackend(f)
	for i := 0; i < 3; i++ {
		res, err := b.Speech(context.Background(), cloneSpec("sana"), "hello there")
		if err != nil {
			t.Fatal(err)
		}
		if res.Format != "wav" || len(res.Audio) < 44 || string(res.Audio[:4]) != "RIFF" {
			t.Fatalf("not a WAV: %q", res.Audio[:8])
		}
	}
	if f.registers != 1 {
		t.Errorf("the clip was registered %d times, want once", f.registers)
	}
	if primes := len(zonosPrime["en_us"]); len(f.gens) != primes+3 {
		t.Errorf("%d generations, want %d priming lines plus 3 requests", len(f.gens), primes)
	}
	g := f.gens[len(f.gens)-1]
	if g["text"] != "hello there" || g["language"] != "en_us" || g["speaker_mode"] != "reference" || g["stream"] != true ||
		g["silence_prefill_frames"] != float64(0) || g["speaker_embedding_id"] == "" {
		t.Errorf("request fields: %v", g)
	}
	if cv, _ := g["condition_values"].(map[string]any); cv["speaking_rate"] != float64(17) {
		t.Errorf("speaking rate: %v", g["condition_values"])
	}
	for _, s := range f.sessions {
		if s != "forge-tts" {
			t.Errorf("a call went without the session header: %q", s)
		}
	}
	if r.released.Load() != 3 {
		t.Errorf("%d leases given back for 3 requests", r.released.Load())
	}
}

// Every voice is brought to one level, whatever the engine gave it, and the correction is bounded.
func TestZonos_BringsEveryVoiceToTheTargetLevel(t *testing.T) {
	for name, tc := range map[string]struct {
		amp     float64
		wantDB  float64
		clamped bool
	}{
		"quiet voice raised": {0.05, -24, false},         // -29.0 dBFS RMS: +5 dB
		"loud voice lowered": {0.2, -24, false},          // -17.0 dBFS RMS: -7 dB
		"far too quiet":      {0.0005, -69.0 + 12, true}, // -69 dBFS RMS, raised by the +12 dB limit only
		"far too loud":       {0.9, -3.9 - 12, true},
	} {
		f := newFakeZonos(t, tc.amp)
		b, _ := zonosBackend(f)
		s, err := b.SpeechStream(context.Background(), cloneSpec("v"), "level check")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pcm, err := io.ReadAll(s.Body)
		s.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := rmsDB(pcm)
		tol := 0.3
		if tc.clamped { // the soft limiter bends the loud one a little; the quiet one sits where the clamp puts it
			tol = 1.5
		}
		if math.Abs(got-tc.wantDB) > tol {
			t.Errorf("%s: output level %.2f dBFS, want %.2f", name, got, tc.wantDB)
		}
	}
}

func TestZonos_PCMIsTheContractFormat(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, r := zonosBackend(f)
	s, err := b.SpeechStream(context.Background(), cloneSpec("v"), "stream")
	if err != nil {
		t.Fatal(err)
	}
	if s.Format != "pcm" {
		t.Fatalf("format %q", s.Format)
	}
	pcm, _ := io.ReadAll(s.Body)
	margin := DefaultZonosOptions().Edge
	want := 24000 + 24*(margin.LeadMS+margin.TailMS) // the engine's one second at 24 kHz, plus the silent margins
	if n := len(pcm) / 2; n < want-20 || n > want+20 {
		t.Errorf("%d samples: want about %d (24 kHz s16le with the margins)", n, want)
	}
	if r.released.Load() != 0 {
		t.Error("the lease was given back before the stream was closed")
	}
	s.Body.Close()
	s.Body.Close()
	if r.released.Load() != 1 {
		t.Errorf("lease released %d times after close, want once", r.released.Load())
	}

	res, err := b.Speech(context.Background(), cloneSpec("v"), "buffered")
	if err != nil {
		t.Fatal(err)
	}
	if rate := binary.LittleEndian.Uint32(res.Audio[24:]); rate != 44100 {
		t.Errorf("wav sample rate %d, want the engine's 44100", rate)
	}
	if dl := binary.LittleEndian.Uint32(res.Audio[40:]); int(dl) != len(res.Audio)-44 || dl != uint32(2*(44100+441*(margin.LeadMS+margin.TailMS)/10)) {
		t.Errorf("wav data length %d for %d bytes, want the engine's second plus the margins", dl, len(res.Audio))
	}
}

// An engine that restarted has lost its cached clips: the voice is registered again and the request still succeeds.
func TestZonos_ReRegistersAfterTheEngineForgets(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	if _, err := b.Speech(context.Background(), cloneSpec("v"), "one"); err != nil {
		t.Fatal(err)
	}
	f.forgetVoices()
	if _, err := b.Speech(context.Background(), cloneSpec("v"), "two"); err != nil {
		t.Fatalf("after the engine forgot: %v", err)
	}
	if f.registers != 2 {
		t.Errorf("registered %d times, want twice", f.registers)
	}
}

func TestZonos_AChangedReferenceIsRegisteredAgain(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	a := cloneSpec("v")
	if _, err := b.Speech(context.Background(), a, "x"); err != nil {
		t.Fatal(err)
	}
	a.RefAudio = []byte("a different recording")
	if _, err := b.Speech(context.Background(), a, "x"); err != nil {
		t.Fatal(err)
	}
	if f.registers != 2 {
		t.Errorf("registered %d times for two different recordings, want 2", f.registers)
	}
}

func TestZonos_ConcurrentFirstRequestsRegisterOnce(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Speech(context.Background(), cloneSpec("shared"), "hi"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.registers != 1 {
		t.Errorf("%d registrations for one new voice requested 8 times at once", f.registers)
	}
}

func TestZonos_SpeedScalesThePaceWithinTheModelsRange(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	for _, speed := range []float64{1.5, 10, 0.01} {
		spec := cloneSpec("v")
		spec.Speed = speed
		if _, err := b.Speech(context.Background(), spec, "pace"); err != nil {
			t.Fatal(err)
		}
	}
	rates := []float64{}
	for _, g := range f.gens[len(zonosPrime["en_us"]):] {
		rates = append(rates, g["condition_values"].(map[string]any)["speaking_rate"].(float64))
	}
	if rates[0] != 25.5 || rates[1] != zonosHighestRate || rates[2] != zonosLowestRate {
		t.Errorf("rates %v, want 25.5 then the model's own limits %v and %v", rates, zonosHighestRate, zonosLowestRate)
	}
}

func TestZonos_RefusesWhatItCannotRender(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, r := zonosBackend(f)
	noRef := cloneSpec("v")
	noRef.RefAudio = nil
	badLang := cloneSpec("v")
	badLang.Language = "Klingon"
	for name, spec := range map[string]SynthSpec{"no reference": noRef, "unknown language": badLang} {
		before := r.released.Load()
		if _, err := b.Speech(context.Background(), spec, "x"); err == nil {
			t.Errorf("%s: no error", name)
		}
		if r.released.Load() != before+1 {
			t.Errorf("%s: the lease was not given back", name)
		}
	}
	if f.registers != 0 {
		t.Error("a refused request still registered a voice")
	}
}

func TestZonos_EngineFailures(t *testing.T) {
	t.Run("registration refused is the engine's answer, not a retry", func(t *testing.T) {
		f := newFakeZonos(t, 0.1)
		f.failReg = http.StatusInternalServerError
		b, r := zonosBackend(f)
		_, err := b.Speech(context.Background(), cloneSpec("v"), "x")
		var ee *EngineError
		if err == nil || errors.As(err, &ee) || !strings.Contains(err.Error(), "register voice") {
			t.Errorf("err = %v", err)
		}
		if r.released.Load() != 1 {
			t.Error("lease not given back")
		}
	})
	t.Run("a silent reference is refused with a reason", func(t *testing.T) {
		f := newFakeZonos(t, 0)
		b, _ := zonosBackend(f)
		_, err := b.Speech(context.Background(), cloneSpec("v"), "x")
		if err == nil || !strings.Contains(err.Error(), "no audible speech") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an engine that is gone is retryable", func(t *testing.T) {
		f := newFakeZonos(t, 0.1)
		b, _ := zonosBackend(f)
		f.ts.Close()
		_, err := b.Speech(context.Background(), cloneSpec("v"), "x")
		var ee *EngineError
		if !errors.As(err, &ee) || !ee.Transient {
			t.Errorf("err = %v, want a transient EngineError", err)
		}
	})
}

func TestZonosLanguage(t *testing.T) {
	for in, want := range map[string]string{"": "en_us", "English": "en_us", "en": "en_us", "Japanese": "ja", " ja ": "ja", "Spanish": "es", "es": "es", "Chinese": "cmn",
		"pt-BR": "pt_br", "Korean": "ko"} {
		if got, err := zonosLanguage(in); err != nil || got != want {
			t.Errorf("zonosLanguage(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := zonosLanguage("Klingon"); err == nil {
		t.Error("an unknown language was accepted")
	}
}

// The Qwen path is untouched for every other kind, including a daemon that reports none.
func TestServerBackend_OtherKindsStillSpeakQwen(t *testing.T) {
	var hit string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit += r.URL.Path + " "
		w.Write([]byte("WAVDATA"))
	}))
	defer ts.Close()
	for _, kind := range []string{"", "qwen-tts-server"} {
		hit = ""
		b := NewServerBackend(&kindResolver{url: ts.URL, kind: kind})
		if _, err := b.Speech(context.Background(), SynthSpec{Mode: ModeVoiceDesign, Instruct: "calm"}, "hi"); err != nil {
			t.Fatal(err)
		}
		if hit != "/v1/audio/speech " {
			t.Errorf("kind %q went to %q", kind, hit)
		}
	}
}

func warmLog(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) { t.Logf(format, args...) }
}

// The warm-up registers every clone once, a second pass costs nothing, and a pass after the engine lost its cache puts them back before any request needs them.
func TestZonos_WarmClonesPreparesEveryVoiceAndRecoversFromARestart(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, r := zonosBackend(f)
	specs := []SynthSpec{cloneSpec("arlo"), cloneSpec("sana"), cloneSpec("reina")}
	if n, err := b.WarmClones(context.Background(), specs, warmLog(t)); err != nil || n != 3 {
		t.Fatalf("first pass: %d, %v", n, err)
	}
	if f.registers != 3 {
		t.Fatalf("%d registrations, want 3", f.registers)
	}
	if n, err := b.WarmClones(context.Background(), specs, warmLog(t)); err != nil || n != 3 || f.registers != 3 {
		t.Fatalf("second pass: %d ready, %d registrations, %v; want it to cost nothing", n, f.registers, err)
	}
	f.forgetVoices() // the engine restarted
	if n, err := b.WarmClones(context.Background(), specs, warmLog(t)); err != nil || n != 3 || f.registers != 6 {
		t.Fatalf("after a restart: %d ready, %d registrations, %v; want all three registered again", n, f.registers, err)
	}
	gensBefore := len(f.gens)
	if _, err := b.Speech(context.Background(), specs[1], "now fast"); err != nil {
		t.Fatal(err)
	}
	if len(f.gens)-gensBefore != 1 || f.registers != 6 {
		t.Errorf("a request after the warm-up still did work for the voice: %d generations, %d registrations", len(f.gens)-gensBefore, f.registers)
	}
	if r.released.Load() != 4 { // three warm-up passes' leases... one per pass, plus the request
		t.Errorf("%d leases given back, want 4 (three passes and one request)", r.released.Load())
	}
}

func TestZonos_WarmClonesLeavesOtherEnginesAlone(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b := NewServerBackend(&kindResolver{url: f.ts.URL, kind: "qwen-tts-server"})
	if n, err := b.WarmClones(context.Background(), []SynthSpec{cloneSpec("v")}, warmLog(t)); err != nil || n != 0 || f.registers != 0 {
		t.Errorf("a Qwen clone engine was touched: %d ready, %d registrations, %v", n, f.registers, err)
	}
}

func TestZonos_WarmClonesReportsAVoiceItCannotPrepareAndKeepsGoing(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.failLabel = "broken"
	b, _ := zonosBackend(f)
	var logged []string
	n, err := b.WarmClones(context.Background(), []SynthSpec{cloneSpec("broken"), cloneSpec("good")}, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil || n != 1 {
		t.Errorf("%d ready, %v; want the good voice ready and no error for the engine as a whole", n, err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], `"broken"`) || !strings.Contains(logged[0], "cannot decode") {
		t.Errorf("the failure was not reported with its reason: %q", logged)
	}
}

// This package does not import the pool, so it spells the kind the daemon reports itself. The two must be one string, or a Zonos engine would be spoken
// to in Qwen's protocol and fail on every request.
func TestKindZonos3ServerMatchesThePool(t *testing.T) {
	if kindZonos3Server != specialist.KindZonos3Server {
		t.Fatalf("tts spells the kind %q, the pool %q", kindZonos3Server, specialist.KindZonos3Server)
	}
}

// A buffered WAV is measured whole and gets one exact gain, so it lands on the target whatever level the engine gave.
func TestZonos_BufferedWavIsNormalisedExactly(t *testing.T) {
	for _, amp := range []float64{0.03, 0.1, 0.25} {
		f := newFakeZonos(t, amp)
		b, _ := zonosBackend(f)
		res, err := b.Speech(context.Background(), cloneSpec("v"), "exact")
		if err != nil {
			t.Fatal(err)
		}
		pcm := res.Audio[44:]
		s := make([]float32, len(pcm)/2)
		for i := range s {
			s[i] = float32(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		}
		if db, ok := ActiveRMSdB(s, zonosNativeRate); !ok || math.Abs(db-(-24)) > 0.15 {
			t.Errorf("amplitude %v: the WAV is at %.2f dBFS, want -24", amp, db)
		}
	}
}

func TestZonosUnknownSpeaker(t *testing.T) {
	for body, want := range map[string]bool{
		`{"detail":"Unknown cached speaker id 'spk_abc'."}`: true,
		`unknown voice 'x'`:             true,
		`{"detail":"text is required"}`: false,
		``:                              false,
	} {
		if got := zonosUnknownSpeaker([]byte(body)); got != want {
			t.Errorf("zonosUnknownSpeaker(%q) = %v, want %v", body, got, want)
		}
	}
}

func voiceIn(id, voiceLang, reqLang string) SynthSpec {
	sp := cloneSpec(id)
	sp.VoiceLanguage, sp.Language = voiceLang, reqLang
	return sp
}

func modesOf(f *fakeZonos, from int) []string {
	var out []string
	for _, g := range f.gens[from:] {
		out = append(out, fmt.Sprint(g["speaker_mode"]))
	}
	return out
}

// Speech in the voice's own language, in a measured language, is spoken in embedding mode; everything else in reference mode.
func TestZonos_ChoosesTheModeFromTheLanguages(t *testing.T) {
	for name, tc := range map[string]struct {
		voiceLang, reqLang, want string
	}{
		"english voice, english text":              {"English", "English", "embedding"},
		"english voice, british english":           {"English", "en-GB", "embedding"},
		"japanese voice, japanese text":            {"Japanese", "Japanese", "embedding"},
		"english voice speaking spanish":           {"English", "Spanish", "reference"},
		"japanese voice speaking english":          {"Japanese", "English", "reference"},
		"spanish voice, spanish text (unmeasured)": {"Spanish", "Spanish", "reference"},
		"voice whose language is not known":        {"", "English", "reference"},
	} {
		f := newFakeZonos(t, 0.1)
		b, _ := zonosBackend(f)
		if _, err := b.Speech(context.Background(), voiceIn("v", tc.voiceLang, tc.reqLang), "mode check"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := modesOf(f, len(f.gens)-1)[0]
		if got != tc.want {
			t.Errorf("%s: spoken in %s mode, want %s", name, got, tc.want)
		}
	}
}

// The two modes are asked for differently: the settings measured for each, and the engine's edge handling in both.
func TestZonos_EachModeGetsItsOwnSettings(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	for _, req := range []string{"English", "Spanish"} {
		if _, err := b.Speech(context.Background(), voiceIn("v", "English", req), "settings"); err != nil {
			t.Fatal(err)
		}
	}
	emb, ref := f.gens[len(f.gens)-2], f.gens[len(f.gens)-1]
	if emb["speaker_mode"] != "embedding" || ref["speaker_mode"] != "reference" {
		t.Fatalf("modes %v and %v", emb["speaker_mode"], ref["speaker_mode"])
	}
	ecv, _ := emb["condition_values"].(map[string]any)
	rcv, _ := ref["condition_values"].(map[string]any)
	if ecv["loudness"] != float64(-23) || ecv["speaking_rate"] != float64(14) {
		t.Errorf("embedding mode pins the loudness and the pace (14), got %v", ecv)
	}
	if rcv["speaking_rate"] != float64(17) || rcv["loudness"] != nil {
		t.Errorf("reference mode forces its pace (17) and leaves the loudness, got %v", rcv)
	}
	for name, g := range map[string]map[string]any{"embedding": emb, "reference": ref} {
		if g["speaker_cfg_scale"] != 1.5 || g["decode_context"] != true || g["fade_out_ms"] != float64(40) || g["silence_prefill_frames"] != float64(0) {
			t.Errorf("%s request: %v", name, g)
		}
	}
}

// A voice that will be spoken in embedding mode is calibrated in both modes, in its own language; one that will not is calibrated in reference mode only.
func TestZonos_CalibratesTheModesAVoiceWillUse(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	if _, err := b.Speech(context.Background(), voiceIn("ja", "Japanese", "Japanese"), "もしもし"); err != nil {
		t.Fatal(err)
	}
	primes := len(zonosPrime["ja"])
	if got := modesOf(f, 0)[:2*primes]; len(got) != 2*primes {
		t.Fatalf("%d priming generations, want %d", len(got), 2*primes)
	}
	for i, g := range f.gens[:2*primes] {
		want := "embedding"
		if i >= primes {
			want = "reference"
		}
		if g["speaker_mode"] != want || g["language"] != "ja" || !strings.ContainsAny(g["text"].(string), "あいうえおかきくけこ") {
			t.Fatalf("priming generation %d: %v, want %s mode in Japanese", i, g, want)
		}
	}
	f2 := newFakeZonos(t, 0.1)
	b2, _ := zonosBackend(f2)
	if _, err := b2.Speech(context.Background(), voiceIn("es", "Spanish", "Spanish"), "hola"); err != nil {
		t.Fatal(err)
	}
	for i, g := range f2.gens[:len(zonosPrime["en_us"])] {
		if g["speaker_mode"] != "reference" {
			t.Errorf("a voice that is never spoken in embedding mode was calibrated in %v (generation %d)", g["speaker_mode"], i)
		}
	}
}

// Each mode has its own gain: the engine gives one voice a different level in each.
func TestZonos_EachModeHasItsOwnLevelCorrection(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.ampBy = map[string]float64{"embedding": 0.03, "reference": 0.25}
	b, _ := zonosBackend(f)
	for req, name := range map[string]string{"English": "embedding", "Spanish": "reference"} {
		s, err := b.SpeechStream(context.Background(), voiceIn("v", "English", req), "level")
		if err != nil {
			t.Fatal(err)
		}
		pcm, _ := io.ReadAll(s.Body)
		s.Body.Close()
		margin := DefaultZonosOptions().Edge
		body := pcm[2*24*margin.LeadMS : len(pcm)-2*24*margin.TailMS]
		if got := rmsDB(body); math.Abs(got-(-24)) > 0.4 {
			t.Errorf("%s mode: %.2f dBFS, want -24 (its own correction)", name, got)
		}
	}
}

// Embedding mode that gives no audible speech for a voice does not fail the voice: it is spoken in reference mode, and the fallback is logged.
func TestZonos_FallsBackToReferenceWhenEmbeddingIsSilent(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.ampBy = map[string]float64{"embedding": 0}
	b, _ := zonosBackend(f)
	var logged []string
	b.zonos.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if _, err := b.Speech(context.Background(), voiceIn("v", "English", "English"), "fallback"); err != nil {
		t.Fatal(err)
	}
	if m := modesOf(f, len(f.gens)-1)[0]; m != "reference" {
		t.Errorf("spoken in %s mode, want reference", m)
	}
	found := false
	for _, l := range logged {
		found = found || strings.Contains(l, "no audible speech in embedding mode")
	}
	if !found {
		t.Errorf("the fallback was not logged: %q", logged)
	}
}

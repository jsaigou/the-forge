// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/tts/style"
)

// The ZONOS3-Mini clone engine. It replaces the Qwen3-TTS base engine for cloned voices; Qwen keeps voice design.
//
// Wire protocol, zonos3-server (Zyphra's ggml runtime):
//
//	POST /v2/speakers  {speaker_audio_base64, label}   header X-TTS-Session-ID   -> {id}      (cache a reference clip in the session)
//	POST /v2/generate  {text, language, stream, speaker_embedding_id, speaker_mode: "reference", ...}  -> float32 LE mono PCM, 44.1 kHz
//
// The first generation with a freshly cached clip encodes it (about 2.2 s on ForgeHost, measured 2026-11-20); later ones reuse the codes (first audio 43 ms).
// So a clone is registered once per engine instance and primed right away, and a request never pays for the encode. The priming also measures the level
// the engine gives this voice, which sets the voice's gain (see audiofinish.go).

// zonosMode is how the engine is told whose voice to speak in.
type zonosMode int

const (
	// zmEmbedding conditions on a 192-d speaker vector. It matches the reference voice far more closely than the reference mode (measured 2026-11-20 against the
	// engine's own speaker encoder: cosine 0.81-0.85 for same-language speech against 0.55-0.65), but it has no hold on the language: an English voice asked to
	// speak Spanish derailed in 8 of 8 clips.
	zmEmbedding zonosMode = iota
	// zmReference puts the encoded reference clip in the prompt. It is looser on identity and the only one of the two that speaks another language than the
	// voice's own.
	zmReference
)

func (m zonosMode) String() string {
	if m == zmEmbedding {
		return "embedding"
	}
	return "reference"
}

// ZonosModeOptions are the request settings of one mode.
type ZonosModeOptions struct {
	// SpeakerCFG is the guidance towards the speaker (1 = off). 1.5 raised identity in both modes; 2.0 made embedding mode derail on long English text.
	SpeakerCFG float64
	// SpeakingRate asks the model for this pace, in bytes of UTF-8 text per second of audio; 0 leaves it to the model. In embedding mode an unset rate gave
	// clips of very different length for one sentence (9.1, 8.7, 15.0 and 8.6 s for one voice); 14 held every clip within 1.02-1.09 of each other with no
	// derailed clip in 40, while 16 and above started to derail (measured 2026-11-20).
	SpeakingRate float64
	// LoudnessLUFS pins the loudness the model aims for (0 = leave it). The embedding carries no level information: without it a loud reference came out 25 dB
	// quiet.
	LoudnessLUFS float64
}

// ZonosOptions are the request settings forge-tts gives every Zonos generation. They are defaults for the engine, not per-voice values.
type ZonosOptions struct {
	Embedding ZonosModeOptions
	Reference ZonosModeOptions
	// EmbeddingFamilies are the language families (the part of the engine's language code before "_": "en", "ja") for which a request in the voice's own language
	// uses embedding mode. Only these were measured; any other language, and any request in a language other than the voice's, uses reference mode.
	EmbeddingFamilies []string
	// SilencePrefillFrames is the codec-silence the engine prepends before the model starts. Its default (3 frames) gave 0.26-0.9 s of silence before speech;
	// 0 starts at once (with DecodeContext the start is gentle, median -38 dB under the speech level).
	SilencePrefillFrames int
	// DecodeContext decodes the generated audio behind the reference context. With the silence prefill at 0 it is what stops the speech starting at full level.
	DecodeContext bool
	// FadeOutMS is the engine's own fade at the end of an utterance. It brought clips that ended within 15 dB of the speech level from 3 of 21 to 0.
	FadeOutMS int
	// Edge is the silent margin and fade-in forge-tts adds.
	Edge EdgeOptions
	// TargetLevelDB is the level, in dBFS of active-speech RMS, every voice is brought to.
	TargetLevelDB float64
	// AdaptLevel lets a streamed utterance steer its own gain to the target as it is heard, on top of the voice's calibrated starting gain. The engine's
	// loudness varies by 3-4 dB from clip to clip even for one voice (measured 2026-11-20), which a fixed gain cannot remove.
	AdaptLevel bool
	// MaxGainDB bounds the correction either way, so a failed measurement cannot turn into a silent or deafening voice.
	MaxGainDB float64
	// Session scopes the engine's speaker cache (header X-TTS-Session-ID).
	Session string
	// Logf, when set, is told what was measured for each voice as it is registered (its level and the gain chosen), so a wrong level can be traced.
	Logf func(format string, args ...any)
}

// DefaultZonosOptions are the settings measured and chosen for this engine.
func DefaultZonosOptions() ZonosOptions {
	return ZonosOptions{
		Embedding:            ZonosModeOptions{SpeakerCFG: 1.5, SpeakingRate: 14, LoudnessLUFS: -23},
		Reference:            ZonosModeOptions{SpeakerCFG: 1.5, SpeakingRate: 17},
		EmbeddingFamilies:    []string{"en", "ja"},
		SilencePrefillFrames: 0, DecodeContext: true, FadeOutMS: 40,
		Edge:          EdgeOptions{LeadMS: 60, TailMS: 120, FadeInMS: 6},
		TargetLevelDB: -24, AdaptLevel: true, MaxGainDB: 12, Session: "forge-tts",
	}
}

func (o ZonosOptions) forMode(m zonosMode) ZonosModeOptions {
	if m == zmEmbedding {
		return o.Embedding
	}
	return o.Reference
}

func langFamily(code string) string {
	if i := strings.IndexByte(code, '_'); i >= 0 {
		return code[:i]
	}
	return code
}

// eligibleForEmbedding reports whether a voice whose own language is voiceLang (the name voice-studio stores, "" if unknown) can be spoken in embedding mode
// when the request is in reqCode: the request must be in the voice's own language, and that language one of EmbeddingFamilies.
func (o ZonosOptions) eligibleForEmbedding(voiceLang, reqCode string) bool {
	if strings.TrimSpace(voiceLang) == "" {
		return false // a preview or an old entry: its language is not known, so do not guess
	}
	vc, err := zonosLanguage(voiceLang)
	if err != nil || langFamily(vc) != langFamily(reqCode) {
		return false
	}
	for _, f := range o.EmbeddingFamilies {
		if f == langFamily(vc) {
			return true
		}
	}
	return false
}

const (
	zonosLowestRate  = 6.5  // the model's lowest speaking-rate bucket edge
	zonosHighestRate = 29.0 // and its highest
	zonosNaturalRate = 14.0 // the middle of the pace the model chooses for itself (11-15)
)

// zonosPlan is what a line's style tags ask of the engine, already resolved against what this engine can do.
type zonosPlan struct {
	controls map[string]float64 // whole-line controls: hidden-space baseline plus (embedding mode only) age
	spans    []style.Span       // hidden-space controls over ranges of the spoken text; never overlapping
	follow   string             // "reading" when the engine has alignment heads for the language, else "rate"
	rate     *float64           // the line's own speaking rate, from the client's defaults
	unpin    bool               // a tempo tag is present: send no pinned speaking rate
	desc     string             // what was asked for, for the response header and for error messages
	notes    map[string]string
}

// zonosCaps is what an engine instance can do, fetched once and refreshed by the warm-up.
type zonosCaps struct {
	steer bool            // the steering bundle is loaded (GET /v2/controls answers)
	laci  map[string]bool // languages with alignment heads
}

// zonosVoice is what the engine holds for one clone: the speaker id it answered with, and the gain measured for it.
type zonosVoice struct {
	hash      string
	speakerID string
	gainDB    [2]float64 // per zonosMode: the engine gives a voice a different level in each
	embOK     bool       // embedding mode was calibrated for this voice and gave audible speech
}

// zonosLanguages maps what callers send (voice-studio's names, ISO codes) to the engine's text-normalisation codes.
var zonosLanguages = map[string]string{
	"english": "en_us", "en": "en_us", "en-us": "en_us", "en_us": "en_us", "en-gb": "en_gb", "en_gb": "en_gb",
	"japanese": "ja", "ja": "ja", "jpn": "ja",
	"spanish": "es", "es": "es",
	"french": "fr_fr", "fr": "fr_fr", "fr-fr": "fr_fr", "fr_fr": "fr_fr",
	"german": "de", "de": "de",
	"italian": "it", "it": "it",
	"portuguese": "pt_br", "pt": "pt_br", "pt-br": "pt_br", "pt_br": "pt_br",
	"chinese": "cmn", "zh": "cmn", "cmn": "cmn", "mandarin": "cmn",
	"korean": "ko", "ko": "ko",
	"russian": "ru", "ru": "ru",
	"arabic": "ar", "ar": "ar", "hindi": "hi", "hi": "hi", "dutch": "nl", "nl": "nl", "polish": "pl", "pl": "pl",
	"turkish": "tr", "tr": "tr", "ukrainian": "uk", "uk": "uk", "vietnamese": "vi", "vi": "vi", "thai": "th", "th": "th",
	"indonesian": "id", "id": "id", "swedish": "sv", "sv": "sv", "greek": "el", "el": "el", "hebrew": "he", "he": "he",
}

// zonosLanguage turns a caller's language into the engine's code. An empty language is English, as it is for Qwen. An unknown one is an error: the engine
// would read the text with the wrong normaliser and say nothing.
func zonosLanguage(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return "en_us", nil
	}
	if c, ok := zonosLanguages[n]; ok {
		return c, nil
	}
	return "", fmt.Errorf("zonos3-server: no support for the language %q (it reads English, Japanese, Spanish, French, German, Italian, Portuguese, Chinese, Korean and others by name or ISO code)", name)
}

// zonosPrime are the lines a freshly cached clone is made to say, to warm its codes and measure its level. Six, because one clip's level is only good to
// 3-4 dB and the voice's gain is the mean of them (two were measured to leave voices 3.5 dB off target). A language without lines of its own
// is primed with English: the level belongs to the voice, not to the language.
var zonosPrime = map[string][]string{
	"en_us": {
		"This is a short sample of my voice, used to set the level.",
		"The weather was mild, and the evening was quiet.",
		"Please call me back when you have a moment, there is something I would like to ask.",
		"We arrived at the station early, so we had time for a cup of tea.",
		"It was a long road, but every mile of it was worth the trouble.",
		"Could you tell me where the nearest pharmacy is, and whether it is still open?",
	},
	"ja": {
		"これは声の大きさを合わせるための短いサンプルです。",
		"今日は天気が良くて、夕方はとても静かでした。",
		"時間があるときに、折り返しお電話をいただけますか。",
		"駅に早く着いたので、お茶を飲む時間がありました。",
		"長い道のりでしたが、一歩一歩に意味がありました。",
		"一番近い薬局はどこにあるか、まだ開いているか教えてください。",
	},
}

func zonosPrimeLines(code string) (lines []string, lang string) {
	if l, ok := zonosPrime[code]; ok {
		return l, code
	}
	return zonosPrime["en_us"], "en_us"
}

// WithZonos sets the generation options for engines of the zonos3-server kind.
func (b *serverBackend) WithZonos(o ZonosOptions) *serverBackend {
	b.zonos = o
	return b
}

func (b *serverBackend) zonosKeyLock(key string) *sync.Mutex {
	l, _ := b.zonosLocks.LoadOrStore(key, &sync.Mutex{})
	return l.(*sync.Mutex)
}

func (b *serverBackend) zonosGenerate(ctx context.Context, base, speakerID, lang, text string, speed float64, mode zonosMode, plan *zonosPlan) (*http.Response, error) {
	mo := b.zonos.forMode(mode)
	payload := map[string]any{
		"text": text, "language": lang, "stream": true,
		"speaker_embedding_id": speakerID, "speaker_mode": mode.String(),
		"silence_prefill_frames": b.zonos.SilencePrefillFrames,
	}
	if b.zonos.DecodeContext {
		payload["decode_context"] = true
	}
	if b.zonos.FadeOutMS > 0 {
		payload["fade_out_ms"] = b.zonos.FadeOutMS
	}
	if mo.SpeakerCFG > 1 {
		payload["speaker_cfg_scale"] = mo.SpeakerCFG
	}
	cv := map[string]any{}
	rate := mo.SpeakingRate
	if plan != nil {
		if plan.rate != nil {
			rate = *plan.rate // the client's own pace for this line (voice-studio's per-voice setting)
		}
		if plan.unpin {
			rate = 0 // a tempo tag needs the pace free: the pinned rate would cancel it (measured: duration moved by under 5%)
		}
		if len(plan.controls) > 0 {
			payload["controls"] = plan.controls
		}
		if len(plan.spans) > 0 {
			payload["control_spans"] = plan.spans
			payload["control_span_follow"] = plan.follow
		}
	}
	if speed > 0 && speed != 1 && !(plan != nil && plan.unpin) {
		if rate == 0 {
			rate = zonosNaturalRate // a speed asks for a pace relative to something: the model's own, when none is forced
		}
		rate *= speed
	}
	if rate > 0 {
		cv["speaking_rate"] = math.Min(zonosHighestRate, math.Max(zonosLowestRate, rate))
	}
	if mo.LoudnessLUFS != 0 {
		cv["loudness"] = mo.LoudnessLUFS
	}
	if len(cv) > 0 {
		payload["condition_values"] = cv
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TTS-Session-ID", b.zonos.Session)
	return b.client.Do(req)
}

// zonosRegister caches the reference clip in the engine and primes it. It returns the voice with the gain measured for it.
func (b *serverBackend) zonosRegister(ctx context.Context, base string, spec SynthSpec, hash string) (zonosVoice, error) {
	body, err := json.Marshal(map[string]any{"speaker_audio_base64": base64.StdEncoding.EncodeToString(spec.RefAudio), "label": spec.ID})
	if err != nil {
		return zonosVoice{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/speakers", bytes.NewReader(body))
	if err != nil {
		return zonosVoice{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TTS-Session-ID", b.zonos.Session)
	resp, err := b.client.Do(req)
	if err != nil {
		return zonosVoice{}, err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zonosVoice{}, &engineAnswered{fmt.Errorf("zonos3-server register voice: %s: %s", resp.Status, strings.TrimSpace(string(data)))}
	}
	var sp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &sp); err != nil || sp.ID == "" {
		return zonosVoice{}, &engineAnswered{fmt.Errorf("zonos3-server register voice: the answer has no speaker id: %s", strings.TrimSpace(string(data)))}
	}

	// An unknown language is not an error here: the level belongs to the voice, so priming falls back to English. The request itself still refuses it.
	code, _ := zonosLanguage(spec.Language)
	if vc, err := zonosLanguage(spec.VoiceLanguage); err == nil && strings.TrimSpace(spec.VoiceLanguage) != "" {
		code = vc // prime in the voice's own language when it is known
	}
	lines, lang := zonosPrimeLines(code)
	v := zonosVoice{hash: hash, speakerID: sp.ID}
	for _, mode := range []zonosMode{zmEmbedding, zmReference} {
		if mode == zmEmbedding && !b.zonos.eligibleForEmbedding(spec.VoiceLanguage, code) {
			continue // the voice is never spoken in embedding mode: do not spend the time
		}
		var sum float64
		var n int
		for _, line := range lines {
			pcm, err := b.zonosReadAll(ctx, base, sp.ID, lang, line, mode)
			if err != nil {
				return zonosVoice{}, err
			}
			if db, ok := ActiveRMSdB(pcm, zonosNativeRate); ok {
				sum += db
				n++
			}
		}
		if n == 0 {
			if mode == zmReference {
				return zonosVoice{}, &engineAnswered{fmt.Errorf("zonos3-server: the reference of voice %q gave no audible speech while its level was measured", spec.ID)}
			}
			if b.zonos.Logf != nil {
				b.zonos.Logf("forge-tts: cloned voice %q gave no audible speech in embedding mode; it will use reference mode", spec.ID)
			}
			continue
		}
		gain := math.Max(-b.zonos.MaxGainDB, math.Min(b.zonos.MaxGainDB, b.zonos.TargetLevelDB-sum/float64(n)))
		v.gainDB[mode] = gain
		if mode == zmEmbedding {
			v.embOK = true
		}
		if b.zonos.Logf != nil {
			b.zonos.Logf("forge-tts: cloned voice %q is ready on the clone engine (%s mode): its speech measures %.1f dBFS over %d clips, so its starting gain is %+.1f dB (target %.1f)",
				spec.ID, mode, sum/float64(n), n, gain, b.zonos.TargetLevelDB)
		}
	}
	return v, nil
}

// zonosReadAll generates one utterance and returns it as samples (used for priming).
func (b *serverBackend) zonosReadAll(ctx context.Context, base, speakerID, lang, text string, mode zonosMode) ([]float32, error) {
	resp, err := b.zonosGenerate(ctx, base, speakerID, lang, text, 1, mode, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &engineAnswered{fmt.Errorf("zonos3-server speech: %s: %s", resp.Status, strings.TrimSpace(string(data)))}
	}
	return float32sFromLE(data), nil
}

// float32sFromLE reads little-endian float32 samples (a trailing partial sample is dropped).
func float32sFromLE(data []byte) []float32 {
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
	}
	return out
}

// zonosVoiceFor returns the engine's cached clone for spec, registering and priming it first if this engine instance does not hold exactly this reference.
// registeredNow reports whether it did, so a caller knows an "unknown voice" answer cannot be a stale cache.
func (b *serverBackend) zonosVoiceFor(ctx context.Context, base string, spec SynthSpec) (v zonosVoice, registeredNow bool, err error) {
	key := registrationKey(base, spec.ID)
	hash := registrationHash(spec)
	lock := b.zonosKeyLock(key) // two requests for one new voice must not register and prime it twice
	lock.Lock()
	defer lock.Unlock()
	b.regMu.Lock()
	have, ok := b.zonosVoices[key]
	b.regMu.Unlock()
	if ok && have.hash == hash {
		return have, false, nil
	}
	v, err = b.zonosRegister(ctx, base, spec, hash)
	if err != nil {
		return zonosVoice{}, false, err
	}
	b.regMu.Lock()
	b.zonosVoices[key] = v
	b.regMu.Unlock()
	return v, true, nil
}

func (b *serverBackend) zonosForget(base, name string) {
	b.regMu.Lock()
	delete(b.zonosVoices, registrationKey(base, name))
	b.regMu.Unlock()
}

// zonosRequest renders spec on a zonos3-server engine and returns the finished audio as an *http.Response whose body is what the Qwen path returns for the
// same format: "pcm" is 24 kHz mono signed 16-bit little-endian, streamed as it is made; "wav" is a buffered 16-bit WAV file at the engine's own 44.1 kHz.
func (b *serverBackend) zonosRequest(ctx context.Context, res Resolved, spec SynthSpec, text, format string) (*http.Response, func(), error) {
	fail := func(err error) (*http.Response, func(), error) {
		res.Release()
		return nil, nil, err
	}
	if len(spec.RefAudio) == 0 {
		return fail(fmt.Errorf("zonos3-server clones from a reference recording and the voice %q has none", spec.ID))
	}
	lang, err := zonosLanguage(spec.Language)
	if err != nil {
		return fail(err)
	}
	if spec.ID == "" {
		// A preview has no stored voice; name the clone by what it is made from, so repeated previews of one recording share an entry.
		sum := sha256.Sum256(spec.RefAudio)
		spec.ID = "preview-" + hex.EncodeToString(sum[:8])
	}
	base := res.URL

	voice, registeredNow, err := b.zonosVoiceFor(ctx, base, spec)
	if err != nil {
		return fail(b.zonosError(ctx, err))
	}
	mode := b.zonosModeFor(voice, spec, lang)
	plan := b.zonosPlanFor(ctx, base, spec, lang, mode)
	resp, err := b.zonosGenerate(ctx, base, voice.speakerID, lang, text, spec.Speed, mode, plan)
	if err != nil {
		return fail(b.engineDown(ctx, err))
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// The engine restarted and lost its cached voices: register again and ask once more.
		if !registeredNow && resp.StatusCode == http.StatusBadRequest && zonosUnknownSpeaker(data) {
			b.zonosForget(base, spec.ID)
			if voice, _, err = b.zonosVoiceFor(ctx, base, spec); err != nil {
				return fail(b.zonosError(ctx, err))
			}
			mode = b.zonosModeFor(voice, spec, lang)
			plan = b.zonosPlanFor(ctx, base, spec, lang, mode)
			if resp, err = b.zonosGenerate(ctx, base, voice.speakerID, lang, text, spec.Speed, mode, plan); err != nil {
				return fail(b.engineDown(ctx, err))
			}
			if resp.StatusCode == http.StatusOK {
				return b.zonosFinish(resp, res, voice, mode, format, plan)
			}
			data, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		return fail(fmt.Errorf("zonos3-server speech: %s: %s", resp.Status, strings.TrimSpace(string(data))))
	}
	return b.zonosFinish(resp, res, voice, mode, format, plan)
}

// zonosModeFor chooses how a request is spoken: embedding mode for speech in the voice's own language when that mode is calibrated for the voice, reference
// mode otherwise (another language, an unmeasured language, a voice whose language is not known).
func (b *serverBackend) zonosModeFor(v zonosVoice, spec SynthSpec, reqCode string) zonosMode {
	if v.embOK && b.zonos.eligibleForEmbedding(spec.VoiceLanguage, reqCode) {
		return zmEmbedding
	}
	return zmReference
}

// zonosUnknownSpeaker reports whether an engine's 400 body says the cached speaker is not there. The engine's own wording (measured 2026-11-20 after a
// restart) is {"detail":"Unknown cached speaker id 'spk_...'."}; "unknown voice" is what its OpenAI-style route says for the same thing.
func zonosUnknownSpeaker(body []byte) bool {
	b := strings.ToLower(string(body))
	return strings.Contains(b, "unknown cached speaker") || strings.Contains(b, "unknown voice")
}

// zonosError keeps an engine's refusal as it is and turns a failure to reach the engine into the retryable answer every other path gives.
func (b *serverBackend) zonosError(ctx context.Context, err error) error {
	var answered *engineAnswered
	if errors.As(err, &answered) || ctx.Err() != nil {
		return err
	}
	return b.engineDown(ctx, err)
}

// zonosFinish wraps the engine's body in the finishing stage for format.
func (b *serverBackend) zonosFinish(resp *http.Response, res Resolved, voice zonosVoice, mode zonosMode, format string, plan *zonosPlan) (*http.Response, func(), error) {
	level := LevelOptions{InitialDB: voice.gainDB[mode], Adapt: b.zonos.AdaptLevel, TargetDB: b.zonos.TargetLevelDB, MaxDB: b.zonos.MaxGainDB}
	var notes http.Header
	var src io.ReadCloser = resp.Body
	if plan != nil {
		notes = plan.header()
		src = &steeredBody{ReadCloser: resp.Body, desc: plan.desc}
	}
	if format == "pcm" {
		fr := newFinishReader(src, level, pcmOutRate, b.zonos.Edge)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: notes, Body: fr}, res.Release, nil
	}
	// A buffered answer is whole before it is sent, so its level is measured exactly and one gain is applied: no adaptation needed.
	defer resp.Body.Close()
	raw, err := io.ReadAll(src)
	if err != nil {
		res.Release()
		return nil, nil, b.engineDown(context.Background(), fmt.Errorf("the engine's audio stopped early: %w", err))
	}
	level.Adapt = false
	if db, ok := ActiveRMSdB(float32sFromLE(raw), zonosNativeRate); ok {
		level.InitialDB = math.Max(-b.zonos.MaxGainDB, math.Min(b.zonos.MaxGainDB, b.zonos.TargetLevelDB-db))
	}
	pcm, err := io.ReadAll(newFinishReader(io.NopCloser(bytes.NewReader(raw)), level, zonosNativeRate, b.zonos.Edge))
	if err != nil {
		res.Release()
		return nil, nil, b.engineDown(context.Background(), fmt.Errorf("finishing the engine's audio failed: %w", err))
	}
	wav := append(wavHeader(len(pcm), zonosNativeRate), pcm...)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: notes, Body: io.NopCloser(bytes.NewReader(wav))}, res.Release, nil
}

// header is the plan's notes as response headers.
func (p *zonosPlan) header() http.Header {
	h := http.Header{}
	for k, v := range p.notes {
		h.Set(k, v)
	}
	return h
}

// zonosNotes reads the style notes a Zonos response carries.
func zonosNotes(resp *http.Response) map[string]string {
	if resp == nil || resp.Header == nil {
		return nil
	}
	out := map[string]string{}
	for _, k := range []string{headerStyleApplied, headerStyleWarnings} {
		if v := resp.Header.Get(k); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// steeredBody names the steering that was active when the engine's response breaks off. A cut stream is a failure of the request, not something to retry quietly: the
// error says what was being asked, so the operator can tell a steering problem from an engine problem.
type steeredBody struct {
	io.ReadCloser
	desc string
}

func (s *steeredBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("the engine ended the response early while steering was active (%s): %w", s.desc, err)
	}
	return n, err
}

// zonosCapsFor returns what the engine at base can do. It is fetched once per engine instance and refreshed by the warm-up, so a request does not pay for it.
func (b *serverBackend) zonosCapsFor(ctx context.Context, base string, refresh bool) zonosCaps {
	b.regMu.Lock()
	if b.zonosCapsByBase == nil {
		b.zonosCapsByBase = map[string]zonosCaps{}
	}
	c, ok := b.zonosCapsByBase[base]
	b.regMu.Unlock()
	if ok && !refresh {
		return c
	}
	c = zonosCaps{laci: map[string]bool{}}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v2/capabilities", nil); err == nil {
		if resp, err := b.client.Do(req); err == nil {
			var caps struct {
				Laci []string `json:"laci_languages"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&caps) == nil {
				for _, l := range caps.Laci {
					c.laci[l] = true
				}
			}
			resp.Body.Close()
		}
	}
	if req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v2/controls", nil); err == nil {
		if resp, err := b.client.Do(req); err == nil {
			c.steer = resp.StatusCode == http.StatusOK
			resp.Body.Close()
		}
	}
	b.regMu.Lock()
	b.zonosCapsByBase[base] = c
	b.regMu.Unlock()
	return c
}

// zonosPlanFor resolves a request's style against what this engine can do in this mode. It returns nil when the request carries no style. Anything it cannot do is dropped
// with a warning, never silently: the response says what was applied and what was not.
func (b *serverBackend) zonosPlanFor(ctx context.Context, base string, spec SynthSpec, lang string, mode zonosMode) *zonosPlan {
	if spec.Style == nil {
		return nil
	}
	v := style.Default()
	r := style.Resolve(spec.Style.parsed, spec.Style.defaults, v)
	p := &zonosPlan{rate: r.Rate, unpin: r.Unpin, notes: map[string]string{}}
	warnings := append([]string(nil), r.Warnings...)
	if r.Active() {
		caps := b.zonosCapsFor(ctx, base, false)
		switch {
		case !caps.steer:
			warnings = append(warnings, "this engine has no steering bundle installed, so no style was applied")
			p.unpin = false
		default:
			p.controls = map[string]float64{}
			for c, val := range r.Global {
				p.controls[c] = val
			}
			if len(r.Raw) > 0 {
				if mode == zmEmbedding {
					for c, val := range r.Raw {
						p.controls[c] = val
					}
				} else {
					warnings = append(warnings, "age was ignored: this request is spoken in reference mode (a language other than the voice's own), and age only works in embedding mode")
				}
			}
			p.spans = r.Spans
			p.follow = "rate"
			if len(p.spans) > 0 {
				if caps.laci[lang] {
					p.follow = "reading"
				} else {
					warnings = append(warnings, "placement of the tags is approximate: the engine has no alignment heads for "+lang)
				}
			}
			if len(p.controls) == 0 {
				p.controls = nil
			}
		}
	}
	p.desc = r.Describe()
	if p.desc != "" && (p.controls != nil || len(p.spans) > 0) {
		p.notes[headerStyleApplied] = headerSafe(p.desc + " mode=" + mode.String())
	}
	if len(warnings) > 0 {
		p.notes[headerStyleWarnings] = headerSafe(strings.Join(warnings, "; "))
	}
	return p
}

// zonosForgetLost drops the cached clones an engine no longer lists (it restarted, and its speaker cache went with it), so the next warm-up registers them
// again instead of the first request finding out. It returns how many it dropped.
func (b *serverBackend) zonosForgetLost(ctx context.Context, base string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/speakers", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-TTS-Session-ID", b.zonos.Session)
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, &engineAnswered{fmt.Errorf("zonos3-server list voices: %s: %s", resp.Status, strings.TrimSpace(string(data)))}
	}
	var list struct {
		Speakers []struct {
			ID string `json:"id"`
		} `json:"speakers"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return 0, &engineAnswered{fmt.Errorf("zonos3-server list voices: unreadable answer: %w", err)}
	}
	present := map[string]bool{}
	for _, sp := range list.Speakers {
		present[sp.ID] = true
	}
	prefix := base + "\x00"
	dropped := 0
	b.regMu.Lock()
	for key, v := range b.zonosVoices {
		if strings.HasPrefix(key, prefix) && !present[v.speakerID] {
			delete(b.zonosVoices, key)
			dropped++
		}
	}
	b.regMu.Unlock()
	return dropped, nil
}

// WarmClones makes sure the engine that answers for cloned voices holds every one of specs, registered and primed, so no request pays for it. It does
// nothing when the engine for clones is not a zonos3-server. A voice that fails is reported through logf and the rest still go. It returns how many are
// ready and an error only when the engine could not be reached at all.
func (b *serverBackend) WarmClones(ctx context.Context, specs []SynthSpec, logf func(format string, args ...any)) (ready int, err error) {
	res, err := b.resolver.Resolve(ctx, ModeBase, "")
	if err != nil {
		return 0, err
	}
	defer res.Release()
	if res.Kind != kindZonos3Server {
		return 0, nil
	}
	b.zonosCapsFor(ctx, res.URL, true) // the engine may have restarted with a different bundle or heads
	if dropped, err := b.zonosForgetLost(ctx, res.URL); err != nil {
		return 0, b.zonosError(ctx, err)
	} else if dropped > 0 {
		logf("forge-tts: the clone engine lost %d cached voices (it restarted); registering them again", dropped)
	}
	for _, spec := range specs {
		if len(spec.RefAudio) == 0 {
			continue
		}
		if _, _, err := b.zonosVoiceFor(ctx, res.URL, spec); err != nil {
			logf("forge-tts: could not prepare the cloned voice %q: %v", spec.ID, err)
			continue
		}
		ready++
	}
	return ready, nil
}

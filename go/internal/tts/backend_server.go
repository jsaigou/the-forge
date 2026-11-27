package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// serverBackend proxies a resident tts-server process (qwentts.cpp/build/tts-server),
// which keeps one model loaded in GPU RAM for its lifetime and exposes an
// OpenAI-compatible API plus in-memory voice registration. One tts-server
// instance is expected per model variant; the backend routes by SynthSpec.Mode.
//
// tts-server API (src/tts-server.h):
//   POST /v1/audio/speech  {input, voice, instructions, response_format}
//   POST /v1/audio/voices  {name, ref_text, wav_b64}  (register/replace clone)
//   GET  /health
type serverBackend struct {
	client   *http.Client
	resolver Resolver

	// registered remembers which clone (by engine address and name) the engine already holds and the content hash it was registered with, so a request
	// does not upload and re-extract the same reference audio again: that cost about 600 ms of every cloned-voice request. An engine that restarted has
	// lost its voices; the first speech call then says "unknown voice", the entry is dropped and the clone registered again (see speechRequest).
	regMu      sync.Mutex
	registered map[string]string

	// Engines of the zonos3-server kind (backend_zonos.go): the options for their requests, the clones each one holds with the gain measured for it, and a
	// lock per clone so two requests for one new voice register and prime it once.
	zonos       ZonosOptions
	zonosVoices map[string]zonosVoice
	zonosLocks  sync.Map
	// zonosCapsByBase caches what each Zonos engine instance can do (steering bundle, alignment-head languages); guarded by regMu.
	zonosCapsByBase map[string]zonosCaps
}

// kindZonos3Server is the launch kind the daemon reports for the ZONOS3-Mini engine. It must equal specialist.KindZonos3Server; the daemon's own tests
// (httpapi) pin the two together, because this package does not import the pool.
const kindZonos3Server = "zonos3-server"

// NewServerBackend routes synthesis to whatever address resolver gives for each request. An engine that cannot be resolved is an answer (a clear error
// that says why), never a silent fall back to a slower path: the daemon said why, and falling back would hide it.
func NewServerBackend(resolver Resolver) *serverBackend {
	return &serverBackend{client: &http.Client{Timeout: 30 * time.Minute}, resolver: resolver, registered: map[string]string{},
		zonos: DefaultZonosOptions(), zonosVoices: map[string]zonosVoice{}}
}

func (b *serverBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	resp, release, err := b.speechRequest(ctx, spec, text, "wav")
	if err != nil {
		return nil, err
	}
	defer release()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &SpeechResult{Audio: data, Format: "wav", Notes: b.noteFor(resp, spec)}, nil
}

// noteFor is what the response says about style: an engine that steered put its own notes on the response (zonosRequest); any other engine just had the tags
// taken out of the text, and that is said.
func (b *serverBackend) noteFor(resp *http.Response, spec SynthSpec) map[string]string {
	if n := zonosNotes(resp); n != nil {
		return n
	}
	return spec.Style.ignoredNotes("this engine cannot be steered")
}

// SpeechStream starts a synthesis and hands back the engine's audio as it is made (response_format "pcm": 24 kHz mono signed 16-bit little-endian,
// which is also what OpenAI's pcm format is). The engine's lease is held until the returned body is closed.
func (b *serverBackend) SpeechStream(ctx context.Context, spec SynthSpec, text string) (*SpeechStream, error) {
	resp, release, err := b.speechRequest(ctx, spec, text, "pcm")
	if err != nil {
		return nil, err
	}
	return &SpeechStream{Body: &releasingBody{ReadCloser: resp.Body, release: release}, Format: "pcm", Notes: b.noteFor(resp, spec)}, nil
}

// releasingBody gives the engine lease back when the audio has been read or abandoned.
type releasingBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releasingBody) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// speechRequest resolves an engine, makes sure a clone is registered on it, and posts the synthesis. It returns the engine's 200 response with its body
// unread; release gives the lease back and is the caller's to call once the body is done. An error has already released it.
func (b *serverBackend) speechRequest(ctx context.Context, spec SynthSpec, text, format string) (*http.Response, func(), error) {
	res, err := b.resolver.Resolve(ctx, spec.Mode, spec.Engine)
	if err != nil {
		return nil, nil, err // the daemon decided (or could not be reached): say so
	}
	if res.Kind == kindZonos3Server {
		return b.zonosRequest(ctx, res, spec, text, format)
	}
	base := res.URL
	fail := func(err error) (*http.Response, func(), error) {
		res.Release()
		return nil, nil, err
	}

	wantClone := spec.Mode == ModeBase && len(spec.RefAudio) > 0
	if wantClone && spec.ID == "" {
		// A preview has no stored voice and so no id. The engine needs a name to register the clone under, and the same reference must get the same name
		// so repeated previews do not pile up in the engine: name it by what it is made from.
		sum := sha256.Sum256(append([]byte(spec.RefText+"\x00"), spec.RefAudio...))
		spec.ID = "preview-" + hex.EncodeToString(sum[:8])
	}
	registeredNow := false
	if wantClone {
		var rerr error
		registeredNow, rerr = b.ensureRegistered(ctx, base, spec)
		if rerr != nil {
			var answered *engineAnswered
			if errors.As(rerr, &answered) {
				return fail(rerr) // the engine answered and said no: that is not "not ready"
			}
			return fail(b.engineDown(ctx, rerr))
		}
	}

	voice := spec.Speaker
	if spec.Mode == ModeBase {
		voice = spec.ID
	} else if spec.Mode == ModeVoiceDesign {
		voice = ""
	}
	body, err := json.Marshal(map[string]any{
		"input":           text,
		"voice":           voice,
		"instructions":    spec.Instruct,
		"response_format": format,
	})
	if err != nil {
		return fail(err)
	}
	post := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		return b.client.Do(httpReq)
	}
	resp, err := post()
	if err != nil {
		return fail(b.engineDown(ctx, err))
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		// A clone remembered as registered that the engine does not know: it restarted and lost its voices. Register it again and ask once more.
		if wantClone && !registeredNow && strings.Contains(string(data), "unknown voice") {
			b.forgetRegistration(base, spec.ID)
			if _, rerr := b.ensureRegistered(ctx, base, spec); rerr != nil {
				var answered *engineAnswered
				if errors.As(rerr, &answered) {
					return fail(rerr)
				}
				return fail(b.engineDown(ctx, rerr))
			}
			if resp, err = post(); err != nil {
				return fail(b.engineDown(ctx, err))
			}
			if resp.StatusCode == http.StatusOK {
				return resp, res.Release, nil
			}
			data, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		return fail(fmt.Errorf("tts-server speech: %s: %s", resp.Status, strings.TrimSpace(string(data))))
	}
	return resp, res.Release, nil
}

func registrationKey(base, name string) string { return base + "\x00" + name }

func registrationHash(spec SynthSpec) string {
	sum := sha256.Sum256(append([]byte(spec.RefText+"\x00"), spec.RefAudio...))
	return hex.EncodeToString(sum[:])
}

// ensureRegistered registers the clone unless this engine already holds exactly this reference under this name. It reports whether it registered now.
func (b *serverBackend) ensureRegistered(ctx context.Context, base string, spec SynthSpec) (bool, error) {
	key, hash := registrationKey(base, spec.ID), registrationHash(spec)
	b.regMu.Lock()
	have := b.registered[key]
	b.regMu.Unlock()
	if have == hash {
		return false, nil
	}
	if err := b.registerVoice(ctx, base, spec); err != nil {
		return false, err
	}
	b.regMu.Lock()
	b.registered[key] = hash
	b.regMu.Unlock()
	return true, nil
}

func (b *serverBackend) forgetRegistration(base, name string) {
	b.regMu.Lock()
	delete(b.registered, registrationKey(base, name))
	b.regMu.Unlock()
}

// engineDown turns a transport failure to an engine the daemon just handed out (it was stopped or evicted in between, or has crashed) into a
// retryable answer instead of a plain 500: another attempt asks the daemon again, which loads it.
func (b *serverBackend) engineDown(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return &EngineError{Code: "model_not_ready", Transient: true, RetryAfter: 2 * time.Second, Message: "the engine did not answer: " + err.Error()}
}

func (b *serverBackend) registerVoice(ctx context.Context, base string, spec SynthSpec) error {
	payload := map[string]any{
		"name":     spec.ID,
		"ref_text": spec.RefText,
		"wav_b64":  base64.StdEncoding.EncodeToString(spec.RefAudio),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/voices", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &engineAnswered{fmt.Errorf("tts-server register voice: %s: %s", resp.Status, strings.TrimSpace(string(data)))}
	}
	return nil
}

// engineAnswered marks an error that came from an engine which was reached and refused, as opposed to one that could not be reached.
type engineAnswered struct{ error }

func (e *engineAnswered) Unwrap() error { return e.error }

func (b *serverBackend) Health(ctx context.Context) (map[string]any, error) {
	// Addresses come from the daemon and asking for one may start an engine: health never does that. It reports what the resolver knows.
	out := map[string]any{"status": "ok", "engine": "tts-server (resolved per request by the daemon)"}
	for k, v := range b.resolver.Status() {
		out[k] = v
	}
	if out["degraded"] == true {
		out["status"] = "degraded"
	}
	return out, nil
}

func (b *serverBackend) Close() error { return nil }

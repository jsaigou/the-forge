// SPDX-License-Identifier: Apache-2.0

package router

// audio_adapter.go: OpenAI-shaped audio routes for catalog (slot) models (LFM3-Audio / Omni; plan Phase 2b, decision D9).
//
// llama-server has no /v1/audio/* routes. Transcription is a chat completion with an `input_audio` part and an ASR system
// prompt; speech is `POST /tts` at the server root. This file translates the OpenAI request shapes into those, and routes
// them through the same resolve / on-demand load / health / audit path as chat (tryBackends + routeOpts).
//
//   - POST /v1/audio/transcriptions: slot models only. A `model` that is not a catalog config with audio input is a 404
//     (forge-stt has no a0 route; whether it should is a later decision). Buffered, WAV/MP3 input, json or text output.
//   - POST /v1/audio/speech: the existing forge-tts passthrough stays the default (D7). Only a `model` that resolves to a
//     visible catalog config is taken by the slot adapter; any other name (tts-1, kokoro, unknown, absent) is forwarded to
//     forge-tts unchanged. Output is the slot's WAV, passed through unbuffered (no transcoding; no streaming).
//
// Neither route substitutes another model: a stand-in that cannot do audio is worse than an honest error.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

const (
	maxTranscriptionBytes = 64 << 20 // 64 MiB of multipart upload
	maxSpeechBodyBytes    = 1 << 20  // the speech body is a JSON object holding text
)

// asrLanguages maps the ISO-639-1 codes the OpenAI API uses to the English language name the model's ASR prompt expects.
var asrLanguages = map[string]string{
	"en": "English", "ja": "Japanese", "zh": "Chinese", "ko": "Korean", "es": "Spanish", "fr": "French",
	"de": "German", "it": "Italian", "pt": "Portuguese", "ru": "Russian", "ar": "Arabic", "hi": "Hindi",
	"nl": "Dutch", "tr": "Turkish", "pl": "Polish", "vi": "Vietnamese", "th": "Thai", "id": "Indonesian",
	"sv": "Swedish", "uk": "Ukrainian",
}

// asrPrompt is the system prompt for a transcription. With no language the model is asked to detect it.
func asrPrompt(code string) (string, bool) {
	if code == "" {
		return "Perform ASR.", true
	}
	name, ok := asrLanguages[strings.ToLower(code)]
	if !ok {
		return "", false
	}
	return "Perform ASR in " + name + ".", true
}

// audioInputFormat derives the `input_audio.format` llama-server accepts (wav, mp3) from the upload's name or type.
func audioInputFormat(filename, contentType string) (string, bool) {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(filename), ".")) {
	case "wav":
		return "wav", true
	case "mp3":
		return "mp3", true
	}
	switch strings.ToLower(contentType) {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav", true
	case "audio/mpeg", "audio/mp3":
		return "mp3", true
	}
	return "", false
}

func writeAudioValidation(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"fields": map[string]string{field: msg},
	})
}

// audioRequestCtx applies the timeout, auth and attribution every audio verb needs. ok=false means the response was written.
func (s *Server) audioRequestCtx(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, bool) {
	ctx := r.Context()
	cancel := func() {}
	if timeout := s.cfg().requestTimeout(); timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	auth := s.checkAuth(r)
	if !auth.ok {
		cancel()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
		return nil, nil, false
	}
	ctx = context.WithValue(ctx, clientAddrKey{},
		authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String())
	ctx = s.attributeConsumer(ctx, r, auth)
	return ctx, cancel, true
}

// audioConfig resolves model to a visible catalog config. found=false means no such catalog name (the caller decides
// whether that is a 404 or a passthrough); a hidden config has already been answered with a 404 (handled=true).
func (s *Server) audioConfig(ctx context.Context, w http.ResponseWriter, model string) (cfg store.Config, found, handled bool) {
	cfg, ok := s.targetConfig(ctx, model)
	if ok && cfg.Visibility != "hidden" {
		return cfg, true, false
	}
	if s.isHiddenModel(ctx, model) || (ok && cfg.Visibility == "hidden") {
		writeModelNotRoutable(w, model)
		return store.Config{}, false, true
	}
	return store.Config{}, false, false
}

func writeAudioCapability(w http.ResponseWriter, model, capability, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": errCodeUnsupportedCapability, "model": model, "capability": capability, "message": msg,
	})
}

// routeAudio runs the shared place-and-forward path for an audio verb.
func (s *Server) routeAudio(ctx context.Context, w http.ResponseWriter, r *http.Request, model string, body map[string]any,
	opts routeOpts, cl *chainLabel) {
	opts.noSubstitute = true
	opts.passUpstreamErrors = true
	ctx = withRouteOpts(ctx, opts)
	catChain, handled, errMsg, loadReason, _ := s.catalogChain(ctx, model, requestedByHeader(r), body)
	switch {
	case handled && catChain != nil:
		defer finishChain(catChain)
		s.tryBackends(ctx, w, r, model, body, catChain, 0, cl)
	case handled:
		resp := map[string]string{
			"error": "catalog_load_failed", "model": model, "detail": errMsg,
			"message": unavailableMessageFor(model, "", errMsg),
		}
		if loadReason != "" {
			resp["reason"] = string(loadReason)
		}
		writeJSON(w, http.StatusBadGateway, resp)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model_not_found", "model": model})
	}
}

// ---- transcriptions ----------------------------------------------------------------------------------------------

func (s *Server) audioTranscriptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, ok := s.audioRequestCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	if s.deps.Cfg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "router not configured"})
		return
	}

	// The raw body is kept: a request the specialist pool answers is forwarded as the client sent it.
	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptionBytes)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds 64 MiB limit"})
			return
		}
		writeAudioValidation(w, "body", "could not be read")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(rawBody))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeAudioValidation(w, "body", "must be multipart/form-data")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	model := r.FormValue("model")
	if len(model) < 1 || len(model) > 128 {
		writeAudioValidation(w, "model", "must be a string of 1-128 characters")
		return
	}
	cfgRow, found, handled := s.audioConfig(ctx, w, model)
	if handled {
		return
	}
	if !found {
		// Not a catalog model: the specialist pool may answer (a named member, else the transcription default), else it is a 404.
		if s.viaPool(ctx, w, r, store.FnTranscription, model, "/v1/audio/transcriptions", rawBody, r.Header.Get("Content-Type")) {
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model_not_found", "model": model})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeAudioValidation(w, "file", "an audio file is required")
		return
	}
	defer file.Close()
	format, ok := audioInputFormat(hdr.Filename, hdr.Header.Get("Content-Type"))
	if !ok {
		writeAudioValidation(w, "file", "must be WAV or MP3 (by file extension or content type)")
		return
	}
	prompt, ok := asrPrompt(r.FormValue("language"))
	if !ok {
		writeAudioValidation(w, "language", "unsupported language code")
		return
	}
	respFormat := r.FormValue("response_format")
	if respFormat == "" {
		respFormat = "json"
	}
	if respFormat != "json" && respFormat != "text" {
		writeAudioValidation(w, "response_format", "only json and text are supported")
		return
	}
	audio, err := io.ReadAll(file)
	if err != nil {
		writeAudioValidation(w, "file", "could not be read")
		return
	}

	if mod, missing := s.missingInputModality(ctx, cfgRow, false, true); missing {
		writeUnsupportedModality(w, model, mod)
		return
	}

	chatBody := map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []any{
			map[string]any{"role": "system", "content": prompt},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "input_audio", "input_audio": map[string]any{"data": base64Std(audio), "format": format},
			}}},
		},
	}
	raw, err := json.Marshal(chatBody)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not build request"})
		return
	}

	// The slot answers with a chat completion; capture it and re-shape it as an OpenAI transcription. A failed
	// upstream response is relayed as it came.
	cl := &chainLabel{}
	cw := newCaptureWriter()
	// The audit row is written from a callback that runs with this ctx, not the one tryBackends sees: it must carry the
	// action itself or the row is filed as a chat completion.
	auditCtx := withRouteOpts(ctx, routeOpts{auditAction: "transcription"})
	s.routeAudio(ctx, cw, r, model, chatBody, routeOpts{
		rawBody:     raw,
		auditAction: "transcription",
		onSuccessBody: func(buf []byte) {
			s.auditOutcome(auditCtx, model, "ok", cl.String()+","+chatUsageDetail(buf))
		},
	}, cl)
	cw.finish(w, func(body []byte) ([]byte, string, error) {
		var resp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
			return nil, "", fmt.Errorf("upstream answered without a transcription")
		}
		text := strings.TrimSpace(resp.Choices[0].Message.Content)
		if respFormat == "text" {
			return []byte(text + "\n"), "text/plain; charset=utf-8", nil
		}
		out, err := json.Marshal(map[string]string{"text": text})
		return out, "application/json", err
	})
}

// chatUsageDetail reads the token usage out of a chat completion for the audit row; a body that does not parse is
// recorded as such rather than dropped.
func chatUsageDetail(buf []byte) string {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return "usage=unparsed(" + err.Error() + ")"
	}
	return "input_tokens=" + strconv.Itoa(resp.Usage.PromptTokens) + ",output_tokens=" + strconv.Itoa(resp.Usage.CompletionTokens)
}

// ---- speech ------------------------------------------------------------------------------------------------------

// audioSpeech is POST /v1/audio/speech: the slot adapter for a catalog model, forge-tts for everything else.
func (s *Server) audioSpeech(forgeTTS http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.checkAuth(r)
		if !auth.ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
			return
		}
		flightFrom(r.Context()).setConsumer(s.consumerLabel(r, auth))
		r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBodyBytes)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds 1 MiB limit"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		restore := func() {
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}

		// Anything that is not a JSON object naming a catalog config is forge-tts's to answer, byte-for-byte as before.
		var body map[string]any
		var model string
		if decodeBodyExact(raw, &body) == nil {
			model, _ = body["model"].(string)
		}
		if model == "" || s.deps.Cfg == nil || s.deps.StoreCatalog == nil {
			restore()
			forgeTTS(w, r)
			return
		}
		ctx, cancel, ok := s.audioRequestCtx(w, r)
		if !ok {
			return
		}
		defer cancel()
		cfgRow, found, handled := s.audioConfig(ctx, w, model)
		if handled {
			return
		}
		if !found {
			restore()
			forgeTTS(w, r)
			return
		}
		if outs, known := s.resolveConfigOutputs(ctx, cfgRow); known && !hasOutput(outs.Enabled, store.OutputAudio) {
			writeAudioCapability(w, model, "speech", model+" cannot produce audio, so it cannot answer /v1/audio/speech. "+
				"List speech-capable models with GET /v1/models?output=audio.")
			return
		}

		input, _ := body["input"].(string)
		if strings.TrimSpace(input) == "" {
			writeAudioValidation(w, "input", "must be a non-empty string")
			return
		}
		if f, ok := body["response_format"].(string); ok && f != "" && f != "wav" {
			writeAudioValidation(w, "response_format", "only wav is supported for catalog models")
			return
		}
		if sp, ok := body["speed"].(json.Number); ok {
			if f, err := sp.Float64(); err != nil || f != 1 {
				writeAudioValidation(w, "speed", "not supported for catalog models")
				return
			}
		} else if _, present := body["speed"]; present && body["speed"] != nil {
			writeAudioValidation(w, "speed", "not supported for catalog models")
			return
		}
		tts := map[string]any{"input": input}
		if v, ok := body["voice"].(string); ok && v != "" {
			tts["voice"] = v
		}
		if l, ok := body["language"].(string); ok && l != "" {
			tts["lang"] = l
		}
		ttsRaw, err := json.Marshal(tts)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not build request"})
			return
		}
		cl := &chainLabel{}
		s.routeAudio(ctx, w, r, model, map[string]any{"model": model}, routeOpts{
			upstreamPath: "/tts", rootPath: true, rawBody: ttsRaw, auditAction: "speech",
		}, cl)
	}
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// captureWriter buffers a response so a handler can re-shape a successful upstream body. Failures are relayed untouched.
type captureWriter struct {
	hdr    http.Header
	status int
	buf    bytes.Buffer
}

func newCaptureWriter() *captureWriter { return &captureWriter{hdr: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.hdr }
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *captureWriter) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.buf.Write(p)
}
func (c *captureWriter) Flush() {}

// finish sends the captured response to w. A 2xx body goes through shape; every other response (and a 2xx that cannot be
// shaped, which becomes a 502 saying so) is relayed as the upstream gave it.
func (c *captureWriter) finish(w http.ResponseWriter, shape func([]byte) ([]byte, string, error)) {
	if c.status == 0 {
		c.status = http.StatusBadGateway
	}
	if c.status < 300 {
		out, ctype, err := shape(c.buf.Bytes())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad_upstream_response", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(c.status)
		_, _ = w.Write(out)
		return
	}
	for k, v := range c.hdr {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(c.buf.Len()))
	w.WriteHeader(c.status)
	_, _ = w.Write(c.buf.Bytes())
}

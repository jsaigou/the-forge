package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/duplexwire"
)

//go:embed web
var webFS embed.FS

// Options configures the server.
type Options struct {
	Store *Store
	// DuplexURL is the base URL of forge-duplex (the ForgeHost service, e.g. https://duplex.example.ts.net, or http://127.0.0.1:8086 in dev).
	DuplexURL string
	// DuplexToken is the long-lived forge-duplex bearer token. It stays on this server: the browser only ever receives a short-lived ticket.
	DuplexToken string
	// VoiceStudioURL is the base URL of voice-studio (https://voice-studio.example.ts.net). When set, a conversation can use a voice sample that
	// voice-studio speaks for it (sample.go); empty turns that off.
	VoiceStudioURL string
	// AllowedOrigins are browser origins allowed to change state besides the server's own host.
	AllowedOrigins []string
	Client         *http.Client
}

// Server is talk-studio's HTTP surface.
type Server struct {
	o   Options
	mux *http.ServeMux
}

func NewServer(o Options) *Server {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	o.DuplexURL = strings.TrimRight(o.DuplexURL, "/")
	o.VoiceStudioURL = strings.TrimRight(o.VoiceStudioURL, "/")
	s := &Server{o: o, mux: http.NewServeMux()}
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	m.HandleFunc("GET /api/config", s.handleConfig)
	m.HandleFunc("GET /api/models", s.handleModels)
	m.HandleFunc("PUT /api/models", s.handleSetModels)
	m.HandleFunc("GET /api/voices", s.handleVoices)
	m.HandleFunc("POST /api/voices", s.handleAddVoice)
	m.HandleFunc("GET /api/voices/{id}/audio", s.handleVoiceAudio)
	m.HandleFunc("PATCH /api/voices/{id}", s.handlePatchVoice)
	m.HandleFunc("DELETE /api/voices/{id}", s.handleDeleteVoice)
	m.HandleFunc("GET /api/vs/voices", s.handleVSVoices)
	m.HandleFunc("POST /api/sample", s.handleMakeSample)
	m.HandleFunc("GET /api/samples/{id}/audio", s.handleSampleAudio)
	m.HandleFunc("GET /api/cards", s.handleCards)
	m.HandleFunc("POST /api/cards", s.handleSaveCard)
	m.HandleFunc("DELETE /api/cards/{id}", s.handleDeleteCard)
	m.HandleFunc("POST /api/session", s.handleSession)
	m.HandleFunc("GET /api/recordings", s.handleRecordings)
	m.HandleFunc("POST /api/recordings", s.handleNewRecording)
	m.HandleFunc("GET /api/recordings/{id}", s.handleRecording)
	m.HandleFunc("PUT /api/recordings/{id}/audio/{stream}", s.handleAppendAudio)
	m.HandleFunc("POST /api/recordings/{id}/events", s.handleAppendEvents)
	m.HandleFunc("POST /api/recordings/{id}/finish", s.handleFinish)
	m.HandleFunc("GET /api/recordings/{id}/audio/{stream}", s.handleRecordingAudio)
	m.HandleFunc("DELETE /api/recordings/{id}", s.handleDeleteRecording)
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	m.Handle("GET /", noCache(http.FileServer(http.FS(sub))))
	return s
}

// Handler adds the origin check for every state-changing request: there is no login (the tailnet is the boundary), so a page on another origin
// must not be able to make a signed-in browser change anything.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.originOK(r) {
			writeErr(w, http.StatusForbidden, "forbidden_origin", "this request comes from another origin")
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser cross-site request: curl, the Go client, a same-origin fetch some browsers send without Origin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	for _, a := range s.o.AllowedOrigins {
		if a == origin {
			return true
		}
	}
	return false
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// fail maps a store error onto a status.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, "offset_conflict", "the upload offset does not match what is stored")
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func decode(r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// --- config and models -----------------------------------------------------------------------------------------------------------------

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	ms, err := s.o.Store.Models()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"models": ms, "frame": map[string]int{"sample_rate": sampleRate, "samples": 3200},
		"limits":            map[string]any{"reference_bytes": duplexwire.MaxReferenceAudioBytes, "card_bytes": duplexwire.MaxCardBytes},
		"duplex_configured": s.o.DuplexURL != "", "voice_studio_configured": s.o.VoiceStudioURL != "", "voice_studio_url": s.o.VoiceStudioURL,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.o.Store.Models()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, ms)
}

func (s *Server) handleSetModels(w http.ResponseWriter, r *http.Request) {
	var ms []Model
	if err := decode(r, &ms, 256<<10); err != nil {
		writeErr(w, 400, "bad_request", "the body must be a JSON list of models: "+err.Error())
		return
	}
	if err := s.o.Store.SetModels(ms); err != nil {
		writeErr(w, 422, "invalid_models", err.Error())
		return
	}
	writeJSON(w, 200, ms)
}

// --- voices ----------------------------------------------------------------------------------------------------------------------------

func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	vs, err := s.o.Store.Voices()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, vs)
}

func (s *Server) handleAddVoice(w http.ResponseWriter, r *http.Request) {
	limit := int64(duplexwire.MaxReferenceAudioBytes + 64<<10)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		writeErr(w, 400, "bad_request", "the upload is not a valid form or is too large")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "bad_request", "a WAV file is required in the 'file' field")
		return
	}
	defer f.Close()
	wav, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		writeErr(w, 400, "bad_request", "could not read the upload")
		return
	}
	v, err := s.o.Store.AddVoice(r.FormValue("name"), r.FormValue("language"), wav)
	if err != nil {
		writeErr(w, 422, "invalid_voice", err.Error())
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) handleVoiceAudio(w http.ResponseWriter, r *http.Request) {
	b, err := s.o.Store.VoiceWAV(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "voice.wav", time.Time{}, bytes.NewReader(b))
}

// handlePatchVoice sets the language spoken in a stored clip: {"language":"en"} (an empty string clears it).
func (s *Server) handlePatchVoice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Language string `json:"language"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, 400, "bad_request", "the body must be JSON like {\"language\":\"en\"}")
		return
	}
	v, err := s.o.Store.SetVoiceLanguage(r.PathValue("id"), req.Language)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			fail(w, err)
			return
		}
		writeErr(w, 422, "invalid_voice", err.Error())
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleDeleteVoice(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Store.DeleteVoice(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- card presets ----------------------------------------------------------------------------------------------------------------------

func (s *Server) handleCards(w http.ResponseWriter, r *http.Request) {
	cs, err := s.o.Store.Cards()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, cs)
}

func (s *Server) handleSaveCard(w http.ResponseWriter, r *http.Request) {
	var c CardPreset
	if err := decode(r, &c, 64<<10); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	c, err := s.o.Store.SaveCard(c)
	if err != nil {
		writeErr(w, 422, "invalid_card", err.Error())
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Store.DeleteCard(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- the session ticket ----------------------------------------------------------------------------------------------------------------

// handleSession mints a single-use ticket at forge-duplex with the long-lived token and hands the browser the socket address and the ticket. The
// token never reaches the page.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := decode(r, &req, 4<<10); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	m, err := s.o.Store.Model(req.Model)
	if err != nil {
		writeErr(w, 404, "unknown_model", "no such model is registered")
		return
	}
	if s.o.DuplexURL == "" {
		writeErr(w, 503, "duplex_not_configured", "no duplex service is configured (TALK_STUDIO_DUPLEX_URL)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, s.o.DuplexURL+"/v1/audio/duplex/ticket", nil)
	if err != nil {
		writeErr(w, 500, "internal", "bad duplex url")
		return
	}
	if s.o.DuplexToken != "" {
		hr.Header.Set("Authorization", "Bearer "+s.o.DuplexToken)
	}
	resp, err := s.o.Client.Do(hr)
	if err != nil {
		writeErr(w, 502, "duplex_unreachable", "the duplex service is not reachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		writeErr(w, 502, "duplex_refused", fmt.Sprintf("the duplex service refused a ticket (HTTP %d)", resp.StatusCode))
		return
	}
	var t struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&t); err != nil || t.Ticket == "" {
		writeErr(w, 502, "duplex_bad_answer", "the duplex service answered without a ticket")
		return
	}
	u, err := url.Parse(s.o.DuplexURL)
	if err != nil {
		writeErr(w, 500, "internal", "bad duplex url")
		return
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/v1/audio/duplex"
	writeJSON(w, 200, map[string]any{"ws_url": u.String(), "ticket": t.Ticket, "expires_in": t.ExpiresIn, "tenant": m.Tenant})
}

// --- recordings ------------------------------------------------------------------------------------------------------------------------

func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	rs, err := s.o.Store.Recordings()
	if err != nil {
		fail(w, err)
		return
	}
	type row struct {
		Recording
		Seconds float64 `json:"seconds"`
	}
	out := make([]row, len(rs))
	for i, x := range rs {
		out[i] = row{x, x.Seconds()}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleNewRecording(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string          `json:"model"`
		Voice string          `json:"voice"`
		Card  json.RawMessage `json:"card"`
	}
	if err := decode(r, &req, 64<<10); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	if _, err := s.o.Store.Model(req.Model); err != nil {
		writeErr(w, 404, "unknown_model", "no such model is registered")
		return
	}
	rec, err := s.o.Store.NewRecording(req.Model, req.Voice, req.Card)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, rec)
}

func (s *Server) handleRecording(w http.ResponseWriter, r *http.Request) {
	rec, err := s.o.Store.Recording(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	ev, err := s.o.Store.Events(rec.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var events []json.RawMessage
	for _, line := range bytes.Split(ev, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 && json.Valid(line) {
			events = append(events, json.RawMessage(line))
		}
	}
	writeJSON(w, 200, map[string]any{"recording": rec, "seconds": rec.Seconds(), "events": events})
}

func (s *Server) handleAppendAudio(w http.ResponseWriter, r *http.Request) {
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || off < 0 {
		writeErr(w, 400, "bad_request", "offset must be a non-negative integer")
		return
	}
	chunk, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunkBytes+1))
	if err != nil {
		writeErr(w, 413, "too_large", "a chunk is at most 2 MiB")
		return
	}
	size, err := s.o.Store.AppendAudio(r.PathValue("id"), r.PathValue("stream"), off, chunk)
	if errors.Is(err, ErrConflict) {
		// Say where the server really is, so the client can resume from there.
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the upload offset does not match what is stored", "code": "offset_conflict", "size": size})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"size": size})
}

func (s *Server) handleAppendEvents(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChunkBytes+1))
	if err != nil {
		writeErr(w, 413, "too_large", "too many events in one request")
		return
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 && !json.Valid(line) {
			writeErr(w, 400, "bad_request", "every event line must be JSON")
			return
		}
	}
	if err := s.o.Store.AppendEvents(r.PathValue("id"), b); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stats json.RawMessage `json:"stats"`
	}
	if err := decode(r, &req, 128<<10); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	rec, err := s.o.Store.FinishRecording(r.PathValue("id"), req.Stats)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, rec)
}

func (s *Server) handleRecordingAudio(w http.ResponseWriter, r *http.Request) {
	rd, _, err := s.o.Store.OpenAudio(r.PathValue("id"), r.PathValue("stream"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, r.PathValue("stream")+".wav", time.Time{}, rd)
}

func (s *Server) handleDeleteRecording(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Store.DeleteRecording(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

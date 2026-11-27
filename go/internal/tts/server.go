package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	Engine   Engine
	Token    string
	AudioDir string

	engineLister EngineLister
	readiness    ReadinessReporter
}

func NewServer(engine Engine, token, audioDir string) *Server {
	return &Server{
		Engine:   engine,
		Token:    token,
		AudioDir: audioDir,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/audio/speech", s.handleSpeech)
	mux.HandleFunc("/v1/voices", s.handleVoices)
	mux.HandleFunc("/v1/engines", s.handleEngines)
	mux.HandleFunc("/v1/readiness", s.handleReadiness)
	mux.HandleFunc("/v1/style/vocabulary", s.handleStyleVocabulary)
	mux.HandleFunc("/v1/style/parse", s.handleStyleParse)
	mux.HandleFunc("/v1/voices/import", s.handleImport)
	mux.HandleFunc("/v1/voices/preview", s.handlePreview)
	mux.HandleFunc("/v1/voices/", s.handleVoiceItem)
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		voiceList := r.URL.Path == "/v1/voices" && r.Method == http.MethodGet
		if strings.HasPrefix(r.URL.Path, "/v1/voices") && !voiceList {
			if r.Header.Get("X-Forge-Internal-Token") != s.Token {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	return r.Header.Get("X-Forge-Internal-Token") == s.Token
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health, err := s.Engine.Health(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "detail": err.Error()})
		return
	}
	health["model_ready"] = true
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.Engine.Models(), "object": "list"})
}

// copyStream writes audio to the client as the engine makes it, flushing every read so the first frame leaves at once instead of with the last. Errors
// after the headers can only end the body early: the status has been sent.
func (s *Server) copyStream(w http.ResponseWriter, r *http.Request, stream *SpeechStream) {
	defer stream.Body.Close()
	w.Header().Set("Content-Type", "audio/pcm")
	w.Header().Set("Cache-Control", "no-store")
	setNotes(w.Header(), stream.Notes)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush() // the client learns the stream has started before the first frame is made
	buf := make([]byte, 16<<10)
	for {
		n, err := stream.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // the client went away: closing the body stops the engine's work
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	var req SpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(req.ResponseFormat, "pcm") {
		if se, ok := s.Engine.(StreamEngine); ok {
			stream, err := se.SpeechStream(r.Context(), req)
			if err == nil {
				s.copyStream(w, r, stream)
				return
			}
			if !errors.Is(err, ErrNoStream) {
				if writeEngineError(w, err) {
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	res, err := s.Engine.Speech(r.Context(), req)
	if err != nil {
		if writeEngineError(w, err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setNotes(w.Header(), res.Notes)
	ct := "audio/wav"
	audio := res.Audio
	if res.Format == "pcm" {
		ct = "audio/pcm"
	} else if strings.EqualFold(req.ResponseFormat, "mp3") {
		mp3, err := encodeMP3(r.Context(), res.Audio)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ct, audio = "audio/mpeg", mp3
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(audio)
}

type voiceView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Language     string `json:"language"`
	Tier         string `json:"tier,omitempty"`
	Builtin      bool   `json:"builtin"`
	HasSample    bool   `json:"has_sample"`
	SampleText   string `json:"sample_text,omitempty"`
	Instruct     string `json:"instruct,omitempty"`
	RefText      string `json:"ref_text,omitempty"`
	XVectorOnly  bool   `json:"x_vector_only,omitempty"`
	// Engine is the engine the voice is bound to ("" = the default for its kind). Optional: callers that do not know it ignore it.
	Engine string `json:"engine,omitempty"`
}

func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listVoices(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.createVoice(w, r)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (s *Server) listVoices(w http.ResponseWriter, r *http.Request) {
	saved, err := s.Engine.ListVoices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	voices := make([]voiceView, 0, len(saved))
	for _, v := range saved {
		tier := v.Tier
		if tier == "" {
			tier = "premium"
		}
		vv := voiceView{
			ID:         v.ID,
			Name:       v.Name,
			Type:       v.Type,
			Language:   v.Language,
			Tier:       tier,
			Builtin:    tier == "fast",
			SampleText: v.SampleText,
			HasSample:  s.sampleExists(v.ID),
			Engine:     v.Engine,
		}
		if s.authorized(r) {
			if v.Design != nil {
				vv.Instruct = v.Design.Instruct
			}
			if v.Clone != nil {
				vv.RefText = v.Clone.RefText
				vv.XVectorOnly = v.Clone.XVectorOnly
			}
		}
		voices = append(voices, vv)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"voices":      voices,
		"models_warm": map[string]bool{"fast": true, "premium": true},
	})
}

func (s *Server) sampleExists(id string) bool {
	if s.AudioDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(s.AudioDir, id+"_sample.wav"))
	return err == nil
}

const defaultPreviewText = "This is a short preview of the voice."

func (s *Server) writeAudio(w http.ResponseWriter, res *SpeechResult) {
	ct := "audio/wav"
	if res.Format == "pcm" {
		ct = "audio/pcm"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(res.Audio)
}

func (s *Server) createVoice(w http.ResponseWriter, r *http.Request) {
	isMultipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/")
	var entry VoiceEntry
	if isMultipart {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		id := strings.ToLower(first(formValue(r, "id")))
		name := first(formValue(r, "name"))
		vtype := first(formValue(r, "type"))
		lang := first(formValue(r, "language"))
		if id == "" || name == "" || vtype == "" {
			http.Error(w, "id, name and type required", http.StatusBadRequest)
			return
		}
		// storeFile below writes id+".wav"/id+"_sample.wav" straight to
		// disk, ahead of (and independent of) Registry.Put's own id check
		// — reject a path-traversal-shaped id here, before any file touches
		// disk, rather than relying on the registry layer alone.
		if err := checkVoiceID(id); err != nil {
			http.Error(w, "id must be lowercase alphanumeric with - or _, max 64 chars", http.StatusBadRequest)
			return
		}
		entry = VoiceEntry{ID: id, Name: name, Type: vtype, Language: lang}
		switch vtype {
		case "design":
			entry.Design = &DesignSpec{Instruct: first(formValue(r, "instruct"))}
		case "clone":
			if _, _, err := r.FormFile("ref_audio"); err != nil {
				http.Error(w, "ref_audio required for clone", http.StatusBadRequest)
				return
			}
			refAudioName := id + ".wav"
			if err := s.storeFile(r, "ref_audio", refAudioName); err != nil {
				http.Error(w, "ref_audio: "+err.Error(), http.StatusBadRequest)
				return
			}
			entry.Clone = &CloneSpec{
				RefAudio:    refAudioName,
				RefText:     first(formValue(r, "ref_text")),
				XVectorOnly: first(formValue(r, "x_vector_only")) == "true",
			}
			if sf, _, err := r.FormFile("sample"); err == nil {
				sf.Close()
				_ = s.storeFile(r, "sample", id+"_sample.wav")
			}
		default:
			http.Error(w, "unknown voice type", http.StatusBadRequest)
			return
		}
	} else {
		var body struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Type     string `json:"type"`
			Language string `json:"language"`
			Instruct string `json:"instruct"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.ID == "" || body.Name == "" || body.Type == "" {
			http.Error(w, "id, name and type required", http.StatusBadRequest)
			return
		}
		if err := checkVoiceID(strings.ToLower(body.ID)); err != nil {
			http.Error(w, "id must be lowercase alphanumeric with - or _, max 64 chars", http.StatusBadRequest)
			return
		}
		if body.Type != "design" {
			http.Error(w, "json create supports type=design only", http.StatusBadRequest)
			return
		}
		entry = VoiceEntry{ID: strings.ToLower(body.ID), Name: body.Name, Type: "design", Language: body.Language, Design: &DesignSpec{Instruct: body.Instruct}}
	}

	if _, ok, _ := s.Engine.GetVoice(r.Context(), entry.ID); ok {
		http.Error(w, "voice id already exists", http.StatusConflict)
		return
	}
	if err := s.Engine.SaveVoice(r.Context(), entry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go func() {
		_ = s.Engine.GenerateSample(context.Background(), entry.ID)
	}()
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := first(formValue(r, "id"))
	name := first(formValue(r, "name"))
	vtype := first(formValue(r, "type"))
	lang := first(formValue(r, "language"))
	if id == "" || name == "" || vtype == "" {
		http.Error(w, "id, name and type required", http.StatusBadRequest)
		return
	}
	// id becomes a file name (id+".wav", id+"_sample.wav"): refuse a path-shaped one before anything is written.
	if err := checkVoiceID(id); err != nil {
		http.Error(w, "id: "+err.Error(), http.StatusBadRequest)
		return
	}

	entry := VoiceEntry{ID: id, Name: name, Type: vtype, Language: lang}
	// An optional engine the voice is bound to. An older voice-studio does not send it; an older forge-tts ignores it. It is stored as given and is
	// resolved by the daemon, so a name this node does not know is not an import error: the voice then reports an unavailable engine when it is played.
	if eng := strings.TrimSpace(first(formValue(r, "engine"))); eng != "" {
		if !validEngineName(eng) {
			http.Error(w, "engine: 1 to 64 characters, no spaces or control characters", http.StatusBadRequest)
			return
		}
		entry.Engine = eng
	}
	switch vtype {
	case "design":
		entry.Design = &DesignSpec{Instruct: first(formValue(r, "instruct"))}
	case "clone":
		refAudioName := id + ".wav"
		if err := s.storeFile(r, "ref_audio", refAudioName); err != nil {
			http.Error(w, "ref_audio: "+err.Error(), http.StatusBadRequest)
			return
		}
		entry.Clone = &CloneSpec{
			RefAudio:    refAudioName,
			RefText:     first(formValue(r, "ref_text")),
			XVectorOnly: first(formValue(r, "x_vector_only")) == "true",
		}
		if sf, _, err := r.FormFile("sample"); err == nil {
			sf.Close()
			_ = s.storeFile(r, "sample", id+"_sample.wav")
		}
	default:
		http.Error(w, "unknown voice type", http.StatusBadRequest)
		return
	}

	if err := s.Engine.SaveVoice(r.Context(), entry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		text := first(formValue(r, "text"))
		if text == "" {
			text = defaultPreviewText
		}
		lang := first(formValue(r, "language"))
		if lang == "" {
			lang = "English"
		}
		refText := first(formValue(r, "ref_text"))
		respFmt := first(formValue(r, "response_format"))
		if respFmt == "" {
			respFmt = "wav"
		}
		fh, _, err := r.FormFile("audio")
		if err != nil {
			http.Error(w, "audio file required for clone preview", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := s.Engine.Preview(r.Context(), PreviewRequest{
			Mode: "clone", RefAudio: data, RefText: refText, Text: text, Language: lang, Format: respFmt,
		})
		if err != nil {
			if writeEngineError(w, err) {
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.writeAudio(w, res)
		return
	}

	var p PreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Mode == "" {
		p.Mode = "design"
	}
	if p.Text == "" {
		p.Text = defaultPreviewText
	}
	if p.Language == "" {
		p.Language = "English"
	}
	if p.Format == "" {
		p.Format = "wav"
	}
	res, err := s.Engine.Preview(r.Context(), p)
	if err != nil {
		if writeEngineError(w, err) {
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeAudio(w, res)
}

func (s *Server) handleVoiceItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/voices/")
	rest = strings.Trim(rest, "/")
	if strings.HasSuffix(r.URL.Path, "/sample") {
		id := strings.TrimSuffix(rest, "/sample")
		s.handleSample(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := s.Engine.DeleteVoice(r.Context(), rest); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Deleting what is not there is a 404, which voice-studio's delete client tolerates (it deletes on every node before a rename or
				// redeploy, including nodes the voice was never on). It used to answer 500.
				http.Error(w, "voice not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": rest})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSample(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		data, err := s.Engine.GetSample(r.Context(), id)
		if err != nil {
			http.Error(w, "no sample", http.StatusNotFound)
			return
		}
		s.serveAudioBytes(w, r, data)
	case http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Text == "" {
			http.Error(w, "text required", http.StatusBadRequest)
			return
		}
		entry, ok, err := s.Engine.GetVoice(r.Context(), id)
		if err != nil || !ok {
			http.Error(w, "voice not found", http.StatusNotFound)
			return
		}
		entry.SampleText = body.Text
		if err := s.Engine.SaveVoice(r.Context(), entry); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.Engine.GenerateSample(r.Context(), id); err != nil {
			if writeEngineError(w, err) {
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "sample_text": body.Text})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveAudioBytes(w http.ResponseWriter, r *http.Request, data []byte) {
	size := int64(len(data))
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}
	var start, end int64
	if n, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); n < 1 || err != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	if start < 0 || start >= size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end < start || end >= size {
		end = size - 1
	}
	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	w.Header().Set("Content-Length", fmt.Sprint(length))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(data[start : end+1])
}

func (s *Server) storeFile(r *http.Request, field, name string) error {
	fh, _, err := r.FormFile(field)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := os.MkdirAll(s.AudioDir, 0o755); err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(s.AudioDir, name))
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, fh); err != nil {
		return err
	}
	return nil
}

func formValue(r *http.Request, key string) []string {
	if r.Form == nil {
		return nil
	}
	return r.Form[key]
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(v)
	w.Write(b)
}

// validEngineName accepts what the daemon accepts for an occupant name (1-64 characters), minus whitespace and control characters, which could only
// be a mistake in a name that ends up in URLs and logs.
func validEngineName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// writeEngineError answers an engine that could not be resolved and reports whether it did. A transient failure (the engine is loading, the pool is
// full, the engine service did not answer) is a 503 whose body contains `model_not_ready`, the exact text voice-studio retries on; a hard one (the engine is
// turned off, or nothing can play this voice) is a 503 with its own code and without that text, so nobody retries a thing that will not change.
func writeEngineError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, ErrNoVoice) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "speech_no_voice", "type": "invalid_request", "message": err.Error()}})
		return true
	}
	ee, ok := IsEngineError(err)
	if !ok {
		return false
	}
	code, typ := ee.Code, "unavailable"
	if ee.Transient {
		code, typ = "model_not_ready", "model_not_ready"
		if ee.RetryAfter <= 0 {
			ee.RetryAfter = 2 * time.Second
		}
		w.Header().Set("Retry-After", strconv.Itoa(int((ee.RetryAfter+time.Second-1)/time.Second)))
	}
	msg := ee.Message
	if ee.Transient && ee.Code != "" && ee.Code != "model_not_ready" {
		msg = ee.Message + " (" + ee.Code + ")"
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": code, "type": typ, "message": msg, "engine": ee.Engine}})
	return true
}

// EngineLister lists the speech engines a node can use. Only the daemon resolver can; with fixed addresses there is nothing to choose from.
type EngineLister interface {
	Engines(ctx context.Context) ([]EngineInfo, error)
}

// SetEngineLister turns on GET /v1/engines.
func (s *Server) SetEngineLister(l EngineLister) { s.engineLister = l }

// handleEngines answers GET /v1/engines: the enabled-or-not speech engines with their kind and state, so voice-studio can offer an Engine choice. It is
// open like the voice list (names and states only). Without a daemon resolver it answers 404, which voice-studio reads as "no choice to offer".
func (s *Server) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engineLister == nil {
		http.Error(w, "engines are fixed on this node", http.StatusNotFound)
		return
	}
	list, err := s.engineLister.Engines(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"code": "engine_list_unavailable", "message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engines": list})
}

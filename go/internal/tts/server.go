package tts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Server struct {
	Engine   Engine
	Token    string
	AudioDir string
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

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	var req SpeechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res, err := s.Engine.Speech(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ct := "audio/wav"
	if res.Format == "pcm" {
		ct = "audio/pcm"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	w.Write(res.Audio)
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

	entry := VoiceEntry{ID: id, Name: name, Type: vtype, Language: lang}
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

package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplexwire"
)

// The data directory holds everything talk-studio keeps: the model registry, voice references, card presets and recorded sessions. Nothing here is
// a secret, but reference voices and recordings can be private (a saved clone, a person's speech): they stay on this volume and are only ever sent
// to the speech-to-speech engine the operator chose.

const (
	sampleRate     = 24000
	maxChunkBytes  = 2 << 20   // one upload chunk
	maxStreamBytes = 400 << 20 // one recorded stream (about 2.3 hours of 24 kHz PCM16)
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("talk-studio: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ErrNotFound and ErrConflict map to 404 and 409 at the HTTP layer.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Model is one speech-to-speech model the page can talk to. The page builds its controls from these fields, so a future model is a new entry here,
// not new client code (ADR-0029 decision 5). Tenant is the name forge-duplex offers it under.
type Model struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Tenant    string   `json:"tenant"`
	Text      bool     `json:"text"`                // the model emits text events
	Reference bool     `json:"reference"`           // the model takes a reference voice
	Card      string   `json:"card"`                // "haru" (structured fields), "text" (one free-text prompt) or "none"
	Languages []string `json:"languages,omitempty"` // for the card's language choice
	Notes     string   `json:"notes,omitempty"`
}

// DefaultModels is what a new data directory starts with.
func DefaultModels() []Model {
	return []Model{{ID: "haru", Label: "Haru 4B", Tenant: "haru", Text: true, Reference: true, Card: "haru", Languages: []string{"ja", "en"},
		Notes: "Full-duplex speech-to-speech. Speaks in the voice of the reference clip."}}
}

func validateModels(ms []Model) error {
	seen := map[string]bool{}
	for _, m := range ms {
		if m.ID == "" || len(m.ID) > 64 || !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`).MatchString(m.ID) {
			return fmt.Errorf("model id %q must be lowercase letters, digits, '.', '_' or '-' (at most 64)", m.ID)
		}
		if seen[m.ID] {
			return fmt.Errorf("model id %q is listed twice", m.ID)
		}
		seen[m.ID] = true
		if m.Tenant == "" || len(m.Tenant) > duplexwire.MaxTenantNameBytes {
			return fmt.Errorf("model %q needs a tenant name (at most %d bytes)", m.ID, duplexwire.MaxTenantNameBytes)
		}
		if m.Label == "" || len(m.Label) > 128 {
			return fmt.Errorf("model %q needs a label (at most 128 characters)", m.ID)
		}
		switch m.Card {
		case "haru", "text", "none":
		default:
			return fmt.Errorf("model %q: card must be haru, text or none", m.ID)
		}
	}
	if len(ms) == 0 {
		return errors.New("at least one model is required")
	}
	return nil
}

// Store is the file-backed data directory. One mutex serialises writes; the data is small and written rarely except recording chunks, which have
// their own per-recording lock.
type Store struct {
	dir string
	mu  sync.Mutex
	rec map[string]*sync.Mutex // per recording, for chunk appends
}

func NewStore(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "voices"), filepath.Join(dir, "recordings")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, rec: map[string]*sync.Mutex{}}
	if _, err := os.Stat(s.path("models.json")); errors.Is(err, os.ErrNotExist) {
		if err := s.writeJSON("models.json", DefaultModels()); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

// writeJSON replaces a file atomically so a crash never leaves half a registry.
func (s *Store) writeJSON(name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name + ".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s *Store) readJSON(name string, v any) error {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// --- models ----------------------------------------------------------------------------------------------------------------------------

func (s *Store) Models() ([]Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ms []Model
	if err := s.readJSON("models.json", &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

func (s *Store) SetModels(ms []Model) error {
	if err := validateModels(ms); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSON("models.json", ms)
}

func (s *Store) Model(id string) (Model, error) {
	ms, err := s.Models()
	if err != nil {
		return Model{}, err
	}
	for _, m := range ms {
		if m.ID == id {
			return m, nil
		}
	}
	return Model{}, ErrNotFound
}

// --- card presets -----------------------------------------------------------------------------------------------------------------------

// CardPreset is a saved scenario (stored as a card preset): a name, the model it is for, the card itself (a field object for haru cards, a JSON string for text cards).
type CardPreset struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Model string          `json:"model"`
	Card  json.RawMessage `json:"card"`
	// Voice is the page's voice choice for this scenario (a saved recording's id, or "vs:<id>" for a voice-studio voice; empty for none) and SampleText the
	// edited text of the made sample (empty when it follows the card). A scenario is the character, the scene and the voice together.
	Voice      string    `json:"voice,omitempty"`
	SampleText string    `json:"sample_text,omitempty"`
	Created    time.Time `json:"created"`
}

func (s *Store) Cards() ([]CardPreset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cs []CardPreset
	if err := s.readJSON("cards.json", &cs); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if cs == nil {
		cs = []CardPreset{}
	}
	return cs, nil
}

func (s *Store) SaveCard(c CardPreset) (CardPreset, error) {
	if c.Name == "" || len(c.Name) > 128 {
		return CardPreset{}, errors.New("a card preset needs a name (at most 128 characters)")
	}
	if len(c.Card) == 0 || len(c.Card) > duplexwire.MaxCardBytes || !json.Valid(c.Card) {
		return CardPreset{}, fmt.Errorf("the card must be valid JSON of at most %d bytes", duplexwire.MaxCardBytes)
	}
	if _, err := s.Model(c.Model); err != nil {
		return CardPreset{}, errors.New("the card names a model that is not registered")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var cs []CardPreset
	if err := s.readJSON("cards.json", &cs); err != nil && !errors.Is(err, ErrNotFound) {
		return CardPreset{}, err
	}
	c.Created = time.Now().UTC()
	replaced := false
	if c.ID != "" {
		for i := range cs {
			if cs[i].ID == c.ID {
				c.Created = cs[i].Created
				cs[i], replaced = c, true
			}
		}
	}
	if !replaced {
		c.ID = newID()
		cs = append(cs, c)
	}
	return c, s.writeJSON("cards.json", cs)
}

func (s *Store) DeleteCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cs []CardPreset
	if err := s.readJSON("cards.json", &cs); err != nil {
		return err
	}
	out := cs[:0]
	found := false
	for _, c := range cs {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		return ErrNotFound
	}
	return s.writeJSON("cards.json", out)
}

// --- voices ----------------------------------------------------------------------------------------------------------------------------

// Voice is a reference voice: a canonical 24 kHz mono PCM16 WAV the engine clones the speaking voice from.
type Voice struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	Bytes   int     `json:"bytes"` // the WAV file
	// Language is the language spoken in the clip (an ISO 639-1 code, empty when not set). The model hears the clip as its own earlier speech, and a card in
	// another language than the clip leaves it silent, so the page keeps the card's language and the clip's together.
	Language string    `json:"language,omitempty"`
	Created  time.Time `json:"created"`
}

var languagePattern = regexp.MustCompile(`^[a-z]{2}$`)

// validLanguage accepts an empty language (not set) or a two-letter lower-case code.
func validLanguage(l string) bool { return l == "" || languagePattern.MatchString(l) }

const wavHeaderBytes = 44

// ParseWAV checks that b is a canonical 44-byte-header mono 16-bit 24 kHz PCM WAV whose data chunk length matches the file, and returns the PCM.
func ParseWAV(b []byte) ([]byte, error) {
	if len(b) < wavHeaderBytes || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " || string(b[36:40]) != "data" {
		return nil, errors.New("not a canonical PCM WAV (44-byte header)")
	}
	le := binary.LittleEndian
	if le.Uint16(b[20:22]) != 1 || le.Uint16(b[22:24]) != 1 || le.Uint32(b[24:28]) != sampleRate || le.Uint16(b[34:36]) != 16 {
		return nil, fmt.Errorf("the WAV must be mono 16-bit PCM at %d Hz", sampleRate)
	}
	n := int(le.Uint32(b[40:44]))
	if n != len(b)-wavHeaderBytes {
		return nil, errors.New("the WAV data length does not match the file")
	}
	if n == 0 || n%2 != 0 {
		return nil, errors.New("the WAV has no audio")
	}
	return b[wavHeaderBytes:], nil
}

// WAVHeader is the 44-byte header for n bytes of mono 16-bit 24 kHz PCM.
func WAVHeader(n int) []byte {
	h := make([]byte, wavHeaderBytes)
	le := binary.LittleEndian
	copy(h[0:], "RIFF")
	le.PutUint32(h[4:], uint32(36+n))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], 1)
	le.PutUint32(h[24:], sampleRate)
	le.PutUint32(h[28:], sampleRate*2)
	le.PutUint16(h[32:], 2)
	le.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	le.PutUint32(h[40:], uint32(n))
	return h
}

// minReferenceRMS is the lowest RMS (full scale 1.0) a reference voice may have. A silent clip is accepted by the engine and silently produces its
// default voice, which looks like a gender bug.
const minReferenceRMS = 0.003

func pcmRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

func (s *Store) AddVoice(name, language string, wav []byte) (Voice, error) {
	name = strings.TrimSpace(name)
	language = strings.TrimSpace(language)
	if !validLanguage(language) {
		return Voice{}, errors.New("the language must be a two-letter code such as en or ja")
	}
	if name == "" || len(name) > 128 {
		return Voice{}, errors.New("a voice needs a name (at most 128 characters)")
	}
	pcm, err := ParseWAV(wav)
	if err != nil {
		return Voice{}, err
	}
	if rms := pcmRMS(pcm); rms < minReferenceRMS {
		return Voice{}, fmt.Errorf("that recording is silent or nearly so (level %.4f): the model would clone nothing and fall back to its default voice", rms)
	}
	if len(wav) > duplexwire.MaxReferenceAudioBytes { // the whole file is what is sent to the engine
		return Voice{}, fmt.Errorf("a reference voice may be at most %d bytes (about %d seconds)", duplexwire.MaxReferenceAudioBytes, duplexwire.MaxReferenceAudioBytes/(sampleRate*2))
	}
	v := Voice{ID: newID(), Name: name, Seconds: float64(len(pcm)) / (sampleRate * 2), Bytes: len(wav), Language: language, Created: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.WriteFile(s.path("voices", v.ID+".wav"), wav, 0o600); err != nil {
		return Voice{}, err
	}
	if err := s.writeJSON(filepath.Join("voices", v.ID+".json"), v); err != nil {
		_ = os.Remove(s.path("voices", v.ID+".wav"))
		return Voice{}, err
	}
	return v, nil
}

func (s *Store) Voices() ([]Voice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := filepath.Glob(s.path("voices", "*.json"))
	if err != nil {
		return nil, err
	}
	out := []Voice{}
	for _, f := range files {
		var v Voice
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &v) != nil || !idPattern.MatchString(v.ID) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// VoiceWAV returns the stored WAV bytes for a voice id.
func (s *Store) VoiceWAV(id string) ([]byte, error) {
	if !idPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(s.path("voices", id+".wav"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *Store) Voice(id string) (Voice, error) {
	if !idPattern.MatchString(id) {
		return Voice{}, ErrNotFound
	}
	var v Voice
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readJSON(filepath.Join("voices", id+".json"), &v); err != nil {
		return Voice{}, err
	}
	return v, nil
}

// SetVoiceLanguage records the language spoken in a stored clip (an empty string clears it).
func (s *Store) SetVoiceLanguage(id, language string) (Voice, error) {
	language = strings.TrimSpace(language)
	if !validLanguage(language) {
		return Voice{}, errors.New("the language must be a two-letter code such as en or ja")
	}
	if !idPattern.MatchString(id) {
		return Voice{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v Voice
	if err := s.readJSON(filepath.Join("voices", id+".json"), &v); err != nil {
		return Voice{}, err
	}
	v.Language = language
	if err := s.writeJSON(filepath.Join("voices", id+".json"), v); err != nil {
		return Voice{}, err
	}
	return v, nil
}

func (s *Store) DeleteVoice(id string) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path("voices", id+".json")); err != nil {
		return ErrNotFound
	}
	_ = os.Remove(s.path("voices", id+".wav"))
	return os.Remove(s.path("voices", id+".json"))
}

// --- recordings ------------------------------------------------------------------------------------------------------------------------

// Recording describes one saved session. The audio is two raw PCM16 streams (what the user sent, what the model said) and the events are JSON
// lines (text the model produced with times, state changes), so a session can be re-checked later with the repo's tools.
type Recording struct {
	ID       string          `json:"id"`
	Model    string          `json:"model"`
	Voice    string          `json:"voice,omitempty"`
	Card     json.RawMessage `json:"card,omitempty"`
	Started  time.Time       `json:"started"`
	Finished *time.Time      `json:"finished,omitempty"`
	UserB    int64           `json:"user_bytes"`
	ModelB   int64           `json:"model_bytes"`
	Stats    json.RawMessage `json:"stats,omitempty"`
}

// Seconds of audio in the longer stream.
func (r Recording) Seconds() float64 {
	n := r.UserB
	if r.ModelB > n {
		n = r.ModelB
	}
	return float64(n) / (sampleRate * 2)
}

func (s *Store) recLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.rec[id]
	if m == nil {
		m = &sync.Mutex{}
		s.rec[id] = m
	}
	return m
}

func (s *Store) NewRecording(model, voice string, card json.RawMessage) (Recording, error) {
	if len(card) > duplexwire.MaxCardBytes {
		return Recording{}, errors.New("the card is too large")
	}
	r := Recording{ID: newID(), Model: model, Voice: voice, Card: card, Started: time.Now().UTC()}
	if err := os.MkdirAll(s.path("recordings", r.ID), 0o700); err != nil {
		return Recording{}, err
	}
	return r, s.saveRec(r)
}

func (s *Store) saveRec(r Recording) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeJSON(filepath.Join("recordings", r.ID, "meta.json"), r)
}

func (s *Store) Recording(id string) (Recording, error) {
	if !idPattern.MatchString(id) {
		return Recording{}, ErrNotFound
	}
	var r Recording
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readJSON(filepath.Join("recordings", id, "meta.json"), &r); err != nil {
		return Recording{}, err
	}
	return r, nil
}

func (s *Store) Recordings() ([]Recording, error) {
	s.mu.Lock()
	files, err := filepath.Glob(s.path("recordings", "*", "meta.json"))
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := []Recording{}
	for _, f := range files {
		var r Recording
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &r) != nil || !idPattern.MatchString(r.ID) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// AppendAudio appends a chunk to a stream ("user" or "model") if offset equals the stream's current size. A mismatch is ErrConflict with the
// current size, so a client that lost an acknowledgement resumes from where the server really is instead of duplicating or skipping audio.
func (s *Store) AppendAudio(id, stream string, offset int64, chunk []byte) (int64, error) {
	if stream != "user" && stream != "model" {
		return 0, ErrNotFound
	}
	if len(chunk)%2 != 0 || len(chunk) > maxChunkBytes {
		return 0, errors.New("a chunk is whole 16-bit samples and at most 2 MiB")
	}
	r, err := s.Recording(id)
	if err != nil {
		return 0, err
	}
	lk := s.recLock(id)
	lk.Lock()
	defer lk.Unlock()
	p := s.path("recordings", id, stream+".pcm")
	st, err := os.Stat(p)
	size := int64(0)
	if err == nil {
		size = st.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if offset != size {
		return size, ErrConflict
	}
	if size+int64(len(chunk)) > maxStreamBytes {
		return size, errors.New("the recording is too long")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(chunk); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	size += int64(len(chunk))
	if stream == "user" {
		r.UserB = size
	} else {
		r.ModelB = size
	}
	return size, s.saveRec(r)
}

// AppendEvents appends JSON lines to the recording's event log.
func (s *Store) AppendEvents(id string, lines []byte) error {
	if _, err := s.Recording(id); err != nil {
		return err
	}
	if len(lines) > maxChunkBytes {
		return errors.New("too many events in one request")
	}
	lk := s.recLock(id)
	lk.Lock()
	defer lk.Unlock()
	f, err := os.OpenFile(s.path("recordings", id, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(lines) > 0 && lines[len(lines)-1] != '\n' {
		lines = append(lines, '\n')
	}
	_, err = f.Write(lines)
	return err
}

func (s *Store) FinishRecording(id string, stats json.RawMessage) (Recording, error) {
	r, err := s.Recording(id)
	if err != nil {
		return Recording{}, err
	}
	now := time.Now().UTC()
	r.Finished = &now
	if len(stats) > 0 && json.Valid(stats) && len(stats) <= 64<<10 {
		r.Stats = stats
	}
	return r, s.saveRec(r)
}

// OpenAudio opens a recorded stream as a WAV (header plus the raw PCM) for http.ServeContent.
func (s *Store) OpenAudio(id, stream string) (io.ReadSeeker, int64, error) {
	if stream != "user" && stream != "model" {
		return nil, 0, ErrNotFound
	}
	if _, err := s.Recording(id); err != nil {
		return nil, 0, err
	}
	b, err := os.ReadFile(s.path("recordings", id, stream+".pcm"))
	if errors.Is(err, os.ErrNotExist) {
		b = nil
	} else if err != nil {
		return nil, 0, err
	}
	wav := append(WAVHeader(len(b)), b...)
	return &bytesReader{b: wav}, int64(len(wav)), nil
}

func (s *Store) Events(id string) ([]byte, error) {
	if _, err := s.Recording(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path("recordings", id, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (s *Store) DeleteRecording(id string) error {
	if !idPattern.MatchString(id) {
		return ErrNotFound
	}
	if _, err := s.Recording(id); err != nil {
		return err
	}
	lk := s.recLock(id)
	lk.Lock()
	defer lk.Unlock()
	return os.RemoveAll(s.path("recordings", id))
}

type bytesReader struct {
	b []byte
	i int64
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.i >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += int64(n)
	return n, nil
}

func (r *bytesReader) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = r.i + off
	case io.SeekEnd:
		abs = int64(len(r.b)) + off
	default:
		return 0, errors.New("bad whence")
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	r.i = abs
	return abs, nil
}

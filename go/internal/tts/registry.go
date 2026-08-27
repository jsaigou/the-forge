package tts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// ErrInvalidVoiceID is returned by any Registry method that turns a voice
// ID into a filename component when the ID isn't safe to do that with.
var ErrInvalidVoiceID = errors.New("tts: invalid voice id")

// validVoiceID matches server.go's createVoice, which already lowercases
// incoming IDs — kept intentionally narrow (no ".", "/", or "\") since
// every method below joins id straight into a filename (id+".wav",
// id+"_sample.wav"). A caller-supplied id was previously trusted verbatim
// here, which let a value like "../../../../etc/cron.d/x" read, write, or
// delete files outside AudioDir — the id+"_sample.wav"/id+".wav" suffix
// narrows but doesn't close that off, and GetSample/SetSample/Delete are
// all reachable with an id that was never legitimately created via Put.
var validVoiceID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func checkVoiceID(id string) error {
	if !validVoiceID.MatchString(id) {
		return ErrInvalidVoiceID
	}
	return nil
}

type Registry struct {
	dir   string
	mu    sync.Mutex
	index map[string]VoiceEntry
}

func NewRegistry(dir string) *Registry {
	return &Registry{dir: dir, index: map[string]VoiceEntry{}}
}

func (r *Registry) path() string     { return filepath.Join(r.dir, "voices.json") }
func (r *Registry) AudioDir() string { return filepath.Join(r.dir, "audio") }

func (r *Registry) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

func (r *Registry) loadLocked() error {
	b, err := os.ReadFile(r.path())
	if err != nil {
		if os.IsNotExist(err) {
			r.index = map[string]VoiceEntry{}
			return nil
		}
		return err
	}
	var entries []VoiceEntry
	if err := json.Unmarshal(b, &entries); err == nil {
		// bare array form
	} else {
		// {"voices":[...]} wrapper form (Python tts_server.py layout)
		var wrapped struct {
			Voices []VoiceEntry `json:"voices"`
		}
		if err2 := json.Unmarshal(b, &wrapped); err2 != nil {
			return fmt.Errorf("tts registry: parse %s: %w", r.path(), err)
		}
		entries = wrapped.Voices
	}
	idx := make(map[string]VoiceEntry, len(entries))
	for _, e := range entries {
		idx[e.ID] = e
	}
	r.index = idx
	return nil
}

func (r *Registry) flushLocked() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	entries := make([]VoiceEntry, 0, len(r.index))
	for _, e := range r.index {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	wrapped := struct {
		Voices []VoiceEntry `json:"voices"`
	}{Voices: entries}
	b, err := json.MarshalIndent(wrapped, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:gosec // tmp is a fixed registry path
		return err
	}
	return os.Rename(tmp, r.path())
}

func (r *Registry) List() ([]VoiceEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return nil, err
	}
	out := make([]VoiceEntry, 0, len(r.index))
	for _, e := range r.index {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Registry) Get(id string) (VoiceEntry, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return VoiceEntry{}, false, err
	}
	e, ok := r.index[id]
	return e, ok, nil
}

func (r *Registry) Put(v VoiceEntry) error {
	if err := checkVoiceID(v.ID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return err
	}
	r.index[v.ID] = v
	return r.flushLocked()
}

func (r *Registry) Delete(id string) error {
	if err := checkVoiceID(id); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.loadLocked(); err != nil {
		return err
	}
	v, ok := r.index[id]
	if !ok {
		return os.ErrNotExist
	}
	delete(r.index, id)
	if err := r.flushLocked(); err != nil {
		return err
	}
	// Fixed above: this used to re-check r.index[id] after the delete
	// already ran, so ok was always false and RefAudio was never cleaned
	// up — v is now captured before the delete instead.
	if v.Clone != nil && v.Clone.RefAudio != "" {
		_ = os.Remove(filepath.Join(r.AudioDir(), v.Clone.RefAudio))
	}
	_ = os.Remove(filepath.Join(r.AudioDir(), id+"_sample.wav"))
	return nil
}

func (r *Registry) GetSample(id string) ([]byte, error) {
	if err := checkVoiceID(id); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.AudioDir(), 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(r.AudioDir(), id+"_sample.wav"))
	if err != nil {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (r *Registry) SetSample(id string, wav []byte) error {
	if err := checkVoiceID(id); err != nil {
		return err
	}
	if err := os.MkdirAll(r.AudioDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.AudioDir(), id+"_sample.wav"), wav, 0o600) //nolint:gosec // id validated by checkVoiceID
}

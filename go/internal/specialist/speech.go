// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Speech resolution (docs/speech-routing-plan.md, P1). Speech is the one function the daemon could not see: forge-tts used to be handed an engine's
// address once and then talk to it directly. Now the front door asks, per request, "which engine, loaded, for this kind of voice?", and gets the
// address plus a lease. The lease is the daemon's only way to know a request is in flight, so an engine that holds one is never evicted as idle.

// The kinds of speech an engine can render, named as forge-tts names its modes. A Qwen checkpoint renders exactly one of the first three (fixed by the
// model file; the engine's `alias` parameter says which); Kokoro renders its own voices.
const (
	SpeechBase        = "base"        // clone voices from a reference recording
	SpeechCustomVoice = "customvoice" // preset named speakers
	SpeechVoiceDesign = "voicedesign" // voices described in words
	SpeechKokoro      = "kokoro"      // Kokoro's built-in voices
)

// SpeechModes lists the kinds in display order.
var SpeechModes = []string{SpeechBase, SpeechCustomVoice, SpeechVoiceDesign, SpeechKokoro}

// ValidSpeechMode reports whether mode is one of SpeechModes.
func ValidSpeechMode(mode string) bool {
	for _, m := range SpeechModes {
		if m == mode {
			return true
		}
	}
	return false
}

// SpeechModeOf says which kind of speech an occupant renders. ok is false for an occupant that is not a speech engine (it does not serve `speech`, or
// its launch kind has no speech mode, or a Qwen engine's alias is not a mode).
func SpeechModeOf(o store.Occupant) (mode string, ok bool) {
	if !serves(o, store.FnSpeech) {
		return "", false
	}
	switch o.Kind {
	case "kokoro":
		return SpeechKokoro, true
	case KindZonos3Server:
		return SpeechBase, true // clone only: the model has no preset speakers and no voice description input
	case "qwen-tts-server":
		if a := o.Params["alias"]; a == SpeechBase || a == SpeechCustomVoice || a == SpeechVoiceDesign {
			return a, true
		}
	}
	return "", false
}

const (
	defaultLeaseTTL = 10 * time.Minute
	maxLeases       = 256 // a caller that never releases cannot grow the table without bound
)

// SpeechRequest asks for an engine for one speech request.
type SpeechRequest struct {
	// Mode is the kind of speech needed (SpeechModes). Required.
	Mode string
	// Engine names the engine a voice is bound to ("" = the voice has none and the default for the kind applies). A named engine is never swapped
	// for another: a different engine would render a different voice.
	Engine string
}

// SpeechLease is a loaded engine and the lease that protects it while a request uses it.
type SpeechLease struct {
	ID      string
	Engine  string
	Slot    string
	Mode    string
	Kind    string // the engine's launch kind (specialist.Kind*): forge-tts picks the wire protocol from it
	BaseURL string // http://127.0.0.1:<port>, no path
	Port    int
	Expires time.Time
	Loaded  bool // the engine had to be started for this request
}

// NoSpeechEngineError: nothing can render this kind of speech (no engine of the kind is registered, or the named one does not exist or is of another
// kind). It is a hard answer, not a transient one: waiting does not change it.
type NoSpeechEngineError struct {
	Mode   string
	Engine string
	Reason string
}

func (e *NoSpeechEngineError) Error() string {
	if e.Engine != "" {
		return fmt.Sprintf("speech engine %q cannot be used for %s: %s", e.Engine, e.Mode, e.Reason)
	}
	return fmt.Sprintf("no speech engine for %s: %s", e.Mode, e.Reason)
}

// ErrTooManyLeases means the lease table is full (a holder is not releasing).
var ErrTooManyLeases = errors.New("specialist: too many open speech leases")

type speechLease struct {
	slot     string
	occupant string
	loadedAt time.Time // identifies the engine INSTANCE: a lease on an engine that has since been stopped and started again protects nothing
	expires  time.Time
}

// leaseCountLocked counts the live leases on slot. A lease is live while it has not expired and its slot still holds the same engine instance it was taken
// on (an engine that was stopped and started again is a different instance: the old lease protects nothing and must not pin the new one). Stale leases
// of any slot are dropped as they are met. Caller holds m.mu.
func (m *Manager) leaseCountLocked(slot string) int {
	now := m.d.Now()
	n := 0
	for id, l := range m.leases {
		r := m.slots[l.slot]
		if !now.Before(l.expires) || r == nil || r.occupant != l.occupant || !r.loadedAt.Equal(l.loadedAt) {
			delete(m.leases, id)
			continue
		}
		if l.slot == slot {
			n++
		}
	}
	return n
}

// ResolveSpeech finds the engine for req, loads it if needed (on demand), and takes a lease on it. The order (plan section 4.2): the named engine if
// there is one (no substitution), otherwise the default engine for the kind, then any other enabled engine of the kind in name order. It returns the
// pool's own errors (*PoolFullError, *UnavailableError) for the transient cases and *NoSpeechEngineError for the hard ones. It never holds the lock
// while it starts an engine.
func (m *Manager) ResolveSpeech(ctx context.Context, req SpeechRequest) (SpeechLease, error) {
	if !ValidSpeechMode(req.Mode) {
		return SpeechLease{}, &NoSpeechEngineError{Mode: req.Mode, Engine: req.Engine, Reason: "unknown kind of speech"}
	}
	name, err := m.pickSpeechEngine(ctx, req)
	if err != nil {
		return SpeechLease{}, err
	}
	if err := m.leaseRoom(); err != nil { // refuse before starting an engine, not after: a request that is going to be turned away must not cold-start one
		return SpeechLease{}, err
	}
	m.mu.Lock()
	was := false
	if slot := m.slotOf(name); slot != "" {
		was = m.slots[slot].state == StateRunning
	}
	m.mu.Unlock()

	st, err := m.Ensure(ctx, name)
	if err != nil {
		return SpeechLease{}, err
	}
	o, err := m.d.Store.OccupantByName(ctx, name)
	if err != nil {
		return SpeechLease{}, err
	}
	port := st.Port
	if k, ok := KindByName(o.Kind); ok && k.FixedPort != 0 {
		port = k.FixedPort
	}
	id, expires, err := m.grantLease(name, st.Slot)
	if err != nil {
		return SpeechLease{}, err
	}
	return SpeechLease{ID: id, Engine: name, Slot: st.Slot, Mode: req.Mode, Kind: o.Kind, BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), Port: port,
		Expires: expires, Loaded: !was}, nil
}

// leaseRoom reports ErrTooManyLeases when the lease table is full (after dropping what has expired or gone stale).
func (m *Manager) leaseRoom() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.leases) >= maxLeases {
		m.leaseCountLocked("") // drops what has expired or gone stale
		if len(m.leases) >= maxLeases {
			return ErrTooManyLeases
		}
	}
	return nil
}

// grantLease takes a lease on the engine `name` running in `slot`, for every function (speech, embeddings, transcription, alignment, a pooled catalog
// config): the daemon cannot see a request in flight, so a lease is how it learns an engine is busy. It fails with *UnavailableError when the engine was
// evicted or stopped between Ensure and now (an address that no longer answers must not be handed out), and ErrTooManyLeases when the table is full.
func (m *Manager) grantLease(name, slot string) (id string, expires time.Time, err error) {
	idb := make([]byte, 8)
	if _, err := rand.Read(idb); err != nil {
		return "", time.Time{}, fmt.Errorf("specialist: lease id: %w", err)
	}
	id = hex.EncodeToString(idb)
	ttl := m.d.LeaseTTL
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.slots[slot]
	if r == nil || r.occupant != name || r.state != StateRunning {
		return "", time.Time{}, &UnavailableError{Occupant: name, Reason: "it was stopped while the request was being placed"}
	}
	if len(m.leases) >= maxLeases {
		m.leaseCountLocked(slot) // drops what has expired before refusing
		if len(m.leases) >= maxLeases {
			return "", time.Time{}, ErrTooManyLeases
		}
	}
	now := m.d.Now()
	r.lastUse = now
	m.leases[id] = &speechLease{slot: slot, occupant: name, loadedAt: r.loadedAt, expires: now.Add(ttl)}
	return id, now.Add(ttl), nil
}

// RenewLease pushes a lease's expiry a full TTL ahead, for a request that outlives the TTL (a long stream). It reports false when the lease is gone
// (released, expired, or its engine was reloaded), which tells the renewer to stop.
func (m *Manager) RenewLease(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return false
	}
	r := m.slots[l.slot]
	now := m.d.Now()
	if !now.Before(l.expires) || r == nil || r.occupant != l.occupant || !r.loadedAt.Equal(l.loadedAt) {
		delete(m.leases, id)
		return false
	}
	ttl := m.d.LeaseTTL
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	l.expires = now.Add(ttl)
	return true
}

// usable reports whether an engine may be chosen for a voice that names none: enabled, and not stopped by the operator.
func usable(o store.Occupant) bool { return o.Enabled && !o.Stopped }

// pickSpeechEngine applies the resolution order and returns an engine name.
func (m *Manager) pickSpeechEngine(ctx context.Context, req SpeechRequest) (string, error) {
	check := func(o store.Occupant) error {
		mode, ok := SpeechModeOf(o)
		if !ok {
			return &NoSpeechEngineError{Mode: req.Mode, Engine: o.Name, Reason: "it is not a speech engine"}
		}
		if mode != req.Mode {
			return &NoSpeechEngineError{Mode: req.Mode, Engine: o.Name, Reason: "it renders " + mode + ", not " + req.Mode}
		}
		if !o.Enabled {
			return &UnavailableError{Occupant: o.Name, Reason: ReasonDisabled}
		}
		if o.Stopped {
			return &UnavailableError{Occupant: o.Name, Reason: ReasonStopped}
		}
		return nil
	}
	if req.Engine != "" {
		o, err := m.d.Store.OccupantByName(ctx, req.Engine)
		if errors.Is(err, store.ErrNotFound) {
			return "", &NoSpeechEngineError{Mode: req.Mode, Engine: req.Engine, Reason: "no such engine"}
		}
		if err != nil {
			return "", err
		}
		if err := check(o); err != nil {
			return "", err
		}
		return o.Name, nil
	}

	list, err := m.d.Store.ListOccupants(ctx)
	if err != nil {
		return "", err
	}
	var defaults map[string]string
	if m.d.SpeechDefaults != nil {
		if defaults, err = m.d.SpeechDefaults(ctx); err != nil {
			return "", fmt.Errorf("read the speech defaults: %w", err)
		}
	}
	o, err := DefaultSpeechEngine(list, defaults, req.Mode)
	if err != nil {
		return "", err
	}
	return o.Name, nil
}

// DefaultSpeechEngine is the engine a voice that names none gets for a kind of speech: the default for the kind if it can be used, otherwise the first
// usable engine of the kind in name order. It returns *NoSpeechEngineError when no engine of the kind is registered and *UnavailableError when every one
// is turned off or stopped. The request path and the readiness report both use it, so they cannot disagree.
func DefaultSpeechEngine(list []store.Occupant, defaults map[string]string, mode string) (store.Occupant, error) {
	var ofMode []store.Occupant
	for _, o := range list {
		if m, ok := SpeechModeOf(o); ok && m == mode {
			ofMode = append(ofMode, o)
		}
	}
	if len(ofMode) == 0 {
		return store.Occupant{}, &NoSpeechEngineError{Mode: mode, Reason: "no engine of that kind is registered"}
	}
	sort.Slice(ofMode, func(i, j int) bool { return ofMode[i].Name < ofMode[j].Name })

	if want := defaults[mode]; want != "" {
		for _, o := range ofMode {
			if o.Name == want && usable(o) {
				return o, nil
			}
		}
	}
	for _, o := range ofMode {
		if usable(o) {
			return o, nil
		}
	}
	// Engines of the kind exist but none can be used: that is the operator's choice (turned off or stopped), said by name.
	for _, o := range ofMode {
		if !o.Enabled {
			return store.Occupant{}, &UnavailableError{Occupant: o.Name, Reason: ReasonDisabled}
		}
	}
	return store.Occupant{}, &UnavailableError{Occupant: ofMode[0].Name, Reason: ReasonStopped}
}

// ReleaseSpeech gives a lease back when its request has finished and marks the engine just used (so the idle clock starts from the END of the request).
// It reports whether the lease existed; releasing an unknown, expired or already released lease is harmless.
func (m *Manager) ReleaseSpeech(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return false
	}
	delete(m.leases, id)
	if r := m.slots[l.slot]; r != nil && r.occupant == l.occupant && r.loadedAt.Equal(l.loadedAt) {
		r.lastUse = m.d.Now()
	}
	return true
}

// SpeechDefaultsKey is the settings key holding the default engine per kind of speech: a JSON object {"base": "engine-name", ...}. Replaces the four
// fixed rows of `tts.engines` (docs/speech-routing-plan.md).
const SpeechDefaultsKey = "tts.defaults"

// ParseSpeechDefaults reads the stored defaults. An empty value means none are set; a stored kind this version does not know is kept out of the result
// rather than failing the read, so an older daemon reading a newer file still serves the kinds it knows.
func ParseSpeechDefaults(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("specialist: %s is not a JSON object: %w", SpeechDefaultsKey, err)
	}
	for k, v := range in {
		var name string
		if !ValidSpeechMode(k) || json.Unmarshal(v, &name) != nil || name == "" {
			continue // a kind or a value this version does not understand is left out, never an error for the rest
		}
		out[k] = name
	}
	return out, nil
}

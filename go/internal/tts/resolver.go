// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where a synthesis request goes (docs/speech-routing-plan.md). A Resolver answers "where, for this kind of speech and (optionally) this named engine?"
// per request. The daemon resolver asks the Forge daemon, which loads the engine on demand, never evicts it while the request runs (the lease) and
// answers with a clear reason when it cannot.

// Resolved is where one request goes and how to give it back.
type Resolved struct {
	URL    string
	Engine string // the engine the daemon chose ("" for the static resolver)
	// Kind is the engine's launch kind as the daemon reports it ("qwen-tts-server", "zonos3-server", ...; "" for the static resolver or a daemon that does
	// not say). It selects the wire protocol: Qwen's /v1/audio/speech, or Zonos's /v2/generate.
	Kind string
	// Release must be called when the request has finished (it is never nil). For the daemon resolver it gives the lease back, which also restarts the
	// engine's idle clock from the END of the request.
	Release func()
}

// Resolver finds the address for a request.
type Resolver interface {
	Resolve(ctx context.Context, mode VoiceMode, engine string) (Resolved, error)
	// Status describes the resolver for /health (never contacts an engine).
	Status() map[string]any
}

// EngineError means no engine could be resolved. It is a decision, not a crash: Transient says whether trying again shortly can change the answer
// (the engine is loading, or the pool is full) or not (the engine is turned off, or nothing can play this voice).
type EngineError struct {
	Code       string
	Message    string
	Engine     string
	Transient  bool
	RetryAfter time.Duration
}

func (e *EngineError) Error() string { return e.Message }

// IsEngineError reports whether err is (or wraps) an *EngineError and returns it.
func IsEngineError(err error) (*EngineError, bool) {
	var ee *EngineError
	if errors.As(err, &ee) {
		return ee, true
	}
	return nil, false
}

// ---- daemon ----------------------------------------------------------------------------------------------------------------------------------

// DaemonResolverConfig configures the daemon resolver.
type DaemonResolverConfig struct {
	BaseURL string // the Forge daemon, for example http://127.0.0.1:5000
	Key     string // a speech-scoped key (forge mint-key -scope speech -bind-ip 127.0.0.1)
	// Timeout bounds one resolve call (an engine that has to be started can take a while). Zero = 60 s.
	Timeout time.Duration
	// CacheFor is how long the last known address of an engine is still used when the daemon cannot be reached. Zero = 2 minutes.
	CacheFor time.Duration
	// KeyExpires is when the key stops working, for /health only (the daemon does not say). Zero = unknown.
	KeyExpires time.Time
	Client     *http.Client
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// DaemonResolver asks the Forge daemon for an engine address and a lease on every request.
type DaemonResolver struct {
	c DaemonResolverConfig

	mu        sync.Mutex
	cache     map[string]cachedAddr
	lastOK    time.Time
	lastError string
	// notified is set when a refused key has been logged, so a refused key is shouted once per change rather than once per request.
	refused bool

	engines   []EngineInfo
	enginesAt time.Time
}

type cachedAddr struct {
	url, engine, kind string
	at                time.Time
}

// NewDaemonResolver builds the resolver. It refuses to build without an address and a key that looks like one: a missing key must stop forge-tts from
// starting in this mode, not quietly turn into a daemon it cannot talk to (the 2026-09 incident was a token variable that was silently unset).
func NewDaemonResolver(c DaemonResolverConfig) (*DaemonResolver, error) {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.Key = strings.TrimSpace(c.Key)
	if c.BaseURL == "" {
		return nil, errors.New("the daemon resolver needs FORGE_TTS_DAEMON_URL")
	}
	if c.Key == "" {
		return nil, errors.New("the daemon resolver needs FORGE_TTS_DAEMON_KEY (a speech-scoped key: forge mint-key -scope speech -bind-ip 127.0.0.1 -ttl ...)")
	}
	if !strings.HasPrefix(c.Key, "sk-forge-") {
		return nil, errors.New("FORGE_TTS_DAEMON_KEY does not look like a Forge key (it should start with sk-forge-)")
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.CacheFor <= 0 {
		c.CacheFor = 2 * time.Minute
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return &DaemonResolver{c: c, cache: map[string]cachedAddr{}}, nil
}

func (d *DaemonResolver) Status() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]any{"resolver": "daemon", "daemon_url": d.c.BaseURL}
	if !d.lastOK.IsZero() {
		out["last_ok"] = d.lastOK.UTC().Format(time.RFC3339)
	}
	if d.lastError != "" {
		out["last_error"] = d.lastError
	}
	if d.refused {
		out["key_refused"] = true
		out["degraded"] = true
	}
	if d.c.KeyExpires.IsZero() {
		out["key_expires"] = "unknown (set FORGE_TTS_DAEMON_KEY_EXPIRES to the date the key expires)"
	} else {
		left := d.c.KeyExpires.Sub(d.c.Now())
		out["key_expires_at"] = d.c.KeyExpires.UTC().Format(time.RFC3339)
		out["key_expires_in_days"] = int(left.Hours() / 24)
		if left < 30*24*time.Hour {
			out["key_warning"] = "the daemon key expires soon: mint a new one and restart forge-tts"
		}
	}
	return out
}

type daemonOK struct {
	Engine  string `json:"engine"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	LeaseID string `json:"lease_id"`
}

type daemonErr struct {
	Code   string         `json:"code"`
	Error  string         `json:"error"`
	Params map[string]any `json:"params"`
}

func (d *DaemonResolver) Resolve(ctx context.Context, mode VoiceMode, engine string) (Resolved, error) {
	body, _ := json.Marshal(map[string]string{"mode": string(mode), "engine": engine})
	cctx, cancel := context.WithTimeout(ctx, d.c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, d.c.BaseURL+"/api/v1/speech/resolve", bytes.NewReader(body))
	if err != nil {
		return Resolved{}, err
	}
	req.Header.Set("Authorization", "Bearer "+d.c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.c.Client.Do(req)
	if err != nil {
		return d.unreachable(ctx, mode, engine, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
		var ok daemonOK
		if err := json.Unmarshal(raw, &ok); err != nil || ok.BaseURL == "" {
			// The daemon may already have granted a lease we cannot use: give it back rather than pin the engine for the lease's lifetime.
			if ok.LeaseID != "" {
				d.releaser(ok.LeaseID)()
			}
			return Resolved{}, d.fail(&EngineError{Code: "model_not_ready", Transient: true, Message: "the engine service gave an answer forge-tts could not read"}, "unreadable resolve answer")
		}
		d.mu.Lock()
		d.cache[cacheKey(mode, engine)] = cachedAddr{url: strings.TrimRight(ok.BaseURL, "/"), engine: ok.Engine, kind: ok.Kind, at: d.c.Now()}
		d.lastOK, d.lastError, d.refused = d.c.Now(), "", false
		d.mu.Unlock()
		return Resolved{URL: strings.TrimRight(ok.BaseURL, "/"), Engine: ok.Engine, Kind: ok.Kind, Release: d.releaser(ok.LeaseID)}, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		d.mu.Lock()
		first := !d.refused
		d.refused = true
		d.mu.Unlock()
		if first {
			d.c.Logf("forge-tts: the daemon REFUSED its key (HTTP %d): it has expired, been revoked or is not a speech key bound to this host. Speech will fail until a new key is set in FORGE_TTS_DAEMON_KEY.", resp.StatusCode)
		}
		return Resolved{}, d.fail(&EngineError{Code: "speech_resolver_unauthorized", Message: "forge-tts's key for the engine service was refused; an operator has to mint a new one"},
			fmt.Sprintf("key refused (HTTP %d)", resp.StatusCode))
	}

	var de daemonErr
	_ = json.Unmarshal(raw, &de)
	retry := retryAfter(resp.Header.Get("Retry-After"))
	transient := true
	if t, has := de.Params["transient"].(bool); has {
		transient = t
	} else if resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		transient = false
	}
	msg := de.Error
	if msg == "" {
		msg = fmt.Sprintf("the engine service answered HTTP %d", resp.StatusCode)
	}
	code := de.Code
	if code == "" {
		code = "speech_resolver_error"
	}
	engineName, _ := de.Params["engine"].(string)
	return Resolved{}, d.fail(&EngineError{Code: code, Message: msg, Engine: engineName, Transient: transient, RetryAfter: retry}, code)
}

// unreachable handles a daemon that did not answer. Engines keep running without the daemon, so a recently known address is still used (without a
// lease); with none, the request gets a transient answer that callers retry.
func (d *DaemonResolver) unreachable(ctx context.Context, mode VoiceMode, engine string, cause error) (Resolved, error) {
	if ctx.Err() != nil {
		return Resolved{}, &EngineError{Code: "model_not_ready", Transient: true, RetryAfter: time.Second, Message: "the request was cancelled while the engine was being found"}
	}
	d.mu.Lock()
	c, have := d.cache[cacheKey(mode, engine)]
	d.lastError = "the daemon did not answer: " + cause.Error()
	d.mu.Unlock()
	if have && d.c.Now().Sub(c.at) <= d.c.CacheFor {
		d.c.Logf("forge-tts: the daemon did not answer (%v); using the last known address of the %s engine", cause, mode)
		return Resolved{URL: c.url, Engine: c.engine, Kind: c.kind, Release: func() {}}, nil
	}
	return Resolved{}, &EngineError{Code: "model_not_ready", Transient: true, RetryAfter: 2 * time.Second,
		Message: "the engine service did not answer, and no recent address for this engine is known"}
}

func (d *DaemonResolver) fail(e *EngineError, why string) error {
	d.mu.Lock()
	d.lastError = why
	d.mu.Unlock()
	return e
}

// releaser gives the lease back once, without making the caller wait for it. An unreleased lease only costs the engine ten minutes of protection.
func (d *DaemonResolver) releaser(id string) func() {
	var once sync.Once
	return func() {
		if id == "" {
			return
		}
		once.Do(func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				body, _ := json.Marshal(map[string]string{"lease_id": id})
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.c.BaseURL+"/api/v1/speech/release", bytes.NewReader(body))
				if err != nil {
					return
				}
				req.Header.Set("Authorization", "Bearer "+d.c.Key)
				req.Header.Set("Content-Type", "application/json")
				if resp, err := d.c.Client.Do(req); err != nil {
					d.c.Logf("forge-tts: could not give back lease %s: %v (it expires on its own)", id, err)
				} else {
					resp.Body.Close()
				}
			}()
		})
	}
}

func cacheKey(mode VoiceMode, engine string) string { return string(mode) + "|" + engine }

func retryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 && n < 3600 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// ---- the engine list ------------------------------------------------------------------------------------------------------------------------

// EngineInfo describes one speech engine for a caller that offers a choice (voice-studio's Engine picker).
type EngineInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Mode    string `json:"mode"`
	Policy  string `json:"policy"`
	Enabled bool   `json:"enabled"`
	Stopped bool   `json:"stopped"`
	State   string `json:"state"`
}

// engineListFresh is how long a fetched engine list is reused: the picker is opened by a person, and states change slowly.
const engineListFresh = 10 * time.Second

// Engines lists the speech engines the daemon knows. It never starts one. When the daemon cannot be reached the last list is served if there is one.
func (d *DaemonResolver) Engines(ctx context.Context) ([]EngineInfo, error) {
	d.mu.Lock()
	if d.engines != nil && d.c.Now().Sub(d.enginesAt) < engineListFresh {
		out := d.engines
		d.mu.Unlock()
		return out, nil
	}
	d.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, d.c.BaseURL+"/api/v1/speech/defaults", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.c.Key)
	resp, err := d.c.Client.Do(req)
	var list []EngineInfo
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("the engine service answered HTTP %d", resp.StatusCode)
		} else {
			var body struct {
				EngineInfo []EngineInfo `json:"engine_info"`
			}
			if derr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); derr != nil {
				err = derr
			} else {
				list = body.EngineInfo
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if d.engines != nil {
			return d.engines, nil
		}
		return nil, err
	}
	if list == nil {
		list = []EngineInfo{}
	}
	d.engines, d.enginesAt = list, d.c.Now()
	return list, nil
}

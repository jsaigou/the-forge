// SPDX-License-Identifier: Apache-2.0

package duplexremote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"golang.org/x/net/websocket"
)

// Config locates one tenant on an engine.
type Config struct {
	// BaseURL is the engine's loopback HTTP base, e.g. http://127.0.0.1:9000.
	BaseURL string
	// Tenant is the engine-side tenant name sent in start (one Tenant value per
	// engine tenant; the platform picks the tenant by name).
	Tenant string
	// Token is an optional bearer key. It is sent only as a header and never
	// logged or put in an error message.
	Token string
	// StepTimeout bounds one Step. The engine is passive, so the platform owns
	// this deadline (engine-contract.md §5.2). Default 1 s.
	StepTimeout time.Duration
	// StartTimeout bounds the connect, start and ready exchange. Default 120 s,
	// the same as the service's driver start budget.
	StartTimeout time.Duration
	// HTTPClient is used for /duplex/v1/info. Default: a 5 s client.
	HTTPClient *http.Client
	// CardRenderer, when non-nil, turns the client's structured card into the
	// value sent as "card" in the start message. The engine accepts either an
	// object or a plain string, so a renderer lets clients keep sending the
	// structured card while the engine receives rendered text. nil passes
	// cfg.Card through unchanged.
	CardRenderer func(card map[string]any) (any, error)
	// Locator, when non-nil, replaces BaseURL: each Start asks it for the engine (loading it on demand) and a lease, keeps the lease renewed while the
	// session lives and gives it back at Close (ADR-0029). The engine is not running when the service starts, so Load then answers from the
	// configuration (the engine is checked at every Start).
	Locator Locator
	// Engine is the pool engine's name for the Locator; empty means Tenant.
	Engine string
	// RenewInterval, when positive, fixes how often a lease is renewed; otherwise a third of the lease's lifetime, at least a second. For tests.
	RenewInterval time.Duration
}

// Tenant is a duplex.DuplexTenant backed by an out-of-process engine.
type Tenant struct {
	cfg Config

	mu      sync.Mutex
	maxRefs int64 // the engine's advertised reference limit from the last Load; 0 = not stated
}

// New returns a remote tenant. It does no I/O; Load and Start do.
func New(cfg Config) *Tenant {
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = time.Second
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 120 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Tenant{cfg: cfg}
}

type engineInfo struct {
	Contract string `json:"contract"`
	Tenants  []struct {
		Name string `json:"name"`
		Text bool   `json:"text"`
	} `json:"tenants"`
	Frame struct {
		SampleRate int    `json:"sample_rate"`
		Samples    int    `json:"samples"`
		Format     string `json:"format"`
	} `json:"frame"`
	Ready             bool  `json:"ready"`
	MaxReferenceBytes int64 `json:"max_reference_bytes"`
}

func unavailable(format string, a ...any) *duplex.Error {
	return &duplex.Error{Code: duplex.ErrTenantUnavailable, Message: fmt.Sprintf(format, a...), Fatal: true}
}

// Load reads /duplex/v1/info and checks that this engine can serve the
// platform: a known contract version, the platform's frame format, the named
// tenant, and a ready model. It is safe to call repeatedly; Start calls it per
// session because the driver does not.
func (t *Tenant) Load(ctx context.Context) (duplex.TenantInfo, error) {
	if t.cfg.Locator != nil {
		// No engine is running yet (it loads on demand at the first Start), so there is nothing to ask. Haru-class tenants speak text events.
		return duplex.TenantInfo{Name: t.cfg.Tenant, Capabilities: duplex.Capabilities{Text: true, InputRates: []int{duplex.SampleRate}}}, nil
	}
	return t.load(ctx, t.cfg.BaseURL)
}

// load reads /duplex/v1/info of the engine at base and checks it as Load describes.
func (t *Tenant) load(ctx context.Context, base string) (duplex.TenantInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+pathInfo, nil)
	if err != nil {
		return duplex.TenantInfo{}, unavailable("engine url: %v", err)
	}
	if t.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.Token)
	}
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return duplex.TenantInfo{}, unavailable("engine info unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return duplex.TenantInfo{}, unavailable("engine info returned HTTP %d", resp.StatusCode)
	}
	var info engineInfo
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxControlSize)).Decode(&info); err != nil {
		return duplex.TenantInfo{}, unavailable("engine info is not valid JSON")
	}
	if info.Contract != ContractVersion {
		return duplex.TenantInfo{}, unavailable("engine speaks contract %q, this platform speaks %q", info.Contract, ContractVersion)
	}
	if info.Frame.SampleRate != duplex.SampleRate || info.Frame.Samples != duplex.FrameSamples || info.Frame.Format != "f32le" {
		return duplex.TenantInfo{}, unavailable("engine frame format %d Hz x %d %s does not match the platform", info.Frame.SampleRate, info.Frame.Samples, info.Frame.Format)
	}
	for _, tn := range info.Tenants {
		if tn.Name != t.cfg.Tenant {
			continue
		}
		if !info.Ready {
			return duplex.TenantInfo{}, &duplex.Error{Code: duplex.ErrModelNotReady, Message: "engine is still loading the model"}
		}
		t.mu.Lock()
		t.maxRefs = info.MaxReferenceBytes
		t.mu.Unlock()
		return duplex.TenantInfo{
			Name:         tn.Name,
			Capabilities: duplex.Capabilities{Text: tn.Text, InputRates: []int{duplex.SampleRate}},
		}, nil
	}
	return duplex.TenantInfo{}, unavailable("engine has no tenant %q", t.cfg.Tenant)
}

// Close is a no-op: the tenant holds no connection outside a session.
func (t *Tenant) Close() error { return nil }

// engineError maps an engine {"type":"error"} message to a platform error.
// Unknown codes become internal; the engine's message is passed through, so the
// contract forbids secrets in it (engine-contract.md §6).
func engineError(data []byte) *duplex.Error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &e)
	code := duplex.ErrorCode(e.Code)
	switch code {
	case duplex.ErrModelNotReady, duplex.ErrTenantUnavailable, duplex.ErrSessionLimit,
		duplex.ErrBadFrame, duplex.ErrBadSequence, duplex.ErrTimeout, duplex.ErrInternal,
		duplex.ErrBadRequest:
	default:
		code = duplex.ErrInternal
	}
	// model_not_ready is retryable before ready; every other engine error is fatal.
	return &duplex.Error{Code: code, Message: e.Message, Fatal: code != duplex.ErrModelNotReady}
}

type startMsg struct {
	Type       string         `json:"type"`
	Contract   string         `json:"contract"`
	SessionID  string         `json:"session_id"`
	Tenant     string         `json:"tenant"`
	Text       bool           `json:"text"`
	MaxMinutes int            `json:"max_minutes"`
	Card       any            `json:"card,omitempty"`
	Reference  startReference `json:"reference"`
}

type startReference struct {
	ID         string `json:"id,omitempty"`
	Bytes      int    `json:"bytes"`
	Transcript string `json:"transcript,omitempty"`
}

// Start checks the engine, opens one lane and returns its session. With a Locator the engine is found (and loaded if it is not running) first and
// leased for the life of the session.
func (t *Tenant) Start(ctx context.Context, cfg duplex.TenantConfig) (duplex.TenantSession, error) {
	if t.cfg.Locator == nil {
		s, err := t.startOn(ctx, cfg, t.cfg.BaseURL)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	engine := t.cfg.Engine
	if engine == "" {
		engine = t.cfg.Tenant
	}
	lease, err := t.cfg.Locator.Locate(ctx, engine)
	if err != nil {
		return nil, err
	}
	s, err := t.startOn(ctx, cfg, strings.TrimRight(lease.BaseURL, "/"))
	if err != nil {
		// The lease must not outlive a start that failed: it would pin an engine nobody is using until it expired.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		t.cfg.Locator.Release(rctx, lease.ID)
		cancel()
		return nil, err
	}
	s.holdLease(t.cfg.Locator, lease, t.cfg.RenewInterval)
	return s, nil
}

// startOn opens one lane on the engine at base.
func (t *Tenant) startOn(ctx context.Context, cfg duplex.TenantConfig, base string) (*session, error) {
	var card any
	if cfg.Card != nil {
		card = cfg.Card
	}
	if t.cfg.CardRenderer != nil {
		rendered, err := t.cfg.CardRenderer(cfg.Card)
		if err != nil {
			return nil, &duplex.Error{Code: duplex.ErrBadRequest, Fatal: true,
				Message: fmt.Sprintf("card rejected: %v", err)}
		}
		card = rendered
	}
	if _, err := t.load(ctx, base); err != nil {
		return nil, err
	}
	t.mu.Lock()
	limit := t.maxRefs
	t.mu.Unlock()
	if limit > 0 && int64(len(cfg.Reference.Audio)) > limit {
		return nil, &duplex.Error{Code: duplex.ErrBadRequest, Fatal: true,
			Message: fmt.Sprintf("reference audio is %d bytes; the engine accepts at most %d", len(cfg.Reference.Audio), limit)}
	}
	ws, err := t.dial(ctx, base)
	if err != nil {
		return nil, err
	}
	s := &session{ws: ws, stepTimeout: t.cfg.StepTimeout}

	deadline := time.Now().Add(t.cfg.StartTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = ws.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = ws.SetDeadline(time.Now()) })
	defer stop()

	body, err := json.Marshal(startMsg{
		Type: "start", Contract: ContractVersion, SessionID: cfg.SessionID, Tenant: t.cfg.Tenant,
		Text: cfg.Text, MaxMinutes: cfg.MaxMinutes, Card: card,
		Reference: startReference{ID: cfg.Reference.ID, Bytes: len(cfg.Reference.Audio), Transcript: cfg.Reference.Transcript},
	})
	if err != nil {
		_ = ws.Close()
		return nil, &duplex.Error{Code: duplex.ErrBadRequest, Message: "card is not JSON-encodable", Fatal: true}
	}
	if err := sendText(ws, body); err != nil {
		_ = ws.Close()
		return nil, startFailure(ctx, err)
	}
	if len(cfg.Reference.Audio) > 0 {
		if err := sendReference(ws, cfg.Reference.Audio); err != nil {
			_ = ws.Close()
			return nil, startFailure(ctx, err)
		}
	}
	reply, err := recvMsg(ws)
	if err != nil {
		_ = ws.Close()
		return nil, startFailure(ctx, err)
	}
	if !reply.Text {
		_ = ws.Close()
		return nil, unavailable("engine answered start with binary data")
	}
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(reply.Data, &head)
	switch head.Type {
	case "ready":
		_ = ws.SetDeadline(time.Time{})
		return s, nil
	case "error":
		_ = ws.Close()
		return nil, engineError(reply.Data)
	default:
		_ = ws.Close()
		return nil, unavailable("engine answered start with %q", head.Type)
	}
}

func startFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &duplex.Error{Code: duplex.ErrCancelled, Message: "start cancelled", Fatal: true}
	}
	if isTimeout(err) {
		return &duplex.Error{Code: duplex.ErrTimeout, Message: "engine did not answer start in time", Fatal: true}
	}
	return unavailable("engine connection failed during start")
}

func (t *Tenant) dial(ctx context.Context, base string) (*websocket.Conn, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, unavailable("engine url: %v", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = pathSession
	origin := "http://" + u.Host
	wc, err := websocket.NewConfig(u.String(), origin)
	if err != nil {
		return nil, unavailable("engine url: %v", err)
	}
	if t.cfg.Token != "" {
		wc.Header = http.Header{"Authorization": []string{"Bearer " + t.cfg.Token}}
	}
	wc.Dialer = &net.Dialer{Timeout: 5 * time.Second}

	type result struct {
		ws  *websocket.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ws, err := websocket.DialConfig(wc)
		ch <- result{ws, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, unavailable("engine session endpoint unreachable")
		}
		return r.ws, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.ws != nil {
				_ = r.ws.Close()
			}
		}()
		return nil, &duplex.Error{Code: duplex.ErrCancelled, Message: "start cancelled", Fatal: true}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// session is one lane on the engine. Step and Flush are serialised: the
// contract allows one step in flight per lane.
type session struct {
	mu          sync.Mutex
	ws          *websocket.Conn
	stepTimeout time.Duration
	broken      error
	closed      bool

	// The lease on the engine this session runs on (nil locator: a fixed engine, nothing to keep).
	loc         Locator
	leaseID     string
	stopRenewal context.CancelFunc
}

// exchange sends one message and reads one reply under the step deadline.
// Callers hold s.mu.
func (s *session) exchange(ctx context.Context, send func() error) (wsMsg, error) {
	if s.closed {
		return wsMsg{}, &duplex.Error{Code: duplex.ErrCancelled, Message: "session closed", Fatal: true}
	}
	if s.broken != nil {
		return wsMsg{}, s.broken
	}
	if err := ctx.Err(); err != nil {
		return wsMsg{}, s.fail(&duplex.Error{Code: duplex.ErrCancelled, Message: "step cancelled", Fatal: true})
	}
	deadline := time.Now().Add(s.stepTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.ws.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = s.ws.SetDeadline(time.Now()) })
	defer stop()

	if err := send(); err != nil {
		return wsMsg{}, s.fail(s.ioError(ctx, err))
	}
	m, err := recvMsg(s.ws)
	if err != nil {
		return wsMsg{}, s.fail(s.ioError(ctx, err))
	}
	return m, nil
}

func (s *session) ioError(ctx context.Context, err error) *duplex.Error {
	switch {
	case ctx.Err() != nil:
		return &duplex.Error{Code: duplex.ErrCancelled, Message: "step cancelled", Fatal: true}
	case isTimeout(err):
		return &duplex.Error{Code: duplex.ErrTimeout, Message: "engine missed the step deadline", Fatal: true}
	default:
		return unavailable("engine connection lost")
	}
}

// fail marks the lane unusable. A socket with an unread or partial reply cannot
// be resynchronised, so one failed step ends the lane.
func (s *session) fail(e *duplex.Error) *duplex.Error {
	s.broken = e
	return e
}

// reply turns one engine message into a TenantFrame, or an error.
func (s *session) reply(m wsMsg, wantSeq uint32) (duplex.TenantFrame, error) {
	if m.Text {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Data, &head)
		if head.Type == "error" {
			return duplex.TenantFrame{}, s.fail(engineError(m.Data))
		}
		return duplex.TenantFrame{}, s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "engine sent an unexpected control message", Fatal: true})
	}
	seq, audio, text, err := decodeStepOut(m.Data)
	if err != nil {
		return duplex.TenantFrame{}, s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "engine reply is malformed: " + err.Error(), Fatal: true})
	}
	if seq != wantSeq {
		return duplex.TenantFrame{}, s.fail(&duplex.Error{Code: duplex.ErrBadSequence, Message: fmt.Sprintf("engine answered seq %d, want %d", seq, wantSeq), Fatal: true})
	}
	return duplex.TenantFrame{Audio: audio, Text: text}, nil
}

// Step sends one frame and returns the engine's one reply.
func (s *session) Step(ctx context.Context, in duplex.Frame, seq uint32) (duplex.TenantFrame, error) {
	if !in.Valid() {
		return duplex.TenantFrame{}, &duplex.Error{Code: duplex.ErrBadFrame, Message: "frame must be 3200 samples", Fatal: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.exchange(ctx, func() error { return sendBinary(s.ws, encodeStepIn(seq, in)) })
	if err != nil {
		return duplex.TenantFrame{}, err
	}
	return s.reply(m, seq)
}

// Flush asks the engine for a frame it buffered (duplex.Flusher). The reply is
// step-shaped; its seq is whatever the engine last answered, so it is not
// checked against a request.
func (s *session) Flush(ctx context.Context) (duplex.TenantFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.exchange(ctx, func() error { return sendText(s.ws, []byte(`{"type":"flush"}`)) })
	if err != nil {
		return duplex.TenantFrame{}, err
	}
	if m.Text {
		return s.reply(m, 0)
	}
	_, audio, text, derr := decodeStepOut(m.Data)
	if derr != nil {
		return duplex.TenantFrame{}, s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "engine flush reply is malformed: " + derr.Error(), Fatal: true})
	}
	return duplex.TenantFrame{Audio: audio, Text: text}, nil
}

// Close tells the engine to release the lane, closes the socket and gives the engine lease back. It is idempotent. Dropping the socket also
// releases the lane on the engine side.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.broken == nil {
		_ = s.ws.SetWriteDeadline(time.Now().Add(time.Second))
		_ = sendText(s.ws, []byte(`{"type":"close","reason":"closed"}`))
	}
	_ = s.ws.Close()
	loc, id, stop := s.loc, s.leaseID, s.stopRenewal
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if loc != nil && id != "" {
		// Not under s.mu: a slow daemon must not hold up anything else on the session.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		loc.Release(ctx, id)
		cancel()
	}
	return nil
}

// holdLease keeps the lease renewed for the life of the session. A lease that cannot be renewed ends the session: the engine under it is no longer
// protected from eviction, and a conversation must not be cut mid-sentence by a stop it could have been told about.
func (s *session) holdLease(loc Locator, l Lease, every time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.loc, s.leaseID, s.stopRenewal = loc, l.ID, cancel
	s.mu.Unlock()
	ttl := time.Until(l.Expires)
	if ttl <= 0 {
		ttl = 10 * time.Minute // the daemon's default lease TTL; only a locator that gives no expiry lands here
	}
	go s.renewLoop(ctx, loc, l.ID, ttl, every)
}

func (s *session) renewLoop(ctx context.Context, loc Locator, id string, ttl, every time.Duration) {
	next := func(remaining time.Duration) time.Duration {
		if every > 0 {
			return every
		}
		d := remaining / 3
		if d < time.Second {
			d = time.Second
		}
		return d
	}
	expires := time.Now().Add(ttl)
	wait := next(ttl)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := loc.Renew(rctx, id)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			expires = time.Now().Add(ttl)
			wait = next(ttl)
		case errors.Is(err, ErrLeaseGone):
			s.lose(unavailable("the engine was stopped or restarted under this conversation"))
			return
		default:
			// The daemon did not answer. Keep trying while the lease is still good; once it has lapsed the engine may already be gone.
			if !time.Now().Before(expires) {
				s.lose(unavailable("the engine lease could not be renewed and has expired"))
				return
			}
			wait = next(time.Until(expires))
			if wait > 2*time.Second && every <= 0 {
				wait = 2 * time.Second
			}
		}
	}
}

// lose marks the session unusable from outside a step: the next Step returns err and the platform ends the session and calls Close.
func (s *session) lose(err *duplex.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken == nil && !s.closed {
		s.broken = err
	}
}

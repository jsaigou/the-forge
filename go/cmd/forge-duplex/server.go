// SPDX-License-Identifier: Apache-2.0

// Command forge-duplex is the v1 duplex audio transport (docs/duplex/protocol.md).
// It owns the WebSocket endpoint, auth, wire limits and the session lifecycle;
// the frame clock, rings and model live behind duplex.DuplexDriver (WS1). The
// bundled mock driver exists for local runs and transport tests only.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexobs"
	"github.com/jsaigou/the-forge/internal/duplexsafety"

	"log"

	"golang.org/x/net/websocket"

	"github.com/jsaigou/the-forge/internal/duplexwire"
)

const (
	dupVersionProtocol = "forge-duplex.v1"

	defaultMaxLanes      = 4
	defaultMaxMinutes    = 30
	defaultIdleTimeout   = 10 * time.Second
	defaultStatsInterval = 10 * time.Second
	defaultDrainTimeout  = time.Second
	defaultTicketTTL     = 30 * time.Second
	defaultWriteTimeout  = 10 * time.Second
	defaultStartTimeout  = 120 * time.Second
)

// Options configures a Server. Zero values pick the documented defaults.
type Options struct {
	// NewDriver builds the driver for a session. Either it or NewDriverFor is required. It is called once per session.
	NewDriver func() duplex.DuplexDriver
	// NewDriverFor, when set, is used instead of NewDriver: it builds the driver for the tenant the client asked for (several speech-to-speech
	// models behind one service). An error is shown to the client as the start failure; a *duplex.Error keeps its code.
	NewDriverFor func(tenant string) (duplex.DuplexDriver, error)
	// NewSafety returns this session's text safety policy. It is called once
	// per session; nil means safety policy hooks are off.
	NewSafety func() duplexsafety.Policy
	// MaxLanes is the number of concurrent sessions (default 4).
	MaxLanes int
	// MaxMinutes caps a session's requested max_minutes (default 30).
	MaxMinutes int
	// MaxSessionDuration overrides the time limit in tests; 0 means MaxMinutes.
	MaxSessionDuration time.Duration
	// IdleTimeout closes a running session with timeout when no input arrives
	// (default 10s).
	IdleTimeout time.Duration
	// StatsInterval is the stats cadence (default 10s).
	StatsInterval time.Duration
	// DrainTimeout bounds the output flush after close/cancel (default 1s).
	DrainTimeout time.Duration
	// WriteTimeout bounds one WebSocket write (default 10s).
	WriteTimeout time.Duration
	// StartTimeout bounds driver.Start (default 120s).
	StartTimeout time.Duration
	// TicketTTL is the lifetime of a short-lived ticket (default 30s).
	TicketTTL time.Duration
	// Tokens are accepted bearer tokens. When empty, loopback/tailnet peers are
	// admitted without a token (the a0 tailnet-bypass policy).
	Tokens []string
	// AllowedOrigins adds browser origins beyond same-host and loopback
	// (entries are host or host:port).
	AllowedOrigins []string
	// AllowAnyOrigin disables the Origin check. Tailnet-only deployments that
	// want cross-origin browser clients can set this.
	AllowAnyOrigin bool
	// AllowedNets adds network prefixes beyond loopback, 100.64.0.0/10 and
	// fd7a:115c:a1e0::/48 (tailnet).
	AllowedNets []netip.Prefix
	// DefaultTenant is used when session.start omits tenant (default loopback).
	DefaultTenant string
	// Logger receives lifecycle lines without tokens or audio (default log.Default()).
	Logger *log.Logger
}

func (o Options) withDefaults() Options {
	if o.MaxLanes <= 0 {
		o.MaxLanes = defaultMaxLanes
	}
	if o.MaxMinutes <= 0 {
		o.MaxMinutes = defaultMaxMinutes
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.StatsInterval <= 0 {
		o.StatsInterval = defaultStatsInterval
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = defaultDrainTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = defaultStartTimeout
	}
	if o.TicketTTL <= 0 {
		o.TicketTTL = defaultTicketTTL
	}
	if o.DefaultTenant == "" {
		o.DefaultTenant = "loopback"
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return o
}

// Server is the HTTP surface and the session manager.
type Server struct {
	opts    Options
	lanes   chan struct{}
	tickets *ticketStore
	obs     *duplexobs.Registry
	mux     *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
	killed   bool
	killWhy  string
}

// New builds a Server. It panics when NewDriver is nil because a server that
// can never start a session is a configuration mistake, not a runtime state.
func New(opts Options) *Server {
	if opts.NewDriver == nil && opts.NewDriverFor == nil {
		panic("forge-duplex: Options.NewDriver or Options.NewDriverFor is required")
	}
	opts = opts.withDefaults()
	s := &Server{
		opts:     opts,
		lanes:    make(chan struct{}, opts.MaxLanes),
		tickets:  newTicketStore(opts.TicketTTL),
		obs:      duplexobs.NewRegistry(duplexobs.Config{}),
		sessions: make(map[string]*session),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/v1/audio/duplex/ticket", s.handleTicket)
	s.mux.HandleFunc("/v1/audio/duplex", s.handleDuplex)
	s.mux.HandleFunc("/", s.handleRoot)
	return s
}

// Handler returns the HTTP handler; main wraps it in http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

// handleMetrics is the WS5 aggregate: active sessions plus lifetime counters,
// in Prometheus text format. It is tailnet/localhost-only like the rest of the
// service (the HTTP server is bound by the caller).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := s.obs.WriteMetrics(w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Kill closes every live session and refuses new ones until the process
// restarts. It is the transport half of the security kill switch (WS9 owns
// policy and the operator surface).
func (s *Server) Kill(reason string) {
	s.mu.Lock()
	s.killed = true
	s.killWhy = reason
	live := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.kill(reason)
	}
}

func (s *Server) isKilled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killed
}

func (s *Server) register(sess *session) {
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
}

func (s *Server) unregister(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *Server) acquireLane() bool {
	select {
	case s.lanes <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseLane() {
	select {
	case <-s.lanes:
	default:
	}
}

func (s *Server) lanesInUse() int { return len(s.lanes) }

// --- peer and origin policy -------------------------------------------------

var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}
	}
	if addr.IsLoopback() && r.Header.Get("X-Forwarded-For") != "" {
		first := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if a, err := netip.ParseAddr(first); err == nil {
			return a
		}
	}
	return addr
}

// peerAllowed is the transport's network gate: loopback, Tailscale CGNAT/ULA
// or an explicitly configured prefix. It mirrors authz.IsTailnetAddr and
// EffectiveRemoteAddr without pulling the store into this small service.
func (s *Server) peerAllowed(r *http.Request) bool {
	addr := remoteAddr(r)
	if !addr.IsValid() {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	if addr.Is4() && tailnetV4.Contains(addr) {
		return true
	}
	if addr.Is6() && tailnetV6.Contains(addr) {
		return true
	}
	for _, p := range s.opts.AllowedNets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *Server) originAllowed(r *http.Request) bool {
	if s.opts.AllowAnyOrigin {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := origin
	if i := strings.Index(origin, "://"); i >= 0 {
		host = origin[i+3:]
	}
	if i := strings.IndexAny(host, "/"); i >= 0 {
		host = host[:i]
	}
	if strings.EqualFold(host, r.Host) {
		return true
	}
	hostname := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		hostname = hh
	}
	hostname = strings.Trim(hostname, "[]")
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" {
		return true
	}
	for _, allowed := range s.opts.AllowedOrigins {
		// An entry may be a bare host, host:port, or a full origin such as https://host; the scheme carries no extra meaning here.
		if i := strings.Index(allowed, "://"); i >= 0 {
			allowed = strings.TrimRight(allowed[i+3:], "/")
		}
		if strings.EqualFold(allowed, host) || strings.EqualFold(allowed, hostname) {
			return true
		}
	}
	return false
}

// --- auth ------------------------------------------------------------------

func bearerHeader(r *http.Request) (string, bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

func offeredProtocol(req *http.Request) []string {
	raw := strings.TrimSpace(req.Header.Get("Sec-Websocket-Protocol"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func bearerFromProtocols(protos []string) (string, bool) {
	for _, p := range protos {
		if strings.HasPrefix(p, "bearer.") {
			return strings.TrimPrefix(p, "bearer."), true
		}
	}
	return "", false
}

func ticketFromProtocols(protos []string) (string, bool) {
	for _, p := range protos {
		if strings.HasPrefix(p, "ticket.") {
			return strings.TrimPrefix(p, "ticket."), true
		}
	}
	return "", false
}

func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) tokenOK(token string) bool {
	for _, want := range s.opts.Tokens {
		if tokenEqual(token, want) {
			return true
		}
	}
	return false
}

// authorizeWS runs during the handshake, before the upgrade and before any
// audio can exist. A failure is an HTTP 403: a client that has not completed
// the upgrade cannot receive a wire error frame.
func (s *Server) authorizeWS(r *http.Request) bool {
	if s.isKilled() {
		return false
	}
	if !s.peerAllowed(r) || !s.originAllowed(r) {
		return false
	}
	if len(s.opts.Tokens) == 0 {
		return true
	}

	protos := offeredProtocol(r)
	if tok, ok := bearerHeader(r); ok && s.tokenOK(tok) {
		return true
	}
	if tok, ok := bearerFromProtocols(protos); ok && s.tokenOK(tok) {
		return true
	}
	if ticket, ok := ticketFromProtocols(protos); ok && s.tickets.consume(ticket) {
		return true
	}
	if ticket := r.URL.Query().Get("ticket"); ticket != "" && s.tickets.consume(ticket) {
		return true
	}
	return false
}

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in_s"`
}

func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.isKilled() {
		http.Error(w, "service disabled", http.StatusServiceUnavailable)
		return
	}
	if !s.peerAllowed(r) || !s.originAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if len(s.opts.Tokens) > 0 {
		tok, ok := bearerHeader(r)
		if !ok || !s.tokenOK(tok) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	ticket, ok := s.tickets.mint()
	if !ok {
		http.Error(w, "cannot mint ticket", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(ticketResponse{Ticket: ticket, ExpiresIn: int(s.opts.TicketTTL / time.Second)})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.peerAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	killed := s.killed
	n := len(s.sessions)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         !killed,
		"sessions":   n,
		"max_lanes":  s.opts.MaxLanes,
		"lanes_used": s.lanesInUse(),
		"killed":     killed,
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("forge-duplex: transport endpoint is /v1/audio/duplex; the browser client is not bundled in this build (WS3).\n"))
}

// --- websocket -------------------------------------------------------------

var messageCodec = websocket.Codec{
	Marshal: func(v any) ([]byte, byte, error) {
		return nil, 0, websocket.ErrNotSupported
	},
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		m := v.(*wireMessage)
		m.Type = payloadType
		m.Data = append(m.Data[:0], data...)
		return nil
	},
}

type wireMessage struct {
	Type byte
	Data []byte
}

func readWireMessage(conn *websocket.Conn) (wireMessage, error) {
	var m wireMessage
	if err := messageCodec.Receive(conn, &m); err != nil {
		return m, err
	}
	return reassembleText(m, func() (wireMessage, error) {
		var next wireMessage
		err := messageCodec.Receive(conn, &next)
		return next, err
	})
}

// reassembleText completes a text message that arrived in pieces. Browsers split a message larger than about 128 KB into WebSocket frames, and this
// library hands every frame back as if it were a message (it does not expose the FIN bit), so a session.start carrying a reference voice arrived as
// a truncated first piece (found with a real browser, 2026-11-20). Control messages are single JSON objects, so a text message that is not complete
// JSON and is already larger than the control limit can only be the start of a fragmented session.start: read the following pieces until the JSON is
// complete, never beyond duplexwire.MaxStartMessage. Anything smaller is left to fail at once, as before.
func reassembleText(first wireMessage, next func() (wireMessage, error)) (wireMessage, error) {
	if first.Type != websocket.TextFrame || len(first.Data) <= duplexwire.MaxControlMessage || json.Valid(first.Data) {
		return first, nil
	}
	for !json.Valid(first.Data) {
		if len(first.Data) > duplexwire.MaxStartMessage {
			return first, nil // too large: the decoder refuses it with its own error
		}
		part, err := next()
		if err != nil {
			return first, err
		}
		if part.Type != websocket.TextFrame {
			return first, nil // an audio frame in the middle of a message: leave it to the decoder to refuse the fragment
		}
		first.Data = append(first.Data, part.Data...)
	}
	return first, nil
}

func (s *Server) handleDuplex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ws := websocket.Server{
		Config: websocket.Config{},
		Handshake: func(cfg *websocket.Config, req *http.Request) error {
			if !s.authorizeWS(req) {
				return errForbidden
			}
			// Echo the stable subprotocol and never echo a bearer/ticket value.
			cfg.Protocol = nil
			for _, p := range offeredProtocol(req) {
				if p == dupVersionProtocol {
					cfg.Protocol = []string{dupVersionProtocol}
					break
				}
			}
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			// The first message may carry the reference voice (duplexwire.MaxStartMessage); once session.start has been read the limit drops to
			// the size of an audio frame's allowance (session.go).
			conn.MaxPayloadBytes = duplexwire.MaxStartMessage
			sess := newSession(s, conn)
			s.register(sess)
			defer s.unregister(sess.id)
			sess.run()
		},
	}
	ws.ServeHTTP(w, r)
}

var errForbidden = &forbiddenError{}

type forbiddenError struct{}

func (*forbiddenError) Error() string { return "forbidden" }

// --- session ids and tickets ----------------------------------------------

var sessionIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "dup_" + strings.Repeat("0", 26)
	}
	return "dup_" + strings.ToLower(sessionIDEncoding.EncodeToString(b))
}

type ticketStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{ttl: ttl, m: make(map[string]time.Time)}
}

func (t *ticketStore) mint() (string, bool) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	id := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	now := time.Now()
	t.mu.Lock()
	for k, exp := range t.m {
		if now.After(exp) {
			delete(t.m, k)
		}
	}
	t.m[id] = now.Add(t.ttl)
	t.mu.Unlock()
	return id, true
}

func (t *ticketStore) consume(id string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[id]
	if !ok {
		return false
	}
	delete(t.m, id)
	return now.Before(exp)
}

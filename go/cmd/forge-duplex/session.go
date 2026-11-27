package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexobs"
	"github.com/jsaigou/the-forge/internal/duplexsafety"
	"github.com/jsaigou/the-forge/internal/duplexwire"
	"golang.org/x/net/websocket"
)

type sessionState int

const (
	stateIdle sessionState = iota
	stateStarting
	stateRunning
	stateDraining
	stateClosed
	stateError
)

func (s sessionState) String() string {
	switch s {
	case stateIdle:
		return "idle"
	case stateStarting:
		return "starting"
	case stateRunning:
		return "running"
	case stateDraining:
		return "draining"
	case stateClosed:
		return "closed"
	case stateError:
		return "error"
	default:
		return "unknown"
	}
}

type readResult struct {
	msg wireMessage
	err error
}

type session struct {
	srv   *Server
	conn  *websocket.Conn
	log   Logger
	id    string
	state sessionState

	startedAt     time.Time
	driver        duplex.DuplexDriver
	info          duplex.SessionInfo
	obs           *duplexobs.Obs
	obsID         string
	laneHeld      bool
	textOn        bool
	safety        duplexsafety.Policy
	safetyStopped bool

	seqIn    duplexwire.Sequencer
	outSeq   uint32
	textSeq  uint32
	statsSeq uint64

	drainReason string
	drained     int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	readCh  chan readResult
	eventCh chan duplex.Event
	killCh  chan string

	statsTicker *time.Ticker
	limitTimer  *time.Timer
	idleTimer   *time.Timer
	drainTimer  *time.Timer
}

// Logger is the subset of *log.Logger the session uses, so tests can make it
// quiet without a second logging abstraction.
type Logger interface {
	Printf(format string, v ...any)
}

func newSession(srv *Server, conn *websocket.Conn) *session {
	return &session{
		srv:       srv,
		conn:      conn,
		log:       srv.opts.Logger,
		id:        newSessionID(),
		state:     stateIdle,
		startedAt: time.Now(),
		readCh:    make(chan readResult, 8),
		eventCh:   make(chan duplex.Event, 8),
		killCh:    make(chan string, 1),
	}
}

func (s *session) kill(reason string) {
	select {
	case s.killCh <- reason:
	default:
	}
}

func (s *session) run() {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cleanup()
	s.wg.Add(1)
	go s.readLoop()

	var statsC <-chan time.Time
	var limitC <-chan time.Time
	var idleC <-chan time.Time
	var drainC <-chan time.Time
	var events <-chan duplex.Event

	for {
		select {
		case <-s.ctx.Done():
			return

		case <-s.killCh:
			if s.fail(&duplex.Error{Code: duplex.ErrCancelled, Message: "server kill switch", Fatal: true}) {
				return
			}

		case rr := <-s.readCh:
			if rr.err != nil {
				if errors.Is(rr.err, websocket.ErrFrameTooLarge) {
					if s.fail(&duplex.Error{Code: duplex.ErrBadFrame, Message: "message exceeds the 128 KiB binary limit", Fatal: true}) {
						return
					}
				}
				// Client close or transport error: no wire message can be delivered.
				return
			}
			if v := s.handleClientMessage(rr.msg); v {
				return
			}

		case ev := <-events:
			if v := s.handleEvent(ev); v {
				return
			}

		case <-statsC:
			s.send(duplexwire.NewStats(s.nextStatsSeq(), s.snapshotStats()))

		case <-limitC:
			if v := s.beginDrain("time_limit"); v {
				return
			}
			drainC = s.drainTimer.C

		case <-idleC:
			if s.fail(&duplex.Error{Code: duplex.ErrTimeout, Message: "no input audio before the idle timeout", Fatal: true}) {
				return
			}

		case <-drainC:
			if s.finish(s.drainReason) {
				return
			}
		}

		if s.state == stateRunning || s.state == stateDraining {
			events = s.eventCh
		}
		statsC = tickerChan(s.statsTicker)
		limitC = timerChan(s.limitTimer)
		idleC = timerChan(s.idleTimer)
		if s.drainTimer != nil {
			drainC = s.drainTimer.C
		}
	}
}

func (s *session) cleanup() {
	s.cancel()
	_ = s.conn.Close()
	s.wg.Wait()
	if s.driver != nil {
		_ = s.driver.Close()
	}
	if s.obs != nil {
		if summary, ok := s.srv.obs.Close(s.obsID, s.drainReason); ok {
			s.log.Printf("duplex: session summary %s", summary.LogLine())
		}
	}
	if s.laneHeld {
		s.laneHeld = false
		s.srv.releaseLane()
	}
	for _, t := range []*time.Timer{s.limitTimer, s.idleTimer, s.drainTimer} {
		if t != nil {
			t.Stop()
		}
	}
	if s.statsTicker != nil {
		s.statsTicker.Stop()
	}
	s.log.Printf("duplex: session closed id=%s tenant=%s state=%s", s.id, s.info.Tenant, s.state)
}

func (s *session) readLoop() {
	defer s.wg.Done()
	for {
		msg, err := readWireMessage(s.conn)
		select {
		case s.readCh <- readResult{msg: msg, err: err}:
		case <-s.ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (s *session) handleClientMessage(msg wireMessage) bool {
	if msg.Type == websocket.BinaryFrame {
		return s.handleAudio(msg.Data)
	}
	if msg.Type != websocket.TextFrame {
		return s.fail(&duplex.Error{Code: duplex.ErrBadRequest, Message: "unsupported WebSocket message type", Fatal: true})
	}

	m, err := duplexwire.DecodeClientMessage(msg.Data)
	if err != nil {
		return s.fail(asPlatformError(err))
	}

	switch m.Type {
	case duplexwire.TypeSessionStart:
		// Only the first message may be large (the reference voice); every later message keeps the frame-sized limit.
		s.conn.MaxPayloadBytes = duplexwire.MaxBinaryMessage
		return s.handleStart(m.Start)
	case duplexwire.TypePing:
		s.send(duplexwire.PongMessage{Type: duplexwire.TypePong, T: m.T})
		return false
	case duplexwire.TypeSessionClose, duplexwire.TypeSessionCancel:
		return s.handleClose(m)
	default:
		return s.fail(&duplex.Error{Code: duplex.ErrBadRequest, Message: "unknown message type", Fatal: true})
	}
}

func (s *session) handleAudio(payload []byte) bool {
	if s.state == stateDraining || s.state == stateClosed {
		return false
	}
	if s.state != stateRunning {
		return s.fail(&duplex.Error{Code: duplex.ErrBadFrame, Message: "audio before session.ready", Fatal: true})
	}

	seq, pcm, err := duplexwire.DecodeAudioFrame(payload)
	if err != nil {
		return s.fail(&duplex.Error{Code: duplex.ErrBadFrame, Message: err.Error(), Fatal: true})
	}
	if err := s.seqIn.Check(seq); err != nil {
		return s.fail(&duplex.Error{Code: duplex.ErrBadSequence, Message: err.Error(), Fatal: true})
	}
	frame, err := duplexwire.FrameFromPCM(pcm)
	if err != nil {
		return s.fail(&duplex.Error{Code: duplex.ErrBadFrame, Message: err.Error(), Fatal: true})
	}
	if err := s.driver.PushInput(s.ctx, frame, seq); err != nil {
		return s.fail(asPlatformError(err))
	}
	if s.idleTimer != nil {
		resetTimer(s.idleTimer, s.srv.opts.IdleTimeout)
	}
	return false
}

func (s *session) handleStart(req *duplexwire.StartRequest) bool {
	if s.state != stateIdle {
		return s.fail(&duplex.Error{Code: duplex.ErrBadRequest, Message: "session.start may only be sent once, before audio", Fatal: true})
	}
	s.state = stateStarting

	if s.srv.isKilled() {
		return s.fail(&duplex.Error{Code: duplex.ErrCancelled, Message: "service is disabled by the kill switch", Fatal: true})
	}
	if !s.srv.acquireLane() {
		return s.fail(&duplex.Error{Code: duplex.ErrSessionLimit, Message: "no free session lane", Fatal: true})
	}
	s.laneHeld = true

	if s.srv.opts.NewSafety != nil {
		s.safety = s.srv.opts.NewSafety()
	}

	tenant := req.Tenant
	if tenant == "" {
		tenant = s.srv.opts.DefaultTenant
	}
	maxMinutes := req.MaxMinutes
	if maxMinutes == 0 || maxMinutes > s.srv.opts.MaxMinutes {
		maxMinutes = s.srv.opts.MaxMinutes
	}

	var driver duplex.DuplexDriver
	if s.srv.opts.NewDriverFor != nil {
		d, derr := s.srv.opts.NewDriverFor(tenant)
		if derr != nil {
			return s.fail(startError(derr))
		}
		driver = d
	} else {
		driver = s.srv.opts.NewDriver()
	}
	if driver == nil {
		return s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "no duplex driver configured", Fatal: true})
	}
	s.driver = driver
	s.textOn = req.Text

	startCtx, cancelStart := context.WithTimeout(s.ctx, s.srv.opts.StartTimeout)
	info, err := driver.Start(startCtx, duplex.SessionConfig{
		Tenant:     tenant,
		SampleRate: req.SampleRate,
		MaxMinutes: maxMinutes,
		Text:       req.Text,
		Card:       req.Card,
		Reference:  req.Reference,
		Client:     req.Client,
	})
	cancelStart()
	if err != nil {
		return s.fail(startError(err))
	}

	if info.SessionID == "" {
		info.SessionID = s.id
	}
	if info.Tenant == "" {
		info.Tenant = tenant
	}
	if info.SampleRate == 0 {
		info.SampleRate = req.SampleRate
	}
	if info.FrameSamples == 0 {
		info.FrameSamples = duplex.FrameSamples
	}
	if info.MaxMinutes == 0 {
		info.MaxMinutes = maxMinutes
	}
	s.info = info
	s.obsID = info.SessionID
	s.obs = s.srv.obs.Start(info)
	s.obs.SetReference(req.Reference)
	s.state = stateRunning

	s.wg.Add(1)
	go s.driveEvents()

	s.statsTicker = time.NewTicker(s.srv.opts.StatsInterval)

	limit := s.srv.opts.MaxSessionDuration
	if limit <= 0 {
		limit = time.Duration(maxMinutes) * time.Minute
	}
	if limit > 0 {
		s.limitTimer = time.NewTimer(limit)
	}
	s.idleTimer = time.NewTimer(s.srv.opts.IdleTimeout)

	if err := s.send(duplexwire.NewReady(info)); err != nil {
		return s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "cannot send session.ready", Fatal: true})
	}
	s.log.Printf("duplex: session ready id=%s tenant=%s max_minutes=%d", s.id, info.Tenant, maxMinutes)
	return false
}

func (s *session) handleClose(m duplexwire.ClientMessage) bool {
	reason := m.Reason
	if m.Type == duplexwire.TypeSessionCancel {
		if reason == "" {
			reason = "cancelled"
		}
	} else if reason == "" {
		reason = "client_done"
	}

	switch s.state {
	case stateIdle:
		s.drainReason = reason
		return s.finish(reason)
	case stateRunning:
		return s.beginDrain(reason)
	case stateDraining:
		return false
	default:
		return false
	}
}

func (s *session) beginDrain(reason string) bool {
	if s.state == stateIdle {
		return s.finish(reason)
	}
	if s.state != stateRunning {
		return false
	}
	s.state = stateDraining
	s.drainReason = reason
	stopTimer(&s.limitTimer)
	stopTimer(&s.idleTimer)
	s.idleTimer = nil
	s.limitTimer = nil
	if s.driver != nil {
		_ = s.driver.Cancel(reason)
	}
	s.drainTimer = time.NewTimer(s.srv.opts.DrainTimeout)
	s.log.Printf("duplex: session draining id=%s reason=%s", s.id, reason)
	return false
}

func (s *session) handleEvent(ev duplex.Event) bool {
	switch ev.Kind {
	case duplex.EventAudio:
		if s.state != stateRunning && s.state != stateDraining {
			return false
		}
		if s.state == stateDraining {
			if s.drained >= 4 {
				return false
			}
			s.drained++
		}
		pcm, err := duplexwire.PCMFromFrame(ev.Audio)
		if err != nil {
			return s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: "driver returned an invalid audio frame", Fatal: true})
		}
		seq := s.outSeq
		s.outSeq++
		payload, err := duplexwire.EncodeAudioFrame(seq, pcm)
		if err != nil {
			return s.fail(&duplex.Error{Code: duplex.ErrInternal, Message: err.Error(), Fatal: true})
		}
		if err := s.sendBinary(payload); err != nil {
			return true
		}
	case duplex.EventText:
		if ev.Text == "" {
			return false
		}
		if s.safety != nil && !s.safetyStopped {
			v := s.safety.Observe(ev.Text)
			if v.Stop {
				s.safetyStopped = true
				code := v.Code
				if code == "" {
					code = "safety"
				}
				if s.obs != nil {
					s.obs.SafetyStop(code)
				}
				s.log.Printf("duplex: session safety stop id=%s code=%s", s.id, code)
				if s.state == stateRunning {
					return s.beginDrain("safety_stop")
				}
				return false
			}
		}
		if s.safetyStopped || !s.textOn {
			return false
		}
		if s.state != stateRunning && s.state != stateDraining {
			return false
		}
		s.send(duplexwire.NewTextDelta(s.textSeq, ev.Text))
		s.textSeq++
	case duplex.EventDone:
		reason := ev.Reason
		if s.state == stateDraining && s.drainReason != "" {
			reason = s.drainReason
		}
		if reason == "" {
			reason = "done"
		}
		s.drainReason = reason
		return s.finish(reason)
	case duplex.EventError:
		e := ev.Error
		if e == nil {
			e = &duplex.Error{Code: duplex.ErrInternal, Message: "driver error without detail", Fatal: true}
		}
		return s.fail(e)
	case duplex.EventStats:
		return false
	default:
		return false
	}
	return false
}

func (s *session) driveEvents() {
	defer s.wg.Done()
	for {
		ev, err := s.driver.NextEvent(s.ctx, 100*time.Millisecond)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue // the driver's documented "nothing yet" timeout
			}
			select {
			case s.eventCh <- duplex.Event{Kind: duplex.EventError, Error: &duplex.Error{Code: duplex.ErrInternal, Message: err.Error(), Fatal: true}}:
			case <-s.ctx.Done():
			}
			return
		}
		if ev.Kind == 0 {
			continue
		}
		if s.obs != nil {
			s.obs.Observe(ev)
		}
		select {
		case s.eventCh <- ev:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *session) finish(reason string) bool {
	if s.state == stateClosed {
		return true
	}
	s.state = stateClosed
	st := s.snapshotStats()
	s.send(duplexwire.NewStats(s.nextStatsSeq(), st))
	s.send(duplexwire.NewDone(reason, s.nextStatsSeq(), st))
	return true
}

func (s *session) fail(e *duplex.Error) bool {
	if s.state == stateClosed || s.state == stateError {
		return true
	}
	if e.Code == "" {
		e.Code = duplex.ErrInternal
	}
	e.Fatal = true
	s.state = stateError
	s.log.Printf("duplex: session error id=%s code=%s", s.id, e.Code)
	s.send(duplexwire.NewError(e))
	return true
}

func (s *session) snapshotStats() duplex.Stats {
	var st duplex.Stats
	if s.driver != nil {
		st = s.driver.Stats()
	}
	if st.SessionID == "" {
		st.SessionID = s.id
	}
	if st.Uptime <= 0 {
		st.Uptime = time.Since(s.startedAt)
	}
	return st
}

func (s *session) nextStatsSeq() uint64 {
	n := s.statsSeq
	s.statsSeq++
	return n
}

func (s *session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Printf("duplex: marshal %T: %v", v, err)
		return err
	}
	return s.sendBinaryText(string(b))
}

func (s *session) sendBinaryText(text string) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.srv.opts.WriteTimeout))
	return websocket.Message.Send(s.conn, text)
}

func (s *session) sendBinary(b []byte) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.srv.opts.WriteTimeout))
	return websocket.Message.Send(s.conn, b)
}

func asPlatformError(err error) *duplex.Error {
	var de *duplex.Error
	if errors.As(err, &de) {
		if de.Code == "" {
			de.Code = duplex.ErrInternal
		}
		return de
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &duplex.Error{Code: duplex.ErrTimeout, Message: err.Error(), Fatal: true}
	}
	if errors.Is(err, io.EOF) {
		return &duplex.Error{Code: duplex.ErrInternal, Message: "driver closed", Fatal: true}
	}
	return &duplex.Error{Code: duplex.ErrInternal, Message: err.Error(), Fatal: true}
}

func startError(err error) *duplex.Error {
	de := asPlatformError(err)
	if de.Code == duplex.ErrInternal || de.Code == duplex.ErrTimeout {
		de.Code = duplex.ErrTenantUnavailable
	}
	de.Fatal = true
	return de
}

func tickerChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func timerChan(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func stopTimer(t **time.Timer) {
	if t == nil || *t == nil {
		return
	}
	(*t).Stop()
	*t = nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

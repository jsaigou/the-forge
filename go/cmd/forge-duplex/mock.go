package main

import (
	"context"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// mockDriver is a transport-side echo driver for local runs and tests. It is
// NOT the WS1 driver and NOT the WS4 loopback tenant: it implements just enough
// of duplex.DuplexDriver to exercise the socket, codec and state machine.
type mockDriver struct {
	mu        sync.Mutex
	started   bool
	closed    bool
	doneSent  bool
	start     time.Time
	in        chan duplex.Frame
	cancelCh  chan string
	inFrames  uint64
	outFrames uint64
}

func newMockDriver() duplex.DuplexDriver {
	return &mockDriver{
		in:       make(chan duplex.Frame, 8),
		cancelCh: make(chan string, 1),
	}
}

func (m *mockDriver) Start(_ context.Context, cfg duplex.SessionConfig) (duplex.SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return duplex.SessionInfo{}, &duplex.Error{Code: duplex.ErrInternal, Message: "mock driver already started", Fatal: true}
	}
	m.started = true
	m.start = time.Now()
	maxMinutes := cfg.MaxMinutes
	if maxMinutes == 0 {
		maxMinutes = defaultMaxMinutes
	}
	return duplex.SessionInfo{
		Tenant:       cfg.Tenant,
		SampleRate:   duplex.SampleRate,
		FrameSamples: duplex.FrameSamples,
		FrameMillis:  float64(duplex.FrameDuration.Microseconds()) / 1000,
		MaxMinutes:   maxMinutes,
		Capabilities: duplex.Capabilities{Text: true, InputRates: []int{duplex.SampleRate}},
	}, nil
}

func (m *mockDriver) PushInput(_ context.Context, in duplex.Frame, _ uint32) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return &duplex.Error{Code: duplex.ErrCancelled, Message: "mock driver is closed", Fatal: true}
	}
	f := make(duplex.Frame, len(in))
	copy(f, in)
	select {
	case m.in <- f:
		m.mu.Lock()
		m.inFrames++
		m.mu.Unlock()
		return nil
	default:
		return &duplex.Error{Code: duplex.ErrQueueFull, Message: "input queue is full", Fatal: true}
	}
}

func (m *mockDriver) NextEvent(ctx context.Context, timeout time.Duration) (duplex.Event, error) {
	m.mu.Lock()
	closed := m.closed
	doneSent := m.doneSent
	m.mu.Unlock()

	select {
	case reason := <-m.cancelCh:
		m.mu.Lock()
		already := m.doneSent
		m.doneSent = true
		m.mu.Unlock()
		if already {
			return duplex.Event{}, &duplex.Error{Code: duplex.ErrCancelled, Message: "mock driver cancelled", Fatal: true}
		}
		if reason == "" {
			reason = "cancelled"
		}
		return duplex.Event{Kind: duplex.EventDone, Reason: reason}, nil
	default:
	}

	if closed && doneSent {
		return duplex.Event{}, &duplex.Error{Code: duplex.ErrCancelled, Message: "mock driver closed", Fatal: true}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return duplex.Event{}, ctx.Err()
	case <-m.cancelCh:
		m.mu.Lock()
		already := m.doneSent
		m.doneSent = true
		m.mu.Unlock()
		if already {
			return duplex.Event{}, &duplex.Error{Code: duplex.ErrCancelled, Message: "mock driver cancelled", Fatal: true}
		}
		return duplex.Event{Kind: duplex.EventDone, Reason: "cancelled"}, nil
	case f := <-m.in:
		m.mu.Lock()
		m.outFrames++
		m.mu.Unlock()
		return duplex.Event{Kind: duplex.EventAudio, Audio: f}, nil
	case <-timer.C:
		return duplex.Event{}, nil
	}
}

func (m *mockDriver) Cancel(reason string) error {
	select {
	case m.cancelCh <- reason:
	default:
	}
	return nil
}

func (m *mockDriver) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	select {
	case m.cancelCh <- "closed":
	default:
	}
	return nil
}

func (m *mockDriver) Stats() duplex.Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := duplex.Stats{
		InFrames:  m.inFrames,
		OutFrames: m.outFrames,
		QueueIn:   len(m.in),
		QueueOut:  0,
	}
	if !m.start.IsZero() {
		st.Uptime = time.Since(m.start)
	}
	return st
}

// SPDX-License-Identifier: Apache-2.0

// Package hfdownload is the resilient HF model-acquisition engine: a
// persistent job queue (migration 0070) fronting the download worker,
// pause/resume/cancel, asynchronous asset enrichment, and atomic
// catalog auto-registration. Replaces smith's old single-shot fetch_model
// procedure (go/internal/smith/procedures/fetch_model.go — deleted
// alongside this package landing), which had no progress reporting, no
// pause, and no queue: it was one blocking procedure-engine step wrapping
// a bare io.Copy.
//
// smith gets read-only search/preflight/status tools plus a
// propose-only download_start tool (go/internal/smith/tools.go) —
// tools.go's structural guarantee is that a Tool.Run never causes a
// write, so download_start enqueues a job in "pending_approval" and
// returns; an operator approves it through the ordinary UI, same as
// every other smith-proposed action.
package hfdownload

import (
	"context"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/store"
)

// Deps wires the engine to the rest of the daemon. Every field is used by
// at least one of preflight/worker/registrar; see each file's doc comment
// for which.
type Deps struct {
	Store   *store.DB
	HF      *hf.Client
	Cfg     func() *config.Config
	// Source is the collector snapshot source, used by Preflight for real
	// disk/GTT figures. nil ⇒ Preflight degrades those two checks to
	// "unavailable" rather than blocking on a stale/absent reading.
	Source collector.Source
	// Publish is the SSE bus. nil ⇒ progress is still recorded to the
	// store (pollable via GET) but nothing streams live.
	Publish bus.Publisher
	Now     func() time.Time
	Logf    func(format string, args ...any)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d Deps) publish(name string, data any) {
	if d.Publish != nil {
		d.Publish.Publish(name, data)
	}
}

// Service is the engine. One process-wide instance, constructed at daemon
// startup and handed to httpapi's route handlers and smith's tool env.
type Service struct {
	d Deps

	mu     sync.Mutex
	active map[int64]context.CancelFunc // jobID -> cancel for its running worker goroutine
	// intent distinguishes "the context died because Pause was called"
	// from "...because Cancel was called" — set right before cancelActive
	// fires the context, read exactly once by runWorker's cleanup path.
	// Per-Service (not a package global) so tests can run multiple
	// isolated Services concurrently.
	intent map[int64]string
}

func New(d Deps) *Service {
	return &Service{d: d, active: map[int64]context.CancelFunc{}, intent: map[int64]string{}}
}

func (s *Service) setIntent(jobID int64, v string) {
	s.mu.Lock()
	s.intent[jobID] = v
	s.mu.Unlock()
}

func (s *Service) takeIntentOrDefault(jobID int64, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.intent[jobID]
	if ok {
		delete(s.intent, jobID)
		return v
	}
	return def
}

// isRunning reports whether jobID currently has a live worker goroutine —
// the in-memory complement to the store's "running" state, needed because
// Pause must cancel a real goroutine, not just flip a database row.
func (s *Service) isRunning(jobID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[jobID]
	return ok
}

func (s *Service) setActive(jobID int64, cancel context.CancelFunc) {
	s.mu.Lock()
	s.active[jobID] = cancel
	s.mu.Unlock()
}

func (s *Service) clearActive(jobID int64) {
	s.mu.Lock()
	delete(s.active, jobID)
	s.mu.Unlock()
}

func (s *Service) cancelActive(jobID int64) bool {
	s.mu.Lock()
	cancel, ok := s.active[jobID]
	if ok {
		delete(s.active, jobID)
	}
	s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// goSafe launches fn in a panic-recovering goroutine — mirrors
// httpapi/gosafe.go's goSafe exactly; duplicated rather than imported
// because httpapi imports this package, not the other way around.
func goSafe(logf func(string, ...any), name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("hfdownload: goroutine %q panic: %v", name, r)
			}
		}()
		fn()
	}()
}

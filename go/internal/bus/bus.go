// SPDX-License-Identifier: Apache-2.0

// Package bus is the in-process SSE event bus shared by all components
// (Contract 2 addition to the docs/v5-plan.md package list — engine, sched,
// and httpapi all need one shared event definition; see
// docs/v5-go-contracts.md).
//
// Event names are Contract 1: the exact names V4 pushes and the PWA
// subscribes to (status_update, switch_started, switch_complete,
// switch_failed, load_started, load_complete, load_failed, unload_complete,
// config_updated, registry:refreshed, tts:job_update, profile:started,
// profile:progress, profile:done, profile:failed, download_*). Never
// invent new names without a Contract 1 amendment.
//
// Contract 1 amendment (Sprint K, 2026-08-05): slot:activity added — fires
// {slot, active} on a collector-observed busy↔idle edge for a loaded slot
// (internal/collector/run.go's reportSlotActivity), namespaced per this
// repo's SSE convention (never a bare event name). Not a replacement for
// status_update's slot_activity field — that field is the source of truth
// on connect/reconnect; this event is the low-latency push between polls.
//
// Contract 1 amendment (HF model-acquisition track): download:progress,
// download:state_changed, download:done, download:failed added
// (go/internal/hfdownload/events.go) — the colon-namespaced form, matching
// every other Sprint-era addition (profile:*, slot:activity, smith:*)
// rather than the legacy underscore "download_*" this doc comment already
// listed from V4 history: nothing in the v0.5 Go rewrite ever implemented
// that name, so there's no live consumer to stay compatible with.
package bus

import (
	"context"
	"sync"
)

// Event is one SSE event: Name goes into the `event:` field, Data is
// JSON-encoded into `data:`.
type Event struct {
	Name string
	Data any
}

// Publisher is the write side, injected into engine/sched/httpapi handlers.
type Publisher interface {
	Publish(name string, data any)
}

// Subscriber is the read side, consumed by the SSE endpoint.
type Subscriber interface {
	// Subscribe returns a channel of events delivered until ctx is done.
	// Slow subscribers get events dropped (bounded buffer), never block
	// publishers.
	Subscribe(ctx context.Context) <-chan Event
}

// Bus is the working in-memory implementation (complete in Phase 1 — it is
// small and every track needs it for tests).
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

var (
	_ Publisher  = (*Bus)(nil)
	_ Subscriber = (*Bus)(nil)
)

// New returns an empty bus.
func New() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// subBuffer bounds each subscriber's queue, mirroring V4's per-client
// queue.Queue(maxsize=20).
const subBuffer = 20

// Publish implements Publisher. Events to full subscriber buffers are
// dropped for that subscriber only.
func (b *Bus) Publish(name string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default: // slow client — drop
		}
	}
}

// Subscribe implements Subscriber.
func (b *Bus) Subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, subBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}()
	return ch
}

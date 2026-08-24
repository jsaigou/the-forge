// SPDX-License-Identifier: Apache-2.0

package httpapi

// sse.go — Contract 1 §4 SSE endpoint, porting the V4 _push_event +
// /api/v1/events stream (forge/app.py).
//
// Frozen semantics (see CLAUDE.local.md "Hard requirements"):
//   - Content-Type: text/event-stream, Cache-Control: no-cache,
//     X-Accel-Buffering: no.
//   - On connect: immediately send one status_update event with the full
//     Status payload.
//   - Keepalive: comment line ": keepalive" after 25s with no events.
//   - Heartbeat: a status_update with full Status every 30s regardless of
//     activity.
//   - Per-client buffer is bounded (V4: 20); slow clients lose events
//     rather than blocking the bus. (Handled by internal/bus — this file
//     only subscribes.)
//   - Event names use the underscore form (status_update, switch_started,
//     switch_complete, switch_failed, load_started, load_complete,
//     load_failed, unload_complete, config_updated, registry:refreshed,
//     tts:job_update). V4's four colon-form bug sites (tts start/stop,
//     service-mode start/stop) are emitted as status_update here.

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// SSE timing constants — frozen by Contract 1 §4.
const (
	sseKeepaliveInterval = 25 * time.Second
	sseHeartbeatInterval = 30 * time.Second
)

// handleSSE streams the event bus to one client. The handler blocks until
// the client disconnects (closes the EventSource) or the server shuts down.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send one initial status_update with the full Status payload. The
	// PWA's useLiveEvents() writes this straight into the Query cache
	// (web/src/lib/sse.ts) — without it the dashboard would render empty
	// until the first 15s poll fired.
	if payload, err := s.encodeSSEStatus(); err == nil {
		_, _ = fmt.Fprint(w, payload)
		flusher.Flush()
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	events := s.deps.Events.Subscribe(ctx)
	if events == nil {
		// No bus wired — close the stream. (cmd/forge always wires a
		// bus; this branch is the test-time nil case.)
		return
	}

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// Full Status every 30s regardless of activity.
			if payload, err := s.encodeSSEStatus(); err == nil {
				_, _ = fmt.Fprint(w, payload)
				flusher.Flush()
			}
		case <-keepalive.C:
			// 25s with no events → comment line keeps the connection
			// alive through proxies without delivering an event.
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, err := encodeSSEEvent(ev.Name, ev.Data)
			if err != nil {
				continue
			}
			_, _ = w.Write(payload)
			flusher.Flush()
		}
	}
}

// encodeSSEStatus publishes one status_update event with the full Status
// payload. Returns the wire bytes ready to write to the response.
func (s *Server) encodeSSEStatus() (string, error) {
	status := s.buildStatusResponse()
	data, err := encodeSSEEvent("status_update", status)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// startHeartbeat launches the periodic status_update broadcast on the bus,
// mirroring V4's _heartbeat_thread. Every 30s every connected client gets
// a fresh full-Status payload even when nothing else has happened.
//
// Run once per Server (heartbeatOnce guards against double-start in tests
// that re-invoke Handler()). Stops when Close() is called.
func (s *Server) startHeartbeat() {
	s.heartbeatOnce.Do(func() {
		goSafe("heartbeat", func() {
			t := time.NewTicker(sseHeartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-s.heartbeatStop:
					return
				case <-t.C:
					if s.deps.Publish == nil {
						continue
					}
					s.deps.Publish.Publish("status_update", s.buildStatusResponse())
				}
			}
		})
	})
}

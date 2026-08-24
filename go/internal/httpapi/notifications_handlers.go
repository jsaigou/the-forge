// SPDX-License-Identifier: Apache-2.0

package httpapi

// notifications_handlers.go — Dashboard notifications (product/QA sprint,
// 2026-07-29). Previously the collector's Alerts (hang, GTT-high) rode
// inside the status payload as an unconsumed field (web/src/lib/types.ts's
// `alerts?` was declared but never read anywhere in the FE) — no
// persistence, no acknowledge/dismiss, and no unit-crash/OOM detection at
// all. This file adds a background sync (startNotificationSync) that reads
// the collector snapshot's Alerts each tick, upserts them into the
// store.Notifications table (dedup + occurrence counting, see
// store/notifications.go), and publishes an SSE event the first time an
// alert becomes active. GET/ack/dismiss/ack-all read/mutate that table.
//
// Deliberately NOT a replacement for status.alerts (kept for compatibility)
// — this is an additive, richer, persisted view of the same underlying
// collector signal plus the new unit-scoped codes (UNIT_OOM/UNIT_CRASH/
// UNIT_RESTARTED, from collector.unitAlerts — systemd Result/NRestarts, no
// dmesg/kernel access needed, see docs).

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/store"
)

// EventNotificationNew is the SSE event published the first time an alert
// becomes active (not on every repeat while it persists — a level-triggered
// alert like a sustained hang would otherwise fire every sync tick).
const EventNotificationNew = "notification:new"

const notificationSyncInterval = 5 * time.Second

// notificationSeverity classifies each known alert code. Unknown codes
// (forward-compat with a future collector alert) default to "warn" rather
// than being dropped.
func notificationSeverity(code string) string {
	switch code {
	case "INFERENCE_HANG", "UNIT_OOM", "UNIT_CRASH", "SLOT_ERROR_STORM":
		return "crit"
	case "GTT_HIGH", "UNIT_RESTARTED":
		return "warn"
	default:
		return "warn"
	}
}

// notificationSubject and notificationDedupeKey mirror each other: a
// port-scoped alert (hang, GTT) keys on its port; a unit-scoped alert
// (OOM/crash/restart) keys on its unit name. Exactly one of Alert.Port/Unit
// is set per the collector's alert producers.
func notificationSubject(port int, unit string) string {
	if unit != "" {
		return unit
	}
	if port != 0 {
		return fmt.Sprintf("%d", port)
	}
	return ""
}

func notificationDedupeKey(code string, port int, unit string) string {
	return code + ":" + notificationSubject(port, unit)
}

// startNotificationSync starts the background alert→notification sync,
// once per Server. No-op when either the Notifications or Snapshots
// dependency isn't wired (Phase 4 stub environment, and most unit tests).
func (s *Server) startNotificationSync() {
	if s.deps.Notifications == nil || s.deps.Snapshots == nil {
		return
	}
	s.notifySyncOnce.Do(func() {
		ticker := time.NewTicker(notificationSyncInterval)
		goSafe("notify_ticker", func() {
			<-s.bgCtx.Done()
			ticker.Stop()
		})
		goSafe("notify_sync", func() {
			for {
				select {
				case <-s.bgCtx.Done():
					return
				case <-ticker.C:
					s.syncNotificationsOnce(s.bgCtx)
				}
			}
		})
	})
}

// syncNotificationsOnce upserts every currently-active alert and publishes
// EventNotificationNew for ones that weren't active as of the previous
// tick. s.notifyActive (in-memory, this process's lifetime only) is what
// distinguishes "just started" from "still ongoing" — the store itself
// doesn't track that distinction (occurrences just keeps counting).
func (s *Server) syncNotificationsOnce(ctx context.Context) {
	snap := s.snapshot()
	if snap == nil {
		return
	}
	now := time.Now()
	current := make(map[string]bool, len(snap.Alerts))

	s.notifyMu.Lock()
	prevActive := s.notifyActive
	s.notifyMu.Unlock()

	for _, a := range snap.Alerts {
		key := notificationDedupeKey(a.Code, a.Port, a.Unit)
		current[key] = true
		wasActive := prevActive[key]

		id, err := s.deps.Notifications.Upsert(ctx, a.Code, notificationSeverity(a.Code),
			notificationSubject(a.Port, a.Unit), a.Msg, key, now)
		if err != nil {
			continue // a missed tick must not crash the daemon
		}
		if !wasActive && s.deps.Publish != nil {
			s.deps.Publish.Publish(EventNotificationNew, map[string]any{
				"id":      id,
				"code":    a.Code,
				"subject": notificationSubject(a.Port, a.Unit),
				"message": a.Msg,
			})
		}
		// Wire the real "inference_hang" usage event (product/QA sprint):
		// registry.Reliability.InferenceHangs / usageModelRow.InferenceHangs
		// read this kind from usage_events, but until now nothing ever wrote
		// it. One event per hang *episode* (the !wasActive transition), not
		// once per 5s sync tick — a hang that stays active for an hour is
		// one hang, not 720 of them.
		if !wasActive && a.Code == "INFERENCE_HANG" && s.deps.Usage != nil {
			if mode, slot, ok := s.modeForPort(snap, a.Port); ok {
				s.recordUsageEvent(ctx, "inference_hang", mode, slot, a.Msg)
			}
		}
	}

	s.notifyMu.Lock()
	s.notifyActive = current
	s.notifyMu.Unlock()
}

// modeForPort resolves a port-scoped alert to the mode/slot currently
// loaded there, via the same reconciled Slots map the dashboard reads.
// false when the port isn't a recognized slot (shouldn't happen for a
// hang alert, which is only ever raised for a port the collector scraped
// as a loaded slot — but a slot can empty between the alert firing and
// this sync tick reading it, so this stays a lookup, not an assumption).
func (s *Server) modeForPort(snap *collector.Snapshot, port int) (mode, slot string, ok bool) {
	for name, st := range snap.Slots {
		if st.Port == port && st.Mode != "" {
			return st.Mode, name, true
		}
	}
	return "", "", false
}

// recordUsageEvent persists a usage_events row. Best-effort — usage
// tracking must never block or crash the notification sync loop (same
// discipline as engine.Manager.recordUsageEvent for load/unload events).
func (s *Server) recordUsageEvent(ctx context.Context, kind, model, slot, detail string) {
	_ = s.deps.Usage.Record(ctx, store.UsageEvent{
		TS: time.Now(), Kind: kind, Model: model, Slot: slot, Detail: detail,
	})
}

// notificationJSON mirrors one store.Notification on the wire.
type notificationJSON struct {
	ID             int64    `json:"id"`
	Code           string   `json:"code"`
	Severity       string   `json:"severity"`
	Subject        string   `json:"subject,omitempty"`
	Message        string   `json:"message"`
	FirstSeen      int64    `json:"first_seen"`
	LastSeen       int64    `json:"last_seen"`
	Occurrences    int      `json:"occurrences"`
	AcknowledgedAt *float64 `json:"acknowledged_at,omitempty"`
	DismissedAt    *float64 `json:"dismissed_at,omitempty"`
}

type notificationsResponse struct {
	Notifications []notificationJSON `json:"notifications"`
}

// handleNotificationsList returns active notifications by default
// (?include_dismissed=1 to also see dismissed ones — the Settings/history
// view, not the Dashboard panel's default).
func (s *Server) handleNotificationsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeJSON(w, http.StatusOK, notificationsResponse{Notifications: []notificationJSON{}})
		return
	}
	includeDismissed := r.URL.Query().Get("include_dismissed") == "1"
	list, err := s.deps.Notifications.List(r.Context(), includeDismissed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list notifications")
		return
	}
	resp := notificationsResponse{Notifications: make([]notificationJSON, len(list))}
	for i, n := range list {
		resp.Notifications[i] = notificationJSON{
			ID: n.ID, Code: n.Code, Severity: n.Severity, Subject: n.Subject,
			Message: n.Message, FirstSeen: n.FirstSeen.Unix(), LastSeen: n.LastSeen.Unix(),
			Occurrences:    n.Occurrences,
			AcknowledgedAt: unixPtrOrNil(n.AcknowledgedAt),
			DismissedAt:    unixPtrOrNil(n.DismissedAt),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func unixPtrOrNil(t *time.Time) *float64 {
	if t == nil {
		return nil
	}
	v := float64(t.Unix())
	return &v
}

// handleNotificationAck acknowledges one notification (operator+).
func (s *Server) handleNotificationAck(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid notification id")
		return
	}
	if err := s.deps.Notifications.Acknowledge(r.Context(), id, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to acknowledge notification")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleNotificationDismiss dismisses one notification (operator+).
func (s *Server) handleNotificationDismiss(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid notification id")
		return
	}
	if err := s.deps.Notifications.Dismiss(r.Context(), id, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to dismiss notification")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleNotificationAckAll acknowledges every currently-active notification
// (operator+).
func (s *Server) handleNotificationAckAll(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not wired")
		return
	}
	if err := s.deps.Notifications.AcknowledgeAll(r.Context(), time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to acknowledge notifications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// notifySyncState is embedded in Server (see httpapi.go) to track which
// dedupe keys were active as of the previous sync tick — process-lifetime
// only, not persisted. Kept as a small named type so Server's field block
// stays a one-line addition (notifySyncState) rather than three loose
// fields.
type notifySyncState struct {
	notifyMu       sync.Mutex
	notifyActive   map[string]bool
	notifySyncOnce sync.Once
}

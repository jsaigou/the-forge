// SPDX-License-Identifier: Apache-2.0

package httpapi

// Operations (ADR-0020 finding F4, docs/api-contract-implementation-plan.md phase 2).
//
// POST /api/v1/load and /unload have always run in the background and returned at once with in_progress:true.
// What was missing is the other half: nothing a poller could read said whether that background work then
// succeeded, failed, or why. The result went only to SSE subscribers, and the slot's loading marker was simply
// cleared. An operation is the pollable record of one such request; slot_states in /status is the per-slot view
// of the same facts. Both live in memory: they describe what this daemon did since it started, so an operation id
// does not survive a restart (it is nonce-prefixed so a stale id can never alias a different operation).

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	operationRingSize = 200
	operationMaxWaitS = 300
)

type operation struct {
	id         string
	kind       string // "load" | "unload"
	slot       string
	config     string
	state      string // "running" | "succeeded" | "failed"
	message    string
	detail     string
	nCtx       int
	startedAt  time.Time
	finishedAt time.Time
	done       chan struct{}
}

// operationJSON is the wire shape of one operation.
type operationJSON struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Slot       string   `json:"slot"`
	Config     string   `json:"config,omitempty"`
	State      string   `json:"state"`
	Message    string   `json:"message,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	NCtx       *int     `json:"n_ctx,omitempty"`
	StartedAt  float64  `json:"started_at"`
	FinishedAt *float64 `json:"finished_at,omitempty"`
}

type operationRegistry struct {
	mu    sync.Mutex
	nonce string
	seq   int64
	byID  map[string]*operation
	order []string // oldest first
}

func newOperationRegistry() *operationRegistry {
	return &operationRegistry{nonce: strconv.FormatInt(time.Now().Unix(), 36), byID: map[string]*operation{}}
}

// start records a new running operation. It takes only the registry's own lock, so it is safe to call while
// holding Server.mu (the lock order is Server.mu then registry.mu, never the reverse).
func (r *operationRegistry) start(kind, slot, config string, now time.Time) *operation {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	op := &operation{
		id: fmt.Sprintf("op-%s-%d", r.nonce, r.seq), kind: kind, slot: slot, config: config,
		state: "running", startedAt: now, done: make(chan struct{}),
	}
	r.byID[op.id] = op
	r.order = append(r.order, op.id)
	// Bound memory: drop the oldest finished operations; a still-running one is never evicted.
	for len(r.order) > operationRingSize {
		evicted := false
		for i, id := range r.order {
			if r.byID[id].state != "running" {
				delete(r.byID, id)
				r.order = append(r.order[:i], r.order[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			break
		}
	}
	return op
}

// finish records the outcome and wakes anything waiting on the operation. A nil op or a second finish is a no-op.
func (r *operationRegistry) finish(op *operation, ok bool, message, detail string, nCtx int, now time.Time) {
	if op == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if op.state != "running" {
		return
	}
	op.state = "succeeded"
	if !ok {
		op.state = "failed"
	}
	op.message, op.detail, op.nCtx, op.finishedAt = message, detail, nCtx, now
	close(op.done)
}

func (r *operationRegistry) get(id string) (*operation, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.byID[id]
	return op, ok
}

func (r *operationRegistry) view(op *operation) operationJSON {
	if op == nil {
		return operationJSON{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.viewLocked(op)
}

func (r *operationRegistry) viewLocked(op *operation) operationJSON {
	v := operationJSON{
		ID: op.id, Kind: op.kind, Slot: op.slot, Config: op.config, State: op.state,
		Message: op.message, Detail: op.detail, StartedAt: unixSeconds(op.startedAt),
	}
	if op.nCtx > 0 {
		n := op.nCtx
		v.NCtx = &n
	}
	if !op.finishedAt.IsZero() {
		f := unixSeconds(op.finishedAt)
		v.FinishedAt = &f
	}
	return v
}

// list returns the newest operations first, optionally for one slot.
func (r *operationRegistry) list(slot string, limit int) []operationJSON {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []operationJSON{}
	for i := len(r.order) - 1; i >= 0 && len(out) < limit; i-- {
		op := r.byID[r.order[i]]
		if slot == "" || op.slot == slot {
			out = append(out, r.viewLocked(op))
		}
	}
	return out
}

// wait blocks until the operation finishes, the timeout passes, or ctx ends, whichever is first. It reports
// nothing: callers read the operation's current state afterwards.
func (r *operationRegistry) wait(ctx context.Context, op *operation, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-op.done:
	case <-t.C:
	case <-ctx.Done():
	}
}

func opID(op *operation) string {
	if op == nil {
		return ""
	}
	return op.id
}

// loadRecord is the last load result for a slot, kept so /status can report a failure after the loading marker
// clears. It is replaced by the next load, and cleared by an unload of that slot.
type loadRecord struct {
	mode    string
	state   string // "ready" | "failed"
	message string
	detail  string
	nCtx    int
	at      time.Time
	opID    string
}

// slotOpStateJSON is the additive per-slot state in /status: whether a load or unload is running, and if the
// last load failed, why. Existing slot_loading / slot_unloading are unchanged.
type slotOpStateJSON struct {
	State       string   `json:"state"` // empty | loading | ready | failed | unloading
	Mode        *string  `json:"mode"`
	Message     string   `json:"message,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	NCtx        *int     `json:"n_ctx,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	FinishedAt  *float64 `json:"finished_at,omitempty"`
}

// readyGrace is how long after a successful load the engine's own record keeps a slot "ready" while the collector
// has not yet reported its mode (it polls every few seconds). Without it a poller sees "empty" right after success.
const readyGrace = 15 * time.Second

// slotOpStatesLocked derives every slot's operation state. Caller holds s.mu. The precedence is deliberate: a
// running unload or load is what the slot is doing now; a failed last load is shown only while the slot is
// still empty (an engine load frees the target first, so a failure leaves it empty, and a slot that is serving
// something is simply ready); otherwise loaded means ready.
func (s *Server) slotOpStatesLocked(resp statusResponse) map[string]slotOpStateJSON {
	names := map[string]struct{}{}
	for n := range resp.Slots {
		names[n] = struct{}{}
	}
	for n := range s.slotLoading {
		names[n] = struct{}{}
	}
	for n := range s.slotUnloading {
		names[n] = struct{}{}
	}
	for n := range s.slotLastLoad {
		names[n] = struct{}{}
	}
	out := make(map[string]slotOpStateJSON, len(names))
	for name := range names {
		cur := ""
		if m := resp.Slots[name]; m != nil {
			cur = *m
		}
		rec, hasRec := s.slotLastLoad[name]
		st := slotOpStateJSON{State: "empty"}
		switch {
		case s.slotUnloading[name].inProgress:
			st.State, st.Mode = "unloading", ptrString(s.slotUnloading[name].mode)
		case s.slotLoading[name].inProgress:
			st.State, st.Mode = "loading", ptrString(s.slotLoading[name].mode)
		case hasRec && rec.state == "failed" && cur == "":
			f := unixSeconds(rec.at)
			st.State, st.Mode, st.Message, st.Detail, st.OperationID, st.FinishedAt = "failed", ptrString(rec.mode), rec.message, rec.detail, rec.opID, &f
		case cur == "" && hasRec && rec.state == "ready" && time.Since(rec.at) < readyGrace:
			st.State, st.Mode, st.OperationID = "ready", ptrString(rec.mode), rec.opID
			if rec.nCtx > 0 {
				n := rec.nCtx
				st.NCtx = &n
			}
			f := unixSeconds(rec.at)
			st.FinishedAt = &f
		case cur != "":
			m := cur
			st.State, st.Mode = "ready", &m
			if hasRec && rec.state == "ready" && rec.mode == cur {
				st.OperationID = rec.opID
				if rec.nCtx > 0 {
					n := rec.nCtx
					st.NCtx = &n
				}
				f := unixSeconds(rec.at)
				st.FinishedAt = &f
			}
		}
		out[name] = st
	}
	return out
}

// GET /api/v1/operations/{id}[?wait=SECONDS] returns one operation; with wait it blocks (up to 300s) until the
// operation finishes, so a client can wait without polling.
func (s *Server) handleOperationGet(w http.ResponseWriter, r *http.Request) {
	op, ok := s.ops.get(r.PathValue("id"))
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "operation"},
			"operation not found (operations are kept in memory and do not survive a daemon restart)")
		return
	}
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > operationMaxWaitS {
			writeValidationErrorCodes(w,
				map[string]string{"wait": "must be a whole number of seconds from 0 to 300"},
				map[string]string{"wait": "must_be_integer"})
			return
		}
		if n > 0 {
			s.ops.wait(r.Context(), op, time.Duration(n)*time.Second)
		}
	}
	writeJSON(w, http.StatusOK, s.ops.view(op))
}

// GET /api/v1/operations[?slot=a1] lists recent operations, newest first.
func (s *Server) handleOperationsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"operations": s.ops.list(r.URL.Query().Get("slot"), 50)})
}

// SPDX-License-Identifier: Apache-2.0

package duplexobs

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/jsaigou/the-forge/internal/duplex"
)

// Registry owns the observers for a process. It is safe for concurrent use.
// Closed sessions are retired into the lifetime counters so their _total
// series stay monotonic after the session is forgotten.
type Registry struct {
	mu sync.Mutex

	cfg      Config
	sessions map[string]*Obs
	lifetime lifetimeTotals
	started  uint64
}

// NewRegistry returns an empty registry. The zero Config is usable.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		cfg:      cfg.withDefaults(),
		sessions: make(map[string]*Obs),
		lifetime: lifetimeTotals{
			dropsByReason:     make(map[string]uint64),
			safetyStopsByCode: make(map[string]uint64),
			errorsByCode:      make(map[duplex.ErrorCode]uint64),
		},
	}
}

// Start registers and returns a session observer. Starting an id that is
// already active replaces it; the old observer remains usable by its caller.
func (r *Registry) Start(info duplex.SessionInfo) *Obs {
	o := New(info, r.cfg)
	r.mu.Lock()
	r.sessions[info.SessionID] = o
	r.started++
	r.mu.Unlock()
	return o
}

// Get returns the active observer for a session id.
func (r *Registry) Get(id string) (*Obs, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.sessions[id]
	return o, ok
}

// Close closes a session, retires its counters and returns its final summary.
func (r *Registry) Close(id, reason string) (Summary, bool) {
	r.mu.Lock()
	o, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	if !ok {
		return Summary{}, false
	}
	s := o.Close(reason)
	r.mu.Lock()
	r.lifetime.add(s)
	r.mu.Unlock()
	return s, true
}

// Len returns the number of active sessions.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Summaries returns a snapshot of every active session, sorted by session id.
func (r *Registry) Summaries() []Summary {
	r.mu.Lock()
	obs := make([]*Obs, 0, len(r.sessions))
	for _, o := range r.sessions {
		obs = append(obs, o)
	}
	r.mu.Unlock()

	out := make([]Summary, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// WriteMetrics renders aggregate Prometheus text exposition for all sessions
// seen by this registry: active sessions plus retired lifetime counters.
// Percentile gauges are the worst active session; queue and GPU values are
// sums across active sessions.
func (r *Registry) WriteMetrics(w io.Writer) (int64, error) {
	r.mu.Lock()
	obs := make([]*Obs, 0, len(r.sessions))
	for _, o := range r.sessions {
		obs = append(obs, o)
	}
	active := len(obs)
	total := r.started
	life := r.lifetime.clone()
	r.mu.Unlock()

	agg := life

	var queueIn, queueOut int
	var gpuUsed, gpuPeak int64
	var stepP50, stepP95, stepMax, e2eP50, e2eP95 float64
	for _, o := range obs {
		s := o.Summary()
		agg.inFrames += s.InFrames
		agg.outFrames += s.OutFrames
		agg.lateInputs += s.LateInputs
		agg.lateOutputs += s.LateOutputs
		agg.dropped += s.Dropped
		agg.textDeltas += s.TextDeltas
		for k, v := range s.DropsByReason {
			agg.dropsByReason[k] += v
		}
		for k, v := range s.SafetyStopsByCode {
			agg.safetyStopsByCode[k] += v
		}
		for k, v := range s.ErrorsByCode {
			agg.errorsByCode[k] += v
		}
		queueIn += s.QueueIn
		queueOut += s.QueueOut
		gpuUsed += s.GPUUsedBytes
		gpuPeak += s.GPUPeakBytes
		stepP50 = maxFloat(stepP50, s.StepP50MS)
		stepP95 = maxFloat(stepP95, s.StepP95MS)
		stepMax = maxFloat(stepMax, s.StepMaxMS)
		e2eP50 = maxFloat(e2eP50, s.E2EP50MS)
		e2eP95 = maxFloat(e2eP95, s.E2EP95MS)
	}

	var n int64
	var firstErr error
	write := func(format string, args ...any) {
		written, err := fmt.Fprintf(w, format, args...)
		n += int64(written)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	write("duplex_sessions_active %d\n", active)
	write("duplex_sessions_total %d\n", total)
	write("duplex_in_frames_total %d\n", agg.inFrames)
	write("duplex_out_frames_total %d\n", agg.outFrames)
	write("duplex_late_inputs_total %d\n", agg.lateInputs)
	write("duplex_late_outputs_total %d\n", agg.lateOutputs)
	write("duplex_dropped_total %d\n", agg.dropped)
	write("duplex_queue_in_frames %d\n", queueIn)
	write("duplex_queue_out_frames %d\n", queueOut)
	write("duplex_step_ms_p50 %s\n", formatFloat(stepP50))
	write("duplex_step_ms_p95 %s\n", formatFloat(stepP95))
	write("duplex_step_ms_max %s\n", formatFloat(stepMax))
	write("duplex_end_to_end_ms_p50 %s\n", formatFloat(e2eP50))
	write("duplex_end_to_end_ms_p95 %s\n", formatFloat(e2eP95))
	write("duplex_text_deltas_total %d\n", agg.textDeltas)
	write("duplex_gpu_used_bytes %d\n", gpuUsed)
	write("duplex_gpu_peak_bytes %d\n", gpuPeak)

	for _, reason := range sortedKeys(agg.dropsByReason) {
		write("duplex_dropped_total{reason=%q} %d\n", reason, agg.dropsByReason[reason])
	}
	for _, code := range sortedKeys(agg.safetyStopsByCode) {
		write("duplex_safety_stops_total{code=%q} %d\n", code, agg.safetyStopsByCode[code])
	}
	for _, code := range sortedErrorCodes(agg.errorsByCode) {
		write("duplex_errors_total{code=%q} %d\n", code, agg.errorsByCode[duplex.ErrorCode(code)])
	}
	return n, firstErr
}

type lifetimeTotals struct {
	inFrames          uint64
	outFrames         uint64
	lateInputs        uint64
	lateOutputs       uint64
	dropped           uint64
	textDeltas        uint64
	dropsByReason     map[string]uint64
	safetyStopsByCode map[string]uint64
	errorsByCode      map[duplex.ErrorCode]uint64
}

func (l *lifetimeTotals) add(s Summary) {
	l.inFrames += s.InFrames
	l.outFrames += s.OutFrames
	l.lateInputs += s.LateInputs
	l.lateOutputs += s.LateOutputs
	l.dropped += s.Dropped
	l.textDeltas += s.TextDeltas
	for k, v := range s.DropsByReason {
		l.dropsByReason[k] += v
	}
	for k, v := range s.SafetyStopsByCode {
		l.safetyStopsByCode[k] += v
	}
	for k, v := range s.ErrorsByCode {
		l.errorsByCode[k] += v
	}
}

func (l lifetimeTotals) clone() lifetimeTotals {
	l.dropsByReason = copyStringCounts(l.dropsByReason)
	l.safetyStopsByCode = copyStringCounts(l.safetyStopsByCode)
	l.errorsByCode = copyErrorCounts(l.errorsByCode)
	return l
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedErrorCodes(m map[duplex.ErrorCode]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

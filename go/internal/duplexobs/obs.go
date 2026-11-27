// SPDX-License-Identifier: Apache-2.0

// Package duplexobs aggregates per-session duplex accounting into the
// protocol's stats payload, a session summary for logs, and an optional
// Prometheus text exposition.
//
// It is deliberately independent of the driver, transport, loopback tenant and
// client: those packages call Observer/Registry when the integration lands.
// One Obs is a session's thread-safe recorder; a Registry owns the per-process
// set of sessions. Durations are recorded as wall-clock milliseconds, matching
// duplex.Stats and protocol.md §3.
package duplexobs

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/statutil"
)

// DefaultSampleCapacity bounds each per-session latency window. At 7.5 frames
// per second this is roughly 18 minutes; max and counters remain lifetime
// values, so a session that outlives the window reports recent percentiles and
// its true maximum.
const DefaultSampleCapacity = 8192

// Config controls an Obs or Registry. The zero value is usable.
type Config struct {
	// Now is the session clock. Tests inject it; nil means time.Now.
	Now func() time.Time
	// SampleCapacity is the per-stream latency window. Zero means
	// DefaultSampleCapacity.
	SampleCapacity int
	// Redactor overrides the default redaction patterns. Nil means NewRedactor.
	Redactor *Redactor
}

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.SampleCapacity <= 0 {
		c.SampleCapacity = DefaultSampleCapacity
	}
	if c.Redactor == nil {
		c.Redactor = NewRedactor()
	}
	return c
}

// Observer is the per-session surface a driver or transport calls. Methods are
// safe for concurrent use.
//
// Frame accounting has two entry points because they carry different data:
// Output records a frame plus its end-to-end latency, while Observe handles
// driver events (text, stats, done, error, and an untimed audio event). For a
// given frame, call Output or Observe(EventAudio), not both.
type Observer interface {
	Input(late bool)
	Step(d time.Duration)
	Output(endToEnd time.Duration, late bool)
	Queues(in, out int)
	Drop(reason string)
	Error(code duplex.ErrorCode)
	Observe(ev duplex.Event)
	Summary() Summary
}

// Percentiles is a step-time distribution. Max is a lifetime maximum.
type Percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// Latency is an end-to-end distribution (protocol.md's end_to_end_ms).
type Latency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

// GPUStats mirrors the protocol payload's gpu object.
type GPUStats struct {
	UsedBytes int64 `json:"used_bytes"`
	PeakBytes int64 `json:"peak_bytes"`
}

// StatsPayload is the JSON body transport sends as protocol.md §3.2's stats
// message. Type is always "stats"; Seq is allocated per session.
type StatsPayload struct {
	Type        string      `json:"type"`
	Seq         uint64      `json:"seq"`
	UptimeMS    int64       `json:"uptime_ms"`
	InFrames    uint64      `json:"in_frames"`
	OutFrames   uint64      `json:"out_frames"`
	LateInputs  uint64      `json:"late_inputs"`
	LateOutputs uint64      `json:"late_outputs"`
	QueueIn     int         `json:"queue_in"`
	QueueOut    int         `json:"queue_out"`
	Dropped     uint64      `json:"dropped"`
	StepMS      Percentiles `json:"step_ms"`
	EndToEndMS  Latency     `json:"end_to_end_ms"`
	GPU         GPUStats    `json:"gpu"`
}

// Summary is a session's final or in-flight diagnostic view. It is also what
// LogLine renders. It never contains tokens, a reference id or reference audio.
type Summary struct {
	SessionID     string            `json:"session_id"`
	Tenant        string            `json:"tenant"`
	UptimeMS      int64             `json:"uptime_ms"`
	InFrames      uint64            `json:"in_frames"`
	OutFrames     uint64            `json:"out_frames"`
	LateInputs    uint64            `json:"late_inputs"`
	LateOutputs   uint64            `json:"late_outputs"`
	QueueIn       int               `json:"queue_in"`
	QueueOut      int               `json:"queue_out"`
	Dropped       uint64            `json:"dropped"`
	DropsByReason map[string]uint64 `json:"drops_by_reason,omitempty"`

	StepP50MS float64 `json:"step_ms_p50"`
	StepP95MS float64 `json:"step_ms_p95"`
	StepMaxMS float64 `json:"step_ms_max"`
	E2EP50MS  float64 `json:"end_to_end_ms_p50"`
	E2EP95MS  float64 `json:"end_to_end_ms_p95"`

	TextDeltas        uint64                      `json:"text_deltas"`
	SafetyStopsByCode map[string]uint64           `json:"safety_stops_by_code,omitempty"`
	ErrorsByCode      map[duplex.ErrorCode]uint64 `json:"errors_by_code,omitempty"`
	LastError         duplex.ErrorCode            `json:"last_error,omitempty"`

	GPUUsedBytes int64 `json:"gpu_used_bytes"`
	GPUPeakBytes int64 `json:"gpu_peak_bytes"`

	ReferenceBytes int  `json:"reference_bytes"`
	HasReference   bool `json:"has_reference"`

	Closed      bool   `json:"closed"`
	Done        bool   `json:"done"`
	CloseReason string `json:"close_reason,omitempty"`

	Redaction RedactionStatus `json:"redaction"`
}

// ContractStats projects the summary onto the frozen duplex.Stats type.
func (s Summary) ContractStats() duplex.Stats {
	return duplex.Stats{
		SessionID:   s.SessionID,
		Uptime:      time.Duration(s.UptimeMS) * time.Millisecond,
		InFrames:    s.InFrames,
		OutFrames:   s.OutFrames,
		LateInputs:  s.LateInputs,
		LateOutputs: s.LateOutputs,
		QueueIn:     s.QueueIn,
		QueueOut:    s.QueueOut,
		Dropped:     s.Dropped,
		StepP50MS:   s.StepP50MS,
		StepP95MS:   s.StepP95MS,
		StepMaxMS:   s.StepMaxMS,
		E2EP50MS:    s.E2EP50MS,
		E2EP95MS:    s.E2EP95MS,
	}
}

// LogFields returns ordered logfmt fields for a session summary.
func (s Summary) LogFields() []Field {
	fields := []Field{
		{Key: "session_id", Value: s.SessionID},
		{Key: "tenant", Value: s.Tenant},
		{Key: "uptime_ms", Value: strconv.FormatInt(s.UptimeMS, 10)},
		{Key: "in_frames", Value: strconv.FormatUint(s.InFrames, 10)},
		{Key: "out_frames", Value: strconv.FormatUint(s.OutFrames, 10)},
		{Key: "late_inputs", Value: strconv.FormatUint(s.LateInputs, 10)},
		{Key: "late_outputs", Value: strconv.FormatUint(s.LateOutputs, 10)},
		{Key: "queue_in", Value: strconv.Itoa(s.QueueIn)},
		{Key: "queue_out", Value: strconv.Itoa(s.QueueOut)},
		{Key: "dropped", Value: strconv.FormatUint(s.Dropped, 10)},
		{Key: "step_ms_p50", Value: formatFloat(s.StepP50MS)},
		{Key: "step_ms_p95", Value: formatFloat(s.StepP95MS)},
		{Key: "step_ms_max", Value: formatFloat(s.StepMaxMS)},
		{Key: "e2e_ms_p50", Value: formatFloat(s.E2EP50MS)},
		{Key: "e2e_ms_p95", Value: formatFloat(s.E2EP95MS)},
		{Key: "text_deltas", Value: strconv.FormatUint(s.TextDeltas, 10)},
		{Key: "closed", Value: strconv.FormatBool(s.Closed)},
		{Key: "done", Value: strconv.FormatBool(s.Done)},
		{Key: "redaction", Value: redactionState(s.Redaction)},
		{Key: "redacted", Value: strconv.FormatUint(s.Redaction.PatternsRedacted, 10)},
		{Key: "reference_audio", Value: "withheld"},
	}
	if s.HasReference {
		fields = append(fields, Field{Key: "reference_bytes", Value: strconv.Itoa(s.ReferenceBytes)})
	}
	if s.CloseReason != "" {
		fields = append(fields, Field{Key: "close_reason", Value: s.CloseReason})
	}
	if s.LastError != "" {
		fields = append(fields, Field{Key: "last_error", Value: string(s.LastError)})
	}
	if len(s.DropsByReason) > 0 {
		fields = append(fields, Field{Key: "drops", Value: formatDrops(s.DropsByReason)})
	}
	if len(s.ErrorsByCode) > 0 {
		fields = append(fields, Field{Key: "errors", Value: formatErrors(s.ErrorsByCode)})
	}
	return fields
}

// LogLine renders the summary as one redacted logfmt line. The observer already
// redacts values it accepts, but a hand-built Summary is scrubbed here too.
func (s Summary) LogLine() string {
	r := NewRedactor()
	fields := s.LogFields()
	for i := range fields {
		fields[i].Value, _ = r.Redact(fields[i].Value)
	}
	return LogLine(fields)
}

func redactionState(s RedactionStatus) string {
	if s.Enabled {
		return "enabled"
	}
	return "disabled"
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func formatDrops(m map[string]uint64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(m[k], 10))
	}
	return b.String()
}

func formatErrors(m map[duplex.ErrorCode]uint64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(m[duplex.ErrorCode(k)], 10))
	}
	return b.String()
}

// Obs is one session's observer. The zero value is not usable; use New or
// Registry.Start. All methods are safe for concurrent use.
type Obs struct {
	mu sync.Mutex

	sessionID string
	tenant    string
	started   time.Time
	now       func() time.Time
	redactor  *Redactor

	inFrames    uint64
	outFrames   uint64
	lateInputs  uint64
	lateOutputs uint64
	dropped     uint64
	textDeltas  uint64
	queueIn     int
	queueOut    int

	step    sampleRing
	e2e     sampleRing
	stepMax float64

	// Fallbacks come from a driver EventStats snapshot; they are used only
	// until the observer has its own samples for that stream.
	stepP50Fallback float64
	stepP95Fallback float64
	e2eP50Fallback  float64
	e2eP95Fallback  float64

	dropsByReason     map[string]uint64
	safetyStopsByCode map[string]uint64
	errorsByCode      map[duplex.ErrorCode]uint64
	lastError         duplex.ErrorCode

	gpuUsed int64
	gpuPeak int64

	referenceBytes int
	hasReference   bool

	redactedPatterns uint64
	closed           bool
	done             bool
	closeReason      string

	statsSeq uint64
}

// New returns an observer for one session. The session id and tenant are
// redacted defensively; reference audio is never part of SessionInfo.
func New(info duplex.SessionInfo, cfg Config) *Obs {
	cfg = cfg.withDefaults()
	id, n := cfg.Redactor.Redact(info.SessionID)
	tenant, m := cfg.Redactor.Redact(info.Tenant)
	return &Obs{
		sessionID:         id,
		tenant:            tenant,
		started:           cfg.Now(),
		now:               cfg.Now,
		redactor:          cfg.Redactor,
		step:              newSampleRing(cfg.SampleCapacity),
		e2e:               newSampleRing(cfg.SampleCapacity),
		dropsByReason:     make(map[string]uint64),
		safetyStopsByCode: make(map[string]uint64),
		errorsByCode:      make(map[duplex.ErrorCode]uint64),
		redactedPatterns:  uint64(n + m),
	}
}

// Input records one accepted input frame and whether it missed its tick.
func (o *Obs) Input(late bool) {
	o.mu.Lock()
	o.inFrames++
	if late {
		o.lateInputs++
	}
	o.mu.Unlock()
}

// Step records one tenant step duration.
func (o *Obs) Step(d time.Duration) {
	ms := durationMS(d)
	o.mu.Lock()
	o.step.add(ms)
	if ms > o.stepMax {
		o.stepMax = ms
	}
	o.mu.Unlock()
}

// Output records one delivered output frame and its end-to-end latency.
func (o *Obs) Output(endToEnd time.Duration, late bool) {
	ms := durationMS(endToEnd)
	o.mu.Lock()
	o.outFrames++
	if late {
		o.lateOutputs++
	}
	o.e2e.add(ms)
	o.mu.Unlock()
}

// Queues records the current input and output queue depths.
func (o *Obs) Queues(in, out int) {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	o.mu.Lock()
	o.queueIn = in
	o.queueOut = out
	o.mu.Unlock()
}

// Drop records one frame or event that was lost, with its redacted reason.
func (o *Obs) Drop(reason string) {
	o.mu.Lock()
	reason = o.redactLocked(reason)
	if reason == "" {
		reason = "unspecified"
	}
	o.dropped++
	o.dropsByReason[reason]++
	o.mu.Unlock()
}

// Error records one protocol error code (protocol.md §6).
func (o *Obs) Error(code duplex.ErrorCode) {
	if code == "" {
		return
	}
	o.mu.Lock()
	o.errorsByCode[code]++
	o.lastError = code
	o.mu.Unlock()
}

// SafetyStop records one platform policy stop under its stable code. The code
// becomes a metric label; the observed text is never recorded.
func (o *Obs) SafetyStop(code string) {
	if code == "" {
		return
	}
	o.mu.Lock()
	o.safetyStopsByCode[code]++
	o.mu.Unlock()
}

// Observe consumes one driver event. EventAudio counts an output frame but
// carries no timing; use Output instead when end-to-end latency is known.
func (o *Obs) Observe(ev duplex.Event) {
	switch ev.Kind {
	case duplex.EventAudio:
		o.mu.Lock()
		o.outFrames++
		o.mu.Unlock()
	case duplex.EventText:
		o.mu.Lock()
		o.textDeltas++
		o.mu.Unlock()
	case duplex.EventStats:
		o.mu.Lock()
		o.mergeStatsLocked(ev.Stats)
		o.mu.Unlock()
	case duplex.EventDone:
		o.mu.Lock()
		o.done = true
		if ev.Reason != "" && !o.closed {
			o.closeReason = o.redactLocked(ev.Reason)
		}
		o.mu.Unlock()
	case duplex.EventError:
		if ev.Error != nil {
			o.Error(ev.Error.Code)
		}
	}
}

// SetReference records reference metadata only: presence and byte count. The
// audio bytes are discarded, which is why summaries cannot leak them.
func (o *Obs) SetReference(ref duplex.Reference) {
	o.mu.Lock()
	o.referenceBytes = len(ref.Audio)
	o.hasReference = ref.ID != "" || len(ref.Audio) > 0
	o.mu.Unlock()
}

// SetGPU records current and peak GPU bytes for this session.
func (o *Obs) SetGPU(used, peak int64) {
	if used < 0 {
		used = 0
	}
	if peak < used {
		peak = used
	}
	o.mu.Lock()
	o.gpuUsed = used
	o.gpuPeak = peak
	o.mu.Unlock()
}

// Summary returns a consistent snapshot. It does not close the session.
func (o *Obs) Summary() Summary {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.summaryLocked()
}

// StatsPayload returns the next protocol stats message body. Seq starts at 0
// and increments per call.
func (o *Obs) StatsPayload() StatsPayload {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.summaryLocked()
	seq := o.statsSeq
	o.statsSeq++
	return StatsPayload{
		Type:        "stats",
		Seq:         seq,
		UptimeMS:    s.UptimeMS,
		InFrames:    s.InFrames,
		OutFrames:   s.OutFrames,
		LateInputs:  s.LateInputs,
		LateOutputs: s.LateOutputs,
		QueueIn:     s.QueueIn,
		QueueOut:    s.QueueOut,
		Dropped:     s.Dropped,
		StepMS:      Percentiles{P50: s.StepP50MS, P95: s.StepP95MS, Max: s.StepMaxMS},
		EndToEndMS:  Latency{P50: s.E2EP50MS, P95: s.E2EP95MS},
		GPU:         GPUStats{UsedBytes: s.GPUUsedBytes, PeakBytes: s.GPUPeakBytes},
	}
}

// Close marks the session closed and returns its final summary. It is
// idempotent; the first close reason wins.
func (o *Obs) Close(reason string) Summary {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		if reason != "" {
			o.closeReason = o.redactLocked(reason)
		}
	}
	return o.summaryLocked()
}

func (o *Obs) summaryLocked() Summary {
	uptime := o.now().Sub(o.started)
	if uptime < 0 {
		uptime = 0
	}

	stepP50, stepP95 := o.stepP50Fallback, o.stepP95Fallback
	if vals := o.step.snapshot(); len(vals) > 0 {
		stepP50 = statutil.Percentile(vals, 50)
		stepP95 = statutil.Percentile(vals, 95)
	}
	e2eP50, e2eP95 := o.e2eP50Fallback, o.e2eP95Fallback
	if vals := o.e2e.snapshot(); len(vals) > 0 {
		e2eP50 = statutil.Percentile(vals, 50)
		e2eP95 = statutil.Percentile(vals, 95)
	}

	return Summary{
		SessionID:         o.sessionID,
		Tenant:            o.tenant,
		UptimeMS:          uptime.Milliseconds(),
		InFrames:          o.inFrames,
		OutFrames:         o.outFrames,
		LateInputs:        o.lateInputs,
		LateOutputs:       o.lateOutputs,
		QueueIn:           o.queueIn,
		QueueOut:          o.queueOut,
		Dropped:           o.dropped,
		DropsByReason:     copyStringCounts(o.dropsByReason),
		StepP50MS:         stepP50,
		StepP95MS:         stepP95,
		StepMaxMS:         o.stepMax,
		E2EP50MS:          e2eP50,
		E2EP95MS:          e2eP95,
		TextDeltas:        o.textDeltas,
		SafetyStopsByCode: copyStringCounts(o.safetyStopsByCode),
		ErrorsByCode:      copyErrorCounts(o.errorsByCode),
		LastError:         o.lastError,
		GPUUsedBytes:      o.gpuUsed,
		GPUPeakBytes:      o.gpuPeak,
		ReferenceBytes:    o.referenceBytes,
		HasReference:      o.hasReference,
		Closed:            o.closed,
		Done:              o.done,
		CloseReason:       o.closeReason,
		Redaction: RedactionStatus{
			Enabled:                true,
			ReferenceAudioWithheld: true,
			PatternsRedacted:       o.redactedPatterns,
		},
	}
}

func (o *Obs) mergeStatsLocked(s duplex.Stats) {
	o.inFrames = maxU64(o.inFrames, s.InFrames)
	o.outFrames = maxU64(o.outFrames, s.OutFrames)
	o.lateInputs = maxU64(o.lateInputs, s.LateInputs)
	o.lateOutputs = maxU64(o.lateOutputs, s.LateOutputs)
	o.dropped = maxU64(o.dropped, s.Dropped)
	o.queueIn = s.QueueIn
	o.queueOut = s.QueueOut
	if s.StepMaxMS > o.stepMax {
		o.stepMax = s.StepMaxMS
	}
	if o.step.empty() {
		o.stepP50Fallback = s.StepP50MS
		o.stepP95Fallback = s.StepP95MS
	}
	if o.e2e.empty() {
		o.e2eP50Fallback = s.E2EP50MS
		o.e2eP95Fallback = s.E2EP95MS
	}
}

func (o *Obs) redactLocked(s string) string {
	if s == "" {
		return s
	}
	out, n := o.redactor.Redact(s)
	o.redactedPatterns += uint64(n)
	return out
}

func durationMS(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond)
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func copyStringCounts(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyErrorCounts(m map[duplex.ErrorCode]uint64) map[duplex.ErrorCode]uint64 {
	out := make(map[duplex.ErrorCode]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sampleRing is a bounded FIFO window of latency samples. It is not safe for
// concurrent use; Obs holds its mutex.
type sampleRing struct {
	buf    []float64
	next   int
	filled int
}

func newSampleRing(capacity int) sampleRing {
	if capacity <= 0 {
		capacity = DefaultSampleCapacity
	}
	return sampleRing{buf: make([]float64, capacity)}
}

func (r *sampleRing) add(v float64) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.filled < len(r.buf) {
		r.filled++
	}
}

func (r *sampleRing) empty() bool { return r.filled == 0 }

func (r *sampleRing) snapshot() []float64 {
	if r.filled == 0 {
		return nil
	}
	if r.filled < len(r.buf) {
		return append([]float64(nil), r.buf[:r.filled]...)
	}
	out := make([]float64, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

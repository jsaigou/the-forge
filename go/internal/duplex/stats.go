// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/statutil"
)

// maxSamples bounds the per-session latency history. p50/p95 are computed over
// this recent window; StepMaxMS is a session-long running maximum so a single
// early outlier is never lost to ring overwrite.
const maxSamples = 2048

// sampleRing is a bounded ring of float64 latency samples (milliseconds). It
// keeps the most recent maxSamples values and yields them in arrival order for
// percentile computation.
type sampleRing struct {
	buf  []float64
	n    int // items currently stored (< cap while not yet full)
	i    int // write index once full
	full bool
}

func newSampleRing() *sampleRing {
	return &sampleRing{buf: make([]float64, maxSamples)}
}

func (s *sampleRing) add(v float64) {
	if s.full {
		s.buf[s.i] = v
		s.i = (s.i + 1) % len(s.buf)
		return
	}
	s.buf[s.n] = v
	s.n++
	if s.n == len(s.buf) {
		s.n = 0
		s.full = true
	}
}

// values returns the stored samples in arrival order.
func (s *sampleRing) values() []float64 {
	if !s.full {
		return append([]float64(nil), s.buf[:s.n]...)
	}
	out := make([]float64, 0, len(s.buf))
	out = append(out, s.buf[s.i:]...)
	out = append(out, s.buf[:s.i]...)
	return out
}

// stats is the driver's per-session accounting. The engine writes; Stats and
// the terminal snapshot read. Durations are wall-clock milliseconds.
type stats struct {
	mu        sync.Mutex
	inFrames  uint64
	outFrames uint64
	lateIn    uint64
	lateOut   uint64
	dropped   uint64
	step      *sampleRing
	e2e       *sampleRing
	stepMax   float64
}

func newStats() *stats {
	return &stats{
		step: newSampleRing(),
		e2e:  newSampleRing(),
	}
}

func (s *stats) addInFrame(n uint64) {
	s.mu.Lock()
	s.inFrames += n
	s.mu.Unlock()
}

func (s *stats) addOutFrame(n uint64) {
	s.mu.Lock()
	s.outFrames += n
	s.mu.Unlock()
}

func (s *stats) addLateInput(n uint64) {
	s.mu.Lock()
	s.lateIn += n
	s.mu.Unlock()
}

func (s *stats) addLateOutput(n uint64) {
	s.mu.Lock()
	s.lateOut += n
	s.mu.Unlock()
}

func (s *stats) addDropped(n uint64) {
	s.mu.Lock()
	s.dropped += n
	s.mu.Unlock()
}

func (s *stats) addStep(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	s.mu.Lock()
	s.step.add(ms)
	if ms > s.stepMax {
		s.stepMax = ms
	}
	s.mu.Unlock()
}

func (s *stats) addE2E(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	s.mu.Lock()
	s.e2e.add(ms)
	s.mu.Unlock()
}

// snapshot returns the current Stats. queueIn/queueOut are read from the live
// rings by the caller (the stats struct does not own the queues).
func (s *stats) snapshot(sessionID string, uptime time.Duration, queueIn, queueOut int) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		SessionID:   sessionID,
		Uptime:      uptime,
		InFrames:    s.inFrames,
		OutFrames:   s.outFrames,
		LateInputs:  s.lateIn,
		LateOutputs: s.lateOut,
		QueueIn:     queueIn,
		QueueOut:    queueOut,
		Dropped:     s.dropped,
		StepP50MS:   statutil.Percentile(s.step.values(), 50),
		StepP95MS:   statutil.Percentile(s.step.values(), 95),
		StepMaxMS:   s.stepMax,
		E2EP50MS:    statutil.Percentile(s.e2e.values(), 50),
		E2EP95MS:    statutil.Percentile(s.e2e.values(), 95),
	}
}

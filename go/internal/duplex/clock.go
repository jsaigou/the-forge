// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"sync"
	"time"
)

// Clock is the session frame clock: it delivers one boundary on C per frame
// period (FrameDuration, 7.5 Hz) and reports the current time via Now.
//
// Two modes exist. NewClock wraps a time.Ticker and is used in production;
// the ticker goroutine forwards boundaries into a one-deep buffer so a
// momentarily slow driver coalesces rather than over-running. NewManualClock
// is the test clock: time does not move until the test says so, and Step
// advances time by one Period, emits a boundary, and blocks until the driver
// acknowledges it — giving tests a deterministic handshake. Advance moves
// manual time forward without emitting a boundary, for fault tests that need
// a wall-clock jump (idle/time-limit/overflow timeouts).
//
// Step is a test-only method; it must not be called concurrently with driver
// cancellation, and never after the driver has finished, because the driver
// is the only receiver on the boundary channel.
type Clock struct {
	// Period is the frame period; zero means FrameDuration.
	Period time.Duration

	manual bool

	mu    sync.Mutex
	mnow  time.Time
	nowFn func() time.Time

	// emit is the boundary channel the driver reads. Unbuffered in manual
	// mode (Step pairs 1:1 with the driver), buffered (cap 1) in real mode.
	emit chan struct{}
	// ack is the manual-mode acknowledge channel; nil in real mode.
	ack chan struct{}

	ticker  *time.Ticker
	stop    chan struct{}
	started bool
	wg      sync.WaitGroup
}

// NewClock returns a real (ticker-backed) frame clock. The returned clock is
// not started until Start is called.
func NewClock() *Clock {
	return &Clock{
		Period: FrameDuration,
		nowFn:  time.Now,
		emit:   make(chan struct{}, 1),
	}
}

// NewManualClock returns a test clock whose time starts at start and only
// advances under explicit Step/Advance calls.
func NewManualClock(start time.Time) *Clock {
	return &Clock{
		Period: FrameDuration,
		manual: true,
		mnow:   start,
		emit:   make(chan struct{}),
		ack:    make(chan struct{}),
	}
}

// Now reports the current clock time. A nil time source falls back to
// time.Now (never the case for the two constructors above).
func (c *Clock) Now() time.Time {
	if c.manual {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.mnow
	}
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// C returns the boundary channel. The driver reads one value per frame.
func (c *Clock) C() <-chan struct{} {
	return c.emit
}

// Start begins emitting boundaries. It is a no-op for a manual clock (the
// test drives boundaries with Step) and idempotent for a real clock.
func (c *Clock) Start() {
	if c.manual {
		return
	}
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.stop = make(chan struct{})
	c.ticker = time.NewTicker(c.period())
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case <-c.ticker.C:
				select {
				case c.emit <- struct{}{}:
				default: // driver behind: coalesce this boundary
				}
			case <-c.stop:
				return
			}
		}
	}()
	c.mu.Unlock()
}

// Stop halts boundary emission and waits for the forwarding goroutine. It is
// a no-op for a manual clock and idempotent.
func (c *Clock) Stop() {
	if c.manual {
		return
	}
	c.mu.Lock()
	if c.ticker != nil {
		c.ticker.Stop()
	}
	if c.stop != nil {
		close(c.stop)
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// Ack acknowledges one processed boundary. Only the manual clock needs it
// (Step blocks on it); the real clock's no-op keeps the driver's loop uniform.
func (c *Clock) Ack() {
	if c.ack != nil {
		c.ack <- struct{}{}
	}
}

// Step advances a manual clock by one period, emits a boundary, and blocks
// until the driver has processed and acknowledged it.
func (c *Clock) Step() {
	if !c.manual {
		return
	}
	c.mu.Lock()
	c.mnow = c.mnow.Add(c.period())
	c.mu.Unlock()
	c.emit <- struct{}{}
	<-c.ack
}

// Advance moves a manual clock's time forward without emitting a boundary.
func (c *Clock) Advance(d time.Duration) {
	if !c.manual {
		return
	}
	c.mu.Lock()
	c.mnow = c.mnow.Add(d)
	c.mu.Unlock()
}

func (c *Clock) period() time.Duration {
	if c.Period > 0 {
		return c.Period
	}
	return FrameDuration
}

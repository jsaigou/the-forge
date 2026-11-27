// SPDX-License-Identifier: Apache-2.0

// Package duplexsafety contains the tenant-policy hooks the duplex platform
// enforces on a session's text stream (docs/duplex/plan.md principle 8).
//
// A Policy observes each text.delta as it streams and can ask the session to
// stop. Policies are stateful and must be created once per session; the
// forge-duplex factory does that. A policy is not safe for concurrent use and
// keeps bounded memory. The package has no third-party dependencies and never
// logs or retains transcript text beyond its bounded window.
package duplexsafety

// Verdict is the result of observing one text delta. Stop ends the session.
// Code is a short stable identifier suitable for a metric label and a log
// field; Reason is human-readable and never contains observed text.
type Verdict struct {
	Stop   bool
	Reason string
	Code   string
}

// Stable policy codes. They are the metric label values.
const (
	CodeLoop = "loop"
	CodeTerm = "term"
)

// Policy observes one streaming text delta. Implementations must be created
// per session, keep bounded state, and do work proportional to their bounded
// window rather than to the session length.
type Policy interface {
	Observe(delta string) Verdict
}

// Chain runs policies in order and returns the first stop. Later policies are
// not observed after a stop. A nil policy is ignored; a nil Chain observes
// nothing.
type Chain struct {
	policies []Policy
}

// NewChain builds a chain from the non-nil policies.
func NewChain(policies ...Policy) *Chain {
	out := make([]Policy, 0, len(policies))
	for _, p := range policies {
		if p != nil {
			out = append(out, p)
		}
	}
	return &Chain{policies: out}
}

// Observe feeds delta to each policy until one stops the session.
func (c *Chain) Observe(delta string) Verdict {
	if c == nil {
		return Verdict{}
	}
	for _, p := range c.policies {
		if v := p.Observe(delta); v.Stop {
			return v
		}
	}
	return Verdict{}
}

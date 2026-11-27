// SPDX-License-Identifier: Apache-2.0

package tts

// GET /v1/readiness: the daemon's speech readiness (docs/speech-setup-plan.md section 2.2) passed on to a client such as voice-studio, with what only
// the front door knows: whether the daemon answers and accepts the key, and when that key expires. It is open like the engine list: ids, states and
// engine names only. The daemon's error detail, which can name files on the host, is not passed on.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// How the daemon answered the readiness question.
const (
	DaemonOK          = "ok"
	DaemonUnreachable = "unreachable"
	DaemonKeyRefused  = "key_refused"
	DaemonUnsupported = "unsupported" // an older daemon without the report
)

const (
	codeDaemonUnreachable    = "speech_daemon_unreachable"
	codeResolverUnauthorized = "speech_resolver_unauthorized"
	codeReadinessUnsupported = "speech_readiness_unsupported"
)

// readinessCapabilities are the capabilities the daemon reports, with the kind of speech each needs. They are listed here so that a report can still name
// them, as unknown, when the daemon cannot be asked. A test pins them to the daemon's.
var readinessCapabilities = []ReadinessCapability{
	{ID: "preset", Mode: string(ModeKokoro)},
	{ID: "clone", Mode: string(ModeBase)},
	{ID: "design", Mode: string(ModeVoiceDesign)},
}

const keyWarningDays = 30

// ReadinessCapability is one capability as passed on to a client.
type ReadinessCapability struct {
	ID     string `json:"id"`
	Mode   string `json:"mode"`
	State  string `json:"state"`
	Usable bool   `json:"usable"`
	Engine string `json:"engine,omitempty"`
	Recipe string `json:"recipe,omitempty"`
	Code   string `json:"code,omitempty"`
	Action string `json:"action,omitempty"`
}

// Readiness is the answer of GET /v1/readiness. Code says why the capabilities are unknown, when they are.
type Readiness struct {
	Ready            bool                  `json:"ready"`
	Usable           bool                  `json:"usable"`
	Daemon           string                `json:"daemon"`
	Code             string                `json:"code,omitempty"`
	KeyExpiresAt     string                `json:"key_expires_at,omitempty"`
	KeyExpiresInDays *int                  `json:"key_expires_in_days,omitempty"`
	KeyWarning       bool                  `json:"key_warning,omitempty"`
	Capabilities     []ReadinessCapability `json:"capabilities"`
}

// ReadinessReporter answers GET /v1/readiness. Only the daemon resolver can.
type ReadinessReporter interface {
	Readiness(ctx context.Context) Readiness
}

const readinessTimeout = 5 * time.Second

// Readiness asks the daemon. It never starts an engine, and it always returns a report: failing to ask is itself the thing to report.
func (d *DaemonResolver) Readiness(ctx context.Context) Readiness {
	out := d.askReadiness(ctx)
	if !d.c.KeyExpires.IsZero() {
		days := int(d.c.KeyExpires.Sub(d.c.Now()).Hours() / 24)
		out.KeyExpiresAt, out.KeyExpiresInDays = d.c.KeyExpires.UTC().Format(time.RFC3339), &days
		out.KeyWarning = days < keyWarningDays
	}
	return out
}

func (d *DaemonResolver) askReadiness(ctx context.Context) Readiness {
	unknown := func(daemon, code string, why error) Readiness {
		d.c.Logf("forge-tts: readiness: %v", why)
		r := Readiness{Daemon: daemon, Code: code, Capabilities: make([]ReadinessCapability, len(readinessCapabilities))}
		for i, c := range readinessCapabilities {
			c.State, c.Code = "unknown", code
			r.Capabilities[i] = c
		}
		return r
	}
	cctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, d.c.BaseURL+"/api/v1/speech/readiness", nil)
	if err != nil {
		return unknown(DaemonUnreachable, codeDaemonUnreachable, err)
	}
	req.Header.Set("Authorization", "Bearer "+d.c.Key)
	resp, err := d.c.Client.Do(req)
	if err != nil {
		return unknown(DaemonUnreachable, codeDaemonUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body has been read or is being discarded: a close error changes nothing
	status := fmt.Errorf("the daemon answered HTTP %d", resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return unknown(DaemonKeyRefused, codeResolverUnauthorized, status)
	case http.StatusNotFound, http.StatusForbidden:
		// An older daemon has no such route, and refuses a speech key any path it does not list. A key that is really refused fails the next request too,
		// and says so there.
		return unknown(DaemonUnsupported, codeReadinessUnsupported, status)
	default:
		return unknown(DaemonUnreachable, codeDaemonUnreachable, status)
	}
	var body Readiness
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || len(body.Capabilities) == 0 {
		return unknown(DaemonUnreachable, codeDaemonUnreachable, fmt.Errorf("the daemon's answer could not be read: %v", err))
	}
	body.Daemon = DaemonOK
	return body
}

// SetReadinessReporter turns on GET /v1/readiness.
func (s *Server) SetReadinessReporter(r ReadinessReporter) { s.readiness = r }

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.readiness == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, s.readiness.Readiness(r.Context()))
}

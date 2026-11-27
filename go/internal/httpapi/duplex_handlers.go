// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/specialist"
)

// Duplex engine resolution (ADR-0029). forge-duplex asks which engine hosts a speech-to-speech session and gets its address and a lease; it renews the
// lease while the session lives and gives it back at close. These three calls are all a duplex-scoped key may reach. A session can outlive the lease
// TTL many times over (30 minutes against 10), so renewal is part of the contract, not an extra: a lease that stops being renewed expires by itself and
// the engine becomes evictable again.

// DuplexPool is the part of the pool the duplex endpoints use. *specialist.Manager implements it; the server asks the pool for it with a type
// assertion so the SpecialistPool interface and its fakes stay as they are.
type DuplexPool interface {
	ResolveDuplex(ctx context.Context, name string) (specialist.Target, error)
	RenewLease(id string) bool
	ReleaseSpeech(id string) bool // releases any lease; the name is historical
}

func (s *Server) duplexPool(w http.ResponseWriter) (DuplexPool, bool) {
	if s.deps.Specialists != nil {
		if dp, ok := s.deps.Specialists.(DuplexPool); ok {
			return dp, true
		}
	}
	writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "duplex engine pool"}, "the engine pool is not running on this server")
	return nil, false
}

// duplexResolveBudget bounds one resolve, an engine start included (a cold Haru start is about a minute; the longest launch health timeout is 120 s).
const duplexResolveBudget = 3 * time.Minute

type duplexResolveBody struct {
	// Engine names the pool member to use; empty means the default for the duplex function.
	Engine string `json:"engine"`
}

// handleDuplexResolve picks the engine for one duplex session, loads it if needed and takes a lease on it.
func (s *Server) handleDuplexResolve(w http.ResponseWriter, r *http.Request) {
	dp, ok := s.duplexPool(w)
	if !ok {
		return
	}
	var b duplexResolveBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if len(b.Engine) > 128 {
		writeValidationErrorCodes(w, map[string]string{"engine": "too long (at most 128 characters)"}, map[string]string{"engine": "too_large"})
		return
	}
	// The start runs on its own context for the reason the speech resolve does: a caller that gives up abandons the wait, not a nearly finished load.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), duplexResolveBudget)
	defer cancel()
	t, err := dp.ResolveDuplex(ctx, b.Engine)
	if err == nil && r.Context().Err() != nil {
		dp.ReleaseSpeech(t.LeaseID) // nobody is left to use the lease: do not pin the engine for its whole life
		return
	}
	if err != nil {
		if isHardDuplexError(err) {
			s.audit(r, identity(r).Name, "duplex_resolve_failed", b.Engine, err.Error())
		}
		s.writeDuplexError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "duplex_resolve", t.Occupant, t.Slot)
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": t.Occupant, "slot": t.Slot, "base_url": t.BaseURL, "lease_id": t.LeaseID, "expires_at": isoFormat(t.Expires),
	})
}

type duplexLeaseBody struct {
	LeaseID string `json:"lease_id"`
}

// handleDuplexRenew pushes a lease a full TTL ahead. 410 means the lease is gone (released, expired, or the engine was stopped and started again): the
// caller must end the session, because the engine under it is no longer protected.
func (s *Server) handleDuplexRenew(w http.ResponseWriter, r *http.Request) {
	dp, ok := s.duplexPool(w)
	if !ok {
		return
	}
	var b duplexLeaseBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.LeaseID == "" {
		writeValidationErrorCodes(w, map[string]string{"lease_id": "required"}, map[string]string{"lease_id": "required"})
		return
	}
	if !dp.RenewLease(b.LeaseID) {
		writeErrorCode(w, http.StatusGone, "duplex_lease_gone", map[string]any{"transient": false}, "the lease is gone: it was released, expired, or its engine was restarted")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDuplexRelease gives a lease back. Idempotent: an unknown or already released lease is not an error, since the caller can do nothing about it.
func (s *Server) handleDuplexRelease(w http.ResponseWriter, r *http.Request) {
	dp, ok := s.duplexPool(w)
	if !ok {
		return
	}
	var b duplexLeaseBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.LeaseID == "" {
		writeValidationErrorCodes(w, map[string]string{"lease_id": "required"}, map[string]string{"lease_id": "required"})
		return
	}
	dp.ReleaseSpeech(b.LeaseID)
	w.WriteHeader(http.StatusNoContent)
}

// isHardDuplexError reports whether retrying cannot change the answer.
func isHardDuplexError(err error) bool {
	var unavail *specialist.UnavailableError
	if errors.Is(err, specialist.ErrNoMatch) || errors.Is(err, specialist.ErrNotFound) {
		return true
	}
	return errors.As(err, &unavail) && (unavail.Reason == specialist.ReasonDisabled || unavail.Reason == specialist.ReasonStopped)
}

// writeDuplexError maps the pool's errors onto stable codes. The split that matters to forge-duplex is transient (loading, a full pool, a request that
// timed out: try again shortly) versus hard (no such engine, disabled, stopped: retrying changes nothing); `transient` is in the body as well as the code.
func (s *Server) writeDuplexError(w http.ResponseWriter, err error) {
	var unavail *specialist.UnavailableError
	switch {
	case errors.Is(err, specialist.ErrNoMatch):
		writeErrorCode(w, http.StatusServiceUnavailable, "duplex_no_engine", map[string]any{"reason": "no engine is named and none is the default for duplex", "transient": false}, err.Error())
	case errors.Is(err, specialist.ErrNotFound):
		writeErrorCode(w, http.StatusServiceUnavailable, "duplex_no_engine", map[string]any{"reason": "no such engine", "transient": false}, err.Error())
	case errors.As(err, &unavail) && (unavail.Reason == specialist.ReasonDisabled || unavail.Reason == specialist.ReasonStopped):
		writeErrorCode(w, http.StatusServiceUnavailable, "duplex_engine_off", map[string]any{"engine": unavail.Occupant, "reason": unavail.Reason, "transient": false}, err.Error())
	case errors.Is(err, specialist.ErrTooManyLeases):
		w.Header().Set("Retry-After", "2")
		writeErrorCode(w, http.StatusServiceUnavailable, "duplex_lease_limit", map[string]any{"transient": true}, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "5")
		writeErrorCode(w, http.StatusServiceUnavailable, "specialist_unavailable",
			map[string]any{"reason": "the request was cancelled or took too long while the engine was starting", "transient": true}, err.Error())
	default:
		s.writeSpecialistError(w, err) // pool full, unavailable (still starting), internal
	}
}

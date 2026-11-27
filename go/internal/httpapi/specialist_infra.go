// SPDX-License-Identifier: Apache-2.0

package httpapi

// specialist_infra.go: the fixed infra rows (STT, Embedding, Aligner) follow the specialist pool once a pool member answers their function
// (ADR-0022). Before this, those rows were tied to the old standalone units and ports, so stopping an old unit made a healthy service show as
// down, and the row's start/stop buttons drove a unit nothing used any more.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// fixedInfraFunction maps a fixed infra service to the pool function that replaces it.
var fixedInfraFunction = map[string]string{
	"stt":       store.FnTranscription,
	"embedding": store.FnEmbeddings,
	"aligner":   store.FnAlignment,
}

// poolBacking describes the pool member that answers a fixed service's function.
type poolBacking struct {
	Occupant string
	Slot     string // "" when the member is registered but not in a slot
	Unit     string
	Port     int // 0 when unknown (not loaded and the kind has no fixed port)
	State    string
	Err      string
}

// poolBackingFor returns the pool member behind function when the pool is running and has a default for it.
func (s *Server) poolBackingFor(ctx context.Context, function string) (poolBacking, bool) {
	if s.deps.Specialists == nil || s.deps.SpecialistStore == nil {
		return poolBacking{}, false
	}
	defaults, err := s.deps.SpecialistStore.Defaults(ctx)
	if err != nil || defaults[function] == "" {
		return poolBacking{}, false
	}
	b := poolBacking{Occupant: defaults[function], State: specialist.StateEmpty}
	if o, err := s.deps.SpecialistStore.OccupantByName(ctx, b.Occupant); err == nil {
		if k, ok := specialist.KindByName(o.Kind); ok {
			b.Port = k.FixedPort
		}
	}
	for _, st := range s.deps.Specialists.Snapshot(ctx) {
		if st.Occupant != b.Occupant {
			continue
		}
		b.Slot, b.State, b.Err = st.Slot, st.State, st.Err
		if b.Port == 0 {
			b.Port = st.Port
		}
		if s.deps.Config != nil {
			b.Unit = s.deps.Config().SpecialistSlots[st.Slot].Unit
		}
		break
	}
	return b, true
}

// poolInfraRow is the infra row for a fixed service whose function the pool answers.
func poolInfraRow(name string, logo *string, b poolBacking) infraService {
	row := infraService{Name: name, Active: b.State == specialist.StateRunning, Kind: "systemd", Logo: logo}
	if b.Unit != "" {
		row.Unit = ptrString(b.Unit)
	}
	if b.Port != 0 {
		p := b.Port
		row.Port = &p
	}
	detail := "specialist pool"
	if b.Slot != "" {
		detail += " · " + b.Slot
	}
	detail += " · " + b.Occupant
	switch {
	case b.State == specialist.StateFailed && b.Err != "":
		detail += " · failed: " + b.Err
	case b.State != specialist.StateRunning:
		detail += " · " + b.State
	}
	row.Detail = ptrString(detail)
	return row
}

// poolFixedInfraOp runs a start or stop of a fixed service through the pool when the pool answers its function. handled=false means the
// service is not pool-backed and the caller drives the old unit as before.
func (s *Server) poolFixedInfraOp(w http.ResponseWriter, r *http.Request, name string, start bool) (handled bool) {
	fn, ok := fixedInfraFunction[name]
	if !ok {
		return false
	}
	b, ok := s.poolBackingFor(r.Context(), fn)
	if !ok {
		return false
	}
	verb := "stop"
	var err error
	if start {
		verb = "start"
		_, err = s.deps.Specialists.Load(r.Context(), b.Occupant, b.Slot)
	} else if b.Slot != "" {
		// A client that gives up must not abort a stop half-way: run it on a context detached from the request, bounded like the unload endpoint.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), specialistUnloadBudget)
		defer cancel()
		err = s.deps.Specialists.Unload(stopCtx, b.Slot)
	}
	if err != nil {
		s.writeSpecialistError(w, err)
		return true
	}
	s.audit(r, identity(r).Name, name+"_"+verb, name, "specialist pool: "+b.Occupant)
	s.probeAndPush()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": fmt.Sprintf("%s %sed (specialist pool)", b.Occupant, verb)})
	return true
}

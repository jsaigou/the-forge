// SPDX-License-Identifier: Apache-2.0

package httpapi

// Specialist slot pool API (ADR-0022, slice 6). Viewers read; everything that changes the pool needs the admin role, and changes to what the pool
// is made of (occupants, policy, defaults) also need the settings step-up, like the catalog writes. Loading and unloading a slot is the
// operator's direct action on a running thing and needs admin but no step-up.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// SpecialistPool is the runtime side of the pool (implemented by *specialist.Manager).
type SpecialistPool interface {
	Snapshot(ctx context.Context) []specialist.SlotState
	Load(ctx context.Context, name, slot string) (specialist.SlotState, error)
	Unload(ctx context.Context, slot string) error
	// Forget drops the runtime record of a deleted occupant that is not running.
	Forget(name string)
	// PooledConfig reports whether a catalog config is hosted by the pool (a config has one owner).
	PooledConfig(ctx context.Context, config string) bool
}

type specialistSlotJSON struct {
	Slot        string   `json:"slot"`
	Label       string   `json:"label"`
	Port        int      `json:"port"`
	Occupant    string   `json:"occupant"`
	Policy      string   `json:"policy"`
	Functions   []string `json:"functions"`
	State       string   `json:"state"`
	Error       string   `json:"error"`
	Attempts    int      `json:"attempts"`
	NextRetryAt string   `json:"next_retry_at"`
	IdleSeconds int      `json:"idle_seconds"`
	MemoryBytes int64    `json:"memory_bytes"`
}

type specialistOccupantJSON struct {
	Name        string            `json:"name"`
	Label       string            `json:"label"`
	Kind        string            `json:"kind"`
	Params      map[string]string `json:"params"`
	Functions   []string          `json:"functions"`
	MemoryBytes int64             `json:"memory_bytes"`
	Policy      string            `json:"policy"`
	HomeSlot    string            `json:"home_slot"`
	Enabled     bool              `json:"enabled"`
	Stopped     bool              `json:"stopped"`
	Notes       string            `json:"notes"`
}

func occupantJSON(o store.Occupant) specialistOccupantJSON {
	return specialistOccupantJSON{Name: o.Name, Label: o.Label, Kind: o.Kind, Params: o.Params, Functions: o.Functions,
		MemoryBytes: o.MemoryBytes, Policy: o.Policy, HomeSlot: o.HomeSlot, Enabled: o.Enabled, Stopped: o.Stopped, Notes: o.Notes}
}

func (j specialistOccupantJSON) toStore() store.Occupant {
	return store.Occupant{Name: j.Name, Label: j.Label, Kind: j.Kind, Params: j.Params, Functions: j.Functions,
		MemoryBytes: j.MemoryBytes, Policy: j.Policy, HomeSlot: j.HomeSlot, Enabled: j.Enabled, Notes: j.Notes}
}

func (s *Server) specialistWired(w http.ResponseWriter) bool {
	if s.deps.SpecialistStore == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "specialist pool"}, "specialist pool not wired")
		return false
	}
	return true
}

// specialistWritable is specialistWired for the routes that write: they also need the rules module's store writer.
func (s *Server) specialistWritable(w http.ResponseWriter) bool {
	if !s.specialistWired(w) {
		return false
	}
	if s.deps.SpecialistWriter == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "specialist pool"}, "specialist pool not wired for writes")
		return false
	}
	return true
}

// occupants builds the occupant rules module from the live dependencies. Every write to an occupant or a default goes through it.
func (s *Server) occupants() *specialist.Occupants {
	m := &specialist.Occupants{
		Read: s.deps.SpecialistStore, Write: s.deps.SpecialistWriter, Catalog: s.deps.SpecialistCatalog,
		ModelRoots: s.specialistModelRoots, UnloadWait: specialistUnloadWait, UnloadBudget: specialistUnloadBudget,
	}
	if s.deps.Specialists != nil {
		m.Pool = s.deps.Specialists
	}
	if s.deps.Config != nil {
		m.HomeSlots = func() map[string]bool {
			slots := map[string]bool{}
			for name := range s.deps.Config().SpecialistSlots {
				slots[name] = true
			}
			return slots
		}
	}
	return m
}

// writeOccupantError maps the rules module's errors onto the API's stable codes.
func (s *Server) writeOccupantError(w http.ResponseWriter, err error) {
	var ve *specialist.ValidationError
	var ce *specialist.ConflictError
	var pe *specialist.PoolError
	switch {
	case errors.As(err, &ve):
		if ve.Codes != nil {
			writeValidationErrorCodes(w, ve.Fields, ve.Codes)
		} else {
			writeValidationError(w, ve.Fields)
		}
	case errors.As(err, &ce):
		details := ce.Params
		if details == nil && ce.Resource != "" {
			details = map[string]any{"resource": ce.Resource}
		}
		writeErrorCode(w, http.StatusConflict, ce.Code, details, ce.Message)
	case errors.Is(err, specialist.ErrAlreadyExists):
		writeErrorCode(w, http.StatusConflict, "already_exists", nil, "an occupant with this name already exists")
	case errors.Is(err, store.ErrNotFound), errors.Is(err, specialist.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "specialist occupant"}, "no such occupant")
	case errors.As(err, &pe):
		s.writeSpecialistError(w, pe.Err)
	default:
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
	}
}

func (s *Server) specialistModelRoots() []string {
	if s.deps.Config == nil {
		return nil
	}
	return specialist.ModelRoots(s.deps.Config().Paths.ModelsDir, s.speechRoot())
}

// handleSpecialistsGet returns the whole pool: slot states, occupants, defaults, and what the registry offers.
func (s *Server) handleSpecialistsGet(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	ctx := r.Context()
	occs, err := s.deps.SpecialistStore.ListOccupants(ctx)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	defaults, err := s.deps.SpecialistStore.Defaults(ctx)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	slots := []specialistSlotJSON{}
	if s.deps.Specialists != nil {
		now := time.Now()
		for _, st := range s.deps.Specialists.Snapshot(ctx) {
			j := specialistSlotJSON{Slot: st.Slot, Port: st.Port, Occupant: st.Occupant, Policy: st.Policy, Functions: st.Functions, State: st.State,
				Error: st.Err, Attempts: st.Attempts, MemoryBytes: st.MemoryBytes}
			if j.Functions == nil {
				j.Functions = []string{}
			}
			if s.deps.Config != nil {
				j.Label = s.deps.Config().SpecialistSlots[st.Slot].Label
			}
			if !st.NextRetry.IsZero() && st.State == specialist.StateFailed {
				j.NextRetryAt = isoFormat(st.NextRetry)
			}
			if st.State == specialist.StateRunning && !st.LastUse.IsZero() {
				j.IdleSeconds = int(now.Sub(st.LastUse).Seconds())
			}
			slots = append(slots, j)
		}
	}
	out := make([]specialistOccupantJSON, 0, len(occs))
	for _, o := range occs {
		out = append(out, occupantJSON(o))
	}
	kinds := []map[string]any{}
	for _, name := range specialist.KindNames() {
		k, _ := specialist.KindByName(name)
		params := map[string]bool{}
		for p, spec := range k.Params {
			params[p] = spec.Required
		}
		kinds = append(kinds, map[string]any{"name": name, "fixed_port": k.FixedPort, "params": params})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"slots": slots, "occupants": out, "defaults": defaults, "functions": store.SpecialistFunctions, "kinds": kinds,
	})
}

type specialistLoadBody struct {
	Occupant string `json:"occupant"`
	Slot     string `json:"slot"`
}

func (s *Server) handleSpecialistLoad(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	var b specialistLoadBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Occupant == "" {
		writeValidationErrorCodes(w, map[string]string{"occupant": "required"}, map[string]string{"occupant": "required"})
		return
	}
	if s.deps.Specialists == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "specialist pool"}, "specialist pool not running (no specialist slots configured)")
		return
	}
	st, err := s.deps.Specialists.Load(r.Context(), b.Occupant, b.Slot)
	if err != nil {
		s.writeSpecialistError(w, err)
		return
	}
	s.audit(r, "human", "specialist_load", b.Occupant, st.Slot)
	writeJSON(w, http.StatusOK, map[string]any{"slot": st.Slot, "occupant": st.Occupant, "state": st.State})
}

// specialistUnloadWait is how long an unload request waits for the stop to finish before answering 202 "stopping". It stays well under the UI's
// 30 s HTTP timeout: stopping a member that is mid-generation can take up to ~75 s (TimeoutStopSec=60 plus the manager's margin).
var specialistUnloadWait = 10 * time.Second

// specialistUnloadBudget bounds the background stop itself (stopUnit gives up at 75 s; this is the outer guard for the follow-up writes).
const specialistUnloadBudget = 3 * time.Minute

type specialistUnloadBody struct {
	Slot string `json:"slot"`
}

func (s *Server) handleSpecialistUnload(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	var b specialistUnloadBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Slot == "" {
		writeValidationErrorCodes(w, map[string]string{"slot": "required"}, map[string]string{"slot": "required"})
		return
	}
	if s.deps.Specialists == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "specialist pool"}, "specialist pool not running (no specialist slots configured)")
		return
	}
	for _, st := range s.deps.Specialists.Snapshot(r.Context()) {
		if st.Slot == b.Slot && st.State == specialist.StateStopping {
			// Already stopping (the first unload is finishing the job): say so, never a false "unloaded".
			writeJSON(w, http.StatusAccepted, map[string]any{"slot": b.Slot, "state": specialist.StateStopping})
			return
		}
	}
	finished, err := s.occupants().UnloadWithin(r.Context(), b.Slot, nil)
	if err != nil {
		writeErrorCode(w, http.StatusConflict, "operation_failed", nil, err.Error())
		return
	}
	s.audit(r, "human", "specialist_unload", b.Slot, "")
	if !finished {
		// Still stopping (a member mid-generation takes a while to wind down): nothing is abandoned, the slot shows "stopping" until done.
		writeJSON(w, http.StatusAccepted, map[string]any{"slot": b.Slot, "state": specialist.StateStopping})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slot": b.Slot, "state": "unloaded"})
}

// writeSpecialistError maps the pool's errors onto stable codes (the same ones a0 uses) so the UI and a0 clients read them alike.
func (s *Server) writeSpecialistError(w http.ResponseWriter, err error) {
	var full *specialist.PoolFullError
	var unavail *specialist.UnavailableError
	switch {
	case errors.As(err, &full):
		if full.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((full.RetryAfter+time.Second-1)/time.Second)))
		}
		writeErrorCode(w, http.StatusServiceUnavailable, "specialist_pool_full", nil, err.Error())
	case errors.As(err, &unavail):
		writeErrorCode(w, http.StatusServiceUnavailable, "specialist_unavailable", map[string]any{"reason": unavail.Reason}, err.Error())
	case errors.Is(err, specialist.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "specialist occupant"}, err.Error())
	default:
		writeErrorCode(w, http.StatusConflict, "operation_failed", nil, err.Error())
	}
}

func (s *Server) handleSpecialistOccupantCreate(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	var b specialistOccupantJSON
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	o := b.toStore()
	got, err := s.occupants().Create(r.Context(), o)
	if err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_occupant_create", o.Name, o.Kind+" "+o.Policy)
	writeJSON(w, http.StatusCreated, occupantJSON(got))
}

func (s *Server) handleSpecialistOccupantUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	name := r.PathValue("name")
	var b specialistOccupantJSON
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	o := b.toStore()
	got, err := s.occupants().Update(r.Context(), name, o)
	if err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_occupant_update", o.Name, o.Kind+" "+o.Policy)
	writeJSON(w, http.StatusOK, occupantJSON(got))
}

type specialistPolicyBody struct {
	Policy   string `json:"policy"`
	HomeSlot string `json:"home_slot"`
}

func (s *Server) handleSpecialistPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	name := r.PathValue("name")
	var b specialistPolicyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	got, err := s.occupants().SetPolicy(r.Context(), name, b.Policy, b.HomeSlot)
	if err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_policy", name, got.Policy+" "+got.HomeSlot)
	writeJSON(w, http.StatusOK, occupantJSON(got))
}

func (s *Server) handleSpecialistOccupantDelete(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	name := r.PathValue("name")
	if err := s.occupants().Delete(r.Context(), name); err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_occupant_delete", name, "")
	w.WriteHeader(http.StatusNoContent)
}

type specialistDefaultBody struct {
	Occupant string `json:"occupant"`
}

func (s *Server) handleSpecialistDefault(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	fn := r.PathValue("function")
	var b specialistDefaultBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	defaults, err := s.occupants().SetDefault(r.Context(), fn, b.Occupant)
	if err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_default", fn, b.Occupant)
	writeJSON(w, http.StatusOK, map[string]any{"defaults": defaults})
}

// Behavior values are the module's: the one vocabulary every specialist control uses (Specialists tab, Models page, Voice & Speech).

type specialistBehaviorBody struct {
	Behavior string `json:"behavior"` // always_on | on_demand | disabled
}

func (s *Server) handleSpecialistBehavior(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	name := r.PathValue("name")
	var b specialistBehaviorBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	res, err := s.occupants().SetBehavior(r.Context(), name, b.Behavior)
	if err != nil {
		s.writeOccupantError(w, err)
		return
	}
	s.audit(r, "human", "specialist_behavior", name, b.Behavior)
	if res.Stopping {
		writeJSON(w, http.StatusAccepted, struct {
			specialistOccupantJSON
			Slot  string `json:"slot"`
			State string `json:"state"`
		}{occupantJSON(res.Occupant), res.Slot, specialist.StateStopping})
		return
	}
	writeJSON(w, http.StatusOK, occupantJSON(res.Occupant))
}

// handleSpecialistSlotLog shows the last lines a slot's unit logged, redacted and capped, so "why did it fail" needs no journal access.
func (s *Server) handleSpecialistSlotLog(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	slot := r.PathValue("slot")
	if s.deps.Config == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "config"}, "config not available")
		return
	}
	sl, ok := s.deps.Config().SpecialistSlots[slot]
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "specialist slot"}, "no such slot")
		return
	}
	if s.deps.SlotLog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "slot log"}, "log view not available on this server")
		return
	}
	lines := 40
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && v > 0 && v <= 200 {
		lines = v
	}
	text := engine.RedactLog(s.deps.SlotLog(r.Context(), sl.Unit, lines), 8000)
	writeJSON(w, http.StatusOK, map[string]any{"slot": slot, "lines": text})
}

// Specialist slots are numbered s1..s12 on ports 8100..8111 (the verified-free block in docs/infrastructure.md).
const (
	maxSpecialistSlots = 12
	specialistBasePort = 8099 // slot sN listens on specialistBasePort+N
)

// handleSpecialistSlotAdd adds the next specialist slot (admin only) and reloads the live config so the pool sees it without a restart.
func (s *Server) handleSpecialistSlotAdd(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not available")
		return
	}
	name, port, err := s.addSpecialistSlot(r.Context())
	var refused *slotAddError
	if errors.As(err, &refused) {
		writeErrorCode(w, refused.status, refused.code, refused.params, refused.msg)
		return
	}
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	s.audit(r, "human", "specialist_slot_add", name, strconv.Itoa(port))
	writeJSON(w, http.StatusCreated, map[string]any{"slot": name, "port": port, "label": "S" + strconv.Itoa(port-specialistBasePort)})
}

// slotAddError is why no slot could be added, as the API says it.
type slotAddError struct {
	status int
	code   string
	params map[string]any
	msg    string
}

func (e *slotAddError) Error() string { return e.msg }

func slotRefused(msg string) error {
	return &slotAddError{status: http.StatusConflict, code: "operation_failed", msg: msg}
}

// addSpecialistSlot creates the next free specialist slot and reloads the live config. The caller records who asked.
func (s *Server) addSpecialistSlot(ctx context.Context) (string, int, error) {
	s.slotAddMu.Lock()
	defer s.slotAddMu.Unlock()
	all, err := s.deps.Catalog.ListSlots(ctx)
	if err != nil {
		return "", 0, err
	}
	taken := map[string]bool{}
	ports := map[int]bool{}
	for _, sl := range all {
		taken[sl.Name] = true
		ports[sl.Port] = true
	}
	n := 0
	for i := 1; i <= maxSpecialistSlots; i++ {
		if !taken["s"+strconv.Itoa(i)] {
			n = i
			break
		}
	}
	if n == 0 {
		return "", 0, &slotAddError{status: http.StatusConflict, code: "specialist_slot_limit", params: map[string]any{"max": maxSpecialistSlots},
			msg: "the specialist pool is at its limit"}
	}
	port := specialistBasePort + n
	if ports[port] {
		return "", 0, slotRefused("port " + strconv.Itoa(port) + " is already used by another slot")
	}
	name := "s" + strconv.Itoa(n)
	if _, err := s.deps.Catalog.CreateSlot(ctx, store.Slot{Name: name, Unit: "forge-pool@" + name, Port: port,
		Label: "S" + strconv.Itoa(n), SortOrder: n, Pool: store.PoolSpecialist}); err != nil {
		return "", 0, slotRefused(err.Error())
	}
	// CreateSlot ignores a name collision and answers with the existing row, so read it back: a slot some other writer created in the meantime
	// (the catalog API, say) must not be reported as ours.
	if got, err := s.deps.Catalog.SlotByName(ctx, name); err != nil || got.Port != port || got.Pool != store.PoolSpecialist {
		return "", 0, slotRefused("slot " + name + " was created by someone else at the same moment; try again")
	}
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	return name, port, nil
}

// handleSpecialistSlotRemove removes a specialist slot, only when nothing is in it and nothing treats it as a home.
func (s *Server) handleSpecialistSlotRemove(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not available")
		return
	}
	ctx := r.Context()
	name := r.PathValue("slot")
	sl, err := s.deps.Catalog.SlotByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sl.Pool != store.PoolSpecialist) {
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "specialist slot"}, "no such specialist slot")
		return
	}
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	if err := s.occupants().SlotRemovable(ctx, name); err != nil {
		s.writeOccupantError(w, err)
		return
	}
	if err := s.deps.Catalog.DeleteSlot(ctx, sl.ID); err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "internal", nil, err.Error())
		return
	}
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	s.audit(r, "human", "specialist_slot_remove", name, "")
	w.WriteHeader(http.StatusNoContent)
}

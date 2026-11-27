// SPDX-License-Identifier: Apache-2.0

package httpapi

// Speech engine setup (docs/speech-setup-plan.md sections 2.1, 2.3 and 4): which engines this server knows how to set up, what it has of each, and the
// one call that registers an engine whose files are already here. The rules live in internal/speechsetup; this file connects them to the same pool and
// settings writes the other endpoints use. Nothing here downloads, builds or starts anything: the pool's supervisor loads an always-on engine by itself.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/user"
	"strconv"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

// speechRoot is the directory the speech engines are installed under.
func (s *Server) speechRoot() string {
	if s.deps.SpeechRoot != "" {
		return s.deps.SpeechRoot
	}
	return specialist.EngineRoot
}

// installCommandPrefix is how an administrator runs this program as the user it runs as.
func installCommandPrefix() string {
	program, err := os.Executable()
	if err != nil {
		log.Printf("httpapi: speech setup: where this program is cannot be read: %v", err)
		program = "forge"
	}
	me, err := user.Current()
	if err != nil {
		log.Printf("httpapi: speech setup: the service user cannot be read: %v", err)
		return program
	}
	return "sudo -u " + me.Username + " " + program
}

type speechRecipesJSON struct {
	Root    string                     `json:"root"`
	Recipes []speechsetup.RecipeStatus `json:"recipes"`
}

func (s *Server) handleSpeechRecipes(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWired(w) {
		return
	}
	engines, err := s.deps.SpecialistStore.ListOccupants(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defaults, err := s.readSpeechDefaults(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	statuses := speechsetup.Statuses(s.speechRoot(), engines, defaults)
	prefix := installCommandPrefix()
	for i := range statuses {
		if statuses[i].Installable && statuses[i].State == speechsetup.RecipeAbsent {
			statuses[i].InstallCommand = prefix + " speech install " + statuses[i].ID
		}
	}
	writeJSON(w, http.StatusOK, speechRecipesJSON{Root: s.speechRoot(), Recipes: statuses})
}

type speechRegisterJSON struct {
	speechsetup.Result
	// RestartRequired: the speech address was recorded just now, and the a0 router reads it at the daemon's next start.
	RestartRequired bool `json:"restart_required"`
}

func (s *Server) handleSpeechRecipeRegister(w http.ResponseWriter, r *http.Request) {
	if !s.specialistWritable(w) {
		return
	}
	if s.deps.Catalog == nil || s.deps.Settings == nil || s.deps.Specialists == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "speech engine pool"}, "speech setup needs the engine pool, the catalog and the settings store")
		return
	}
	// One registration at a time: two of them would otherwise both find no engine and both try to add it.
	s.speechSetupMu.Lock()
	defer s.speechSetupMu.Unlock()
	id := r.PathValue("id")
	// A client that gives up must not leave a registration half done.
	ctx := context.WithoutCancel(r.Context())
	host := &speechHost{s: s, r: r}
	res, err := speechsetup.Register(ctx, host, s.speechRoot(), id)
	if res.SlotAdded != "" || res.Created {
		s.audit(r, identity(r).Name, "speech_engine_register", id, res.Engine+" slot_added="+res.SlotAdded)
	}
	if err != nil {
		s.writeSpeechSetupError(w, id, err)
		return
	}
	s.probeAndPush()
	writeJSON(w, http.StatusOK, speechRegisterJSON{Result: res, RestartRequired: res.FrontDoorSet})
}

func (s *Server) writeSpeechSetupError(w http.ResponseWriter, id string, err error) {
	var missing *speechsetup.FilesMissingError
	var taken *speechsetup.NameTakenError
	var full *slotAddError
	switch {
	case errors.Is(err, speechsetup.ErrUnknownRecipe):
		writeErrorCode(w, http.StatusNotFound, "speech_recipe_unknown", map[string]any{"recipe": id}, "no such speech engine recipe")
	case errors.As(err, &missing):
		writeErrorCode(w, http.StatusConflict, "speech_files_incomplete", map[string]any{"recipe": id, "count": len(missing.Problems), "first": missing.Problems[0].Path}, err.Error())
	case errors.As(err, &taken):
		writeErrorCode(w, http.StatusConflict, "speech_engine_name_taken", map[string]any{"engine": taken.Engine}, err.Error())
	case errors.As(err, &full):
		writeErrorCode(w, full.status, full.code, full.params, err.Error())
	default:
		s.writeOccupantError(w, err)
	}
}

// speechHost is speechsetup.Host over the daemon's own rules. r supplies the identity for the audit and restart records.
type speechHost struct {
	s *Server
	r *http.Request
}

func (h *speechHost) Engines(ctx context.Context) ([]store.Occupant, error) {
	return h.s.deps.SpecialistStore.ListOccupants(ctx)
}

func (h *speechHost) SpeechDefaults(ctx context.Context) (map[string]string, error) {
	return h.s.readSpeechDefaults(ctx)
}

func (h *speechHost) SetSpeechDefault(ctx context.Context, mode, engine string) error {
	defaults, err := h.s.readSpeechDefaults(ctx)
	if err != nil {
		return err
	}
	defaults[mode] = engine
	raw, err := json.Marshal(defaults)
	if err != nil {
		return err
	}
	if err := h.s.deps.Settings.Set(ctx, specialist.SpeechDefaultsKey, raw); err != nil {
		return err
	}
	h.s.audit(h.r, identity(h.r).Name, "speech_defaults_update", specialist.SpeechDefaultsKey, string(raw))
	return nil
}

func (h *speechHost) Slots(ctx context.Context) ([]specialist.SlotState, error) {
	return h.s.deps.Specialists.Snapshot(ctx), nil
}

func (h *speechHost) AddSlot(ctx context.Context) (string, error) {
	name, port, err := h.s.addSpecialistSlot(ctx)
	if err == nil {
		h.s.audit(h.r, identity(h.r).Name, "specialist_slot_add", name, strconv.Itoa(port))
	}
	return name, err
}

func (h *speechHost) Create(ctx context.Context, o store.Occupant) error {
	_, err := h.s.occupants().Create(ctx, o)
	return err
}

func (h *speechHost) SetHome(ctx context.Context, engine, slot string) error {
	_, err := h.s.occupants().SetPolicy(ctx, engine, store.PolicyAlwaysOn, slot)
	return err
}

func (h *speechHost) EnsureFrontDoorURL(ctx context.Context, url string) (bool, error) {
	if h.s.resolvedRouterConfig(ctx).TTSURL != "" {
		return false, nil
	}
	value, err := json.Marshal(url)
	if err != nil {
		return false, err
	}
	merged, err := mergeSettingJSON(h.s.getRawSetting(ctx, "infra.router"), map[string]json.RawMessage{"tts_url": value})
	if err != nil {
		return false, err
	}
	if err := h.s.deps.Settings.Set(ctx, "infra.router", merged); err != nil {
		return false, err
	}
	h.s.markRestartRequired(h.r, "infra.router")
	h.s.audit(h.r, identity(h.r).Name, "router_config", "infra.router", string(merged))
	return true, nil
}

func (h *speechHost) FreeBytes() int64 {
	if h.s.deps.Engine == nil {
		return 0
	}
	b, err := h.s.deps.Engine.MemoryBudget()
	if err != nil {
		log.Printf("httpapi: speech setup: memory budget unreadable (%v); a new engine is registered on demand", err)
		return 0
	}
	return b.FreeBytes
}

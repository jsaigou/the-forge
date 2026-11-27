// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_removal.go — the guided removal of a model or config (store.CatalogRemovals):
//   GET  /api/v1/catalog/{models|configs}/{id}/removal  what would go, and what stands in the way
//   POST /api/v1/catalog/{models|configs}/{id}/remove   do it, optionally deleting the files
// The plain DELETE routes are unchanged: they remove one row and never touch a file.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

type removalPlanJSON struct {
	Kind              string               `json:"kind"`
	ID                int64                `json:"id"`
	Name              string               `json:"name"`
	ModelID           int64                `json:"model_id"`
	ModelName         string               `json:"model_name"`
	Configs           []removalConfigJSON  `json:"configs"`
	Variants          int                  `json:"variants"`
	Offerings         []string             `json:"offerings"`
	Aliases           []string             `json:"aliases"`
	Benchmarks        int                  `json:"benchmarks"`
	Notes             int                  `json:"notes"`
	LastConfigOfModel bool                 `json:"last_config_of_model"`
	Blockers          []removalBlockerJSON `json:"blockers"`
	Files             []removalFileJSON    `json:"files"`
	Token             string               `json:"token"`
}

type removalConfigJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type removalBlockerJSON struct {
	Code   string `json:"code"`
	Config string `json:"config"`
	Name   string `json:"name"`
}

type removalFileJSON struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	IsDir  bool   `json:"is_dir"`
	Delete bool   `json:"delete"`
	Keep   string `json:"keep"`
	KeptBy string `json:"kept_by"`
}

type removalBody struct {
	DeleteFiles bool   `json:"delete_files"`
	Token       string `json:"token"`
	Reason      string `json:"reason"`
}

type removalResultJSON struct {
	Ok           bool              `json:"ok"`
	FilesDeleted []string          `json:"files_deleted"`
	BytesFreed   int64             `json:"bytes_freed"`
	FileErrors   map[string]string `json:"file_errors"`
}

func removalPlanToJSON(p store.RemovalPlan) removalPlanJSON {
	out := removalPlanJSON{
		Kind: p.Kind, ID: p.ID, Name: p.Name, ModelID: p.ModelID, ModelName: p.ModelName,
		Configs: []removalConfigJSON{}, Variants: p.Variants, Offerings: p.Offerings, Aliases: p.Aliases,
		Benchmarks: p.Benchmarks, Notes: p.Notes, LastConfigOfModel: p.LastConfigOfModel,
		Blockers: []removalBlockerJSON{}, Files: []removalFileJSON{}, Token: p.Token,
	}
	for _, c := range p.Configs {
		out.Configs = append(out.Configs, removalConfigJSON{ID: c.ID, Name: c.Name})
	}
	for _, b := range p.Blockers {
		out.Blockers = append(out.Blockers, removalBlockerJSON{Code: b.Code, Config: b.Config, Name: b.Name})
	}
	for _, f := range p.Files {
		out.Files = append(out.Files, removalFileJSON{Path: f.Path, Bytes: f.Bytes, IsDir: f.IsDir, Delete: f.Delete, Keep: f.Keep, KeptBy: f.KeptBy})
	}
	return out
}

// catalogRemovals builds the removal module from the live dependencies.
func (s *Server) catalogRemovals() store.CatalogRemovals {
	return store.CatalogRemovals{Cat: s.deps.Catalog, Changed: s.invalidateCfg, Loaded: s.isConfigInUse, ModelsDir: s.modelsDir}
}

// isConfigInUse reports whether a slot is running the config, or is loading or unloading it right now.
func (s *Server) isConfigInUse(name string) bool {
	if s.isConfigLoaded(name) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.slotLoading {
		if st.inProgress && st.mode == name {
			return true
		}
	}
	for _, st := range s.slotUnloading {
		if st.inProgress && st.mode == name {
			return true
		}
	}
	return false
}

func (s *Server) handleCatalogRemovalPlan(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Catalog == nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
			return
		}
		id, ok := parseID(r)
		if !ok {
			writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
			return
		}
		ctx, cancel := catalogCtx(r)
		defer cancel()
		p, err := s.catalogRemovals().Plan(ctx, kind, id)
		if err != nil {
			writeRemovalError(w, kind, err)
			return
		}
		writeJSON(w, http.StatusOK, removalPlanToJSON(p))
	}
}

func (s *Server) handleCatalogRemove(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Catalog == nil {
			writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
			return
		}
		id, ok := parseID(r)
		if !ok {
			writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
			return
		}
		var b removalBody
		if fields := decodeCatalogBody(r, &b); fields != nil {
			writeValidationError(w, fields)
			return
		}
		// A narrowed key may edit the catalog, never the disk.
		if b.DeleteFiles && identity(r).Scope != "" {
			writeErrorCode(w, http.StatusForbidden, "removal_files_scoped_key", nil, "this key's scope does not allow deleting files")
			return
		}
		ctx, cancel := catalogCtx(r)
		defer cancel()
		res, err := s.catalogRemovals().Execute(ctx, kind, id, b.DeleteFiles, b.Token)
		if err != nil {
			writeRemovalError(w, kind, err)
			return
		}
		names := make([]string, 0, len(res.Plan.Configs))
		for _, c := range res.Plan.Configs {
			names = append(names, c.Name)
		}
		detail := res.Plan.Name + " — configs: " + strings.Join(names, ", ")
		if b.DeleteFiles {
			detail += "; files deleted: " + strings.Join(res.FilesDeleted, ", ")
			for path, why := range res.FileErrors {
				detail += "; could not delete " + path + ": " + why
			}
		}
		s.audit(r, identity(r).Name, "catalog_"+kind+"_remove", strconv.FormatInt(id, 10), withReason(detail, b.Reason))
		writeJSON(w, http.StatusOK, removalResultJSON{Ok: true, FilesDeleted: res.FilesDeleted, BytesFreed: res.BytesFreed, FileErrors: res.FileErrors})
	}
}

func writeRemovalError(w http.ResponseWriter, kind string, err error) {
	switch {
	case errors.Is(err, store.ErrRemovalBlocked):
		writeErrorCode(w, http.StatusConflict, "removal_blocked", nil, "something still depends on what would be removed")
	case errors.Is(err, store.ErrRemovalChanged):
		writeErrorCode(w, http.StatusConflict, "removal_plan_changed", nil, "the catalog or the files changed; review the removal again")
	case isNotFound(err):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": kind}, kind+" not found")
	default:
		writeInternalError(w, err)
	}
}

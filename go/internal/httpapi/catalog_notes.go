// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_notes.go — Note CRUD (split from catalog_handlers.go, Sprint 5
// code-quality cleanup, #33). Mutating routes are requireRole(admin) +
// requireAssurance(page.settings); see httpapi.go's route table.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type noteJSON struct {
	ID          int64  `json:"id"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func noteToJSON(n store.Note) noteJSON {
	return noteJSON{
		ID: n.ID, SubjectType: n.SubjectType, SubjectID: n.SubjectID,
		Author: n.Author, Body: n.Body,
		CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: n.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleCatalogNotesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []noteJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Note
	if st := r.URL.Query().Get("subject_type"); st != "" {
		sid, err := strconv.ParseInt(r.URL.Query().Get("subject_id"), 10, 64)
		if err != nil {
			writeValidationErrorCodes(w, map[string]string{"subject_id": "must be an integer"}, map[string]string{"subject_id": "must_be_integer"})
			return
		}
		list, err = cat.ListNotesForSubject(ctx, st, sid)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "notes query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListNotes(ctx)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "notes query failed")
			return
		}
	}
	out := make([]noteJSON, 0, len(list))
	for _, n := range list {
		out = append(out, noteToJSON(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogNoteGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
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
	n, err := cat.GetNote(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "note"}, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, noteToJSON(n))
}

func (s *Server) handleCatalogNoteCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b noteBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields, codes := validateNote(b); len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateNote(ctx, store.Note{
		SubjectType: b.SubjectType, SubjectID: b.SubjectID,
		Author: b.Author, Body: b.Body,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_create", strconv.FormatInt(id, 10), "")
	n, _ := cat.GetNote(ctx, id)
	writeJSON(w, http.StatusCreated, noteToJSON(n))
}

func (s *Server) handleCatalogNoteUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	var b noteBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields, codes := validateNote(b); len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateNote(ctx, store.Note{
		ID: id, SubjectType: b.SubjectType, SubjectID: b.SubjectID,
		Author: b.Author, Body: b.Body,
	})
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "note"}, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_update", strconv.FormatInt(id, 10), "")
	n, _ := cat.GetNote(ctx, id)
	writeJSON(w, http.StatusOK, noteToJSON(n))
}

func (s *Server) handleCatalogNoteDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
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
	if err := cat.DeleteNote(ctx, id); err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "note"}, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type noteBody struct {
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Author      string `json:"author"`
	Body        string `json:"body"`
}

// validateNote checks field constraints. The second return value is codes'
// i18n Phase 2 companion (field -> stable error code) — see
// writeValidationErrorCodes' doc comment in httpapi.go.
func validateNote(b noteBody) (map[string]string, map[string]string) {
	fields := map[string]string{}
	codes := map[string]string{}
	switch b.SubjectType {
	case "model", "config", "offering":
	default:
		fields["subject_type"] = "must be model, config, or offering"
		codes["subject_type"] = "must_be_one_of"
	}
	if b.SubjectID == 0 {
		fields["subject_id"] = "is required"
		codes["subject_id"] = "required"
	}
	if b.Body == "" {
		fields["body"] = "is required"
		codes["body"] = "required"
	}
	if b.Author == "" {
		fields["author"] = "is required"
		codes["author"] = "required"
	}
	return fields, codes
}

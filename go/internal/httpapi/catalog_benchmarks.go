// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_benchmarks.go — Benchmark CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type benchmarkJSON struct {
	ID          int64  `json:"id"`
	Metric      string `json:"metric"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	SourceURL   string `json:"source_url"`
	SourceDate  string `json:"source_date"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Notes       string `json:"notes"`
}

func benchmarkToJSON(b store.Benchmark) benchmarkJSON {
	return benchmarkJSON{
		ID: b.ID, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate, SubjectType: b.SubjectType,
		SubjectID: b.SubjectID, Notes: b.Notes,
	}
}

func (s *Server) handleCatalogBenchmarksList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []benchmarkJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Benchmark
	if st := r.URL.Query().Get("subject_type"); st != "" {
		sid, err := strconv.ParseInt(r.URL.Query().Get("subject_id"), 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"subject_id": "must be an integer"})
			return
		}
		list, err = cat.ListBenchmarksForSubject(ctx, st, sid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "benchmarks query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListBenchmarks(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "benchmarks query failed")
			return
		}
	}
	out := make([]benchmarkJSON, 0, len(list))
	for _, b := range list {
		out = append(out, benchmarkToJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogBenchmarkGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	b, err := cat.GetBenchmark(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, benchmarkToJSON(b))
}

func (s *Server) handleCatalogBenchmarkCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateBenchmark(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_create", strconv.FormatInt(id, 10), b.Metric)
	bm, _ := cat.GetBenchmark(ctx, id)
	writeJSON(w, http.StatusCreated, benchmarkToJSON(bm))
}

func (s *Server) handleCatalogBenchmarkUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	// Fetched before validation (not just after, as every other update
	// handler in this file does) so validateBenchmarkUpdate can grandfather
	// a pre-existing offering-scoped row — see its doc comment.
	existing, err := cat.GetBenchmark(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	if fields := validateBenchmarkUpdate(b, existing); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	err = cat.UpdateBenchmark(ctx, store.Benchmark{
		ID: id, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_update", strconv.FormatInt(id, 10), b.Metric)
	bm, _ := cat.GetBenchmark(ctx, id)
	writeJSON(w, http.StatusOK, benchmarkToJSON(bm))
}

func (s *Server) handleCatalogBenchmarkDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteBenchmark(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogBenchmarkValidate(w http.ResponseWriter, r *http.Request) {
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateBenchmark(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

type benchmarkBody struct {
	Metric      string `json:"metric"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	SourceURL   string `json:"source_url"`
	SourceDate  string `json:"source_date"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Notes       string `json:"notes"`
}

// validateBenchmark enforces the F7 fabrication-prevention gate (published
// requires source_url + source_date) + subject/value field constraints.
// subject_type is restricted to model/variant/config (Phase 8, pre-release
// feedback sprint): registry.go's loadSnapshot only ever indexed those
// three — an "offering"-scoped benchmark was validated, stored, and
// listable, but reached no card anywhere. See validateBenchmarkUpdate for
// why UPDATE still needs to accept "offering" in one narrow case.
func validateBenchmark(b benchmarkBody) map[string]string {
	return validateBenchmarkFields(b, false)
}

// validateBenchmarkUpdate grandfathers a pre-existing offering-scoped row:
// a PUT that leaves an already-offering-scoped row's subject untouched must
// still succeed, or that row becomes permanently unsavable through the only
// UI that can reach it (the form can no longer even represent "offering" as
// a selectable value once new ones are rejected, so every edit attempt
// would either silently re-home the row to a different subject or fail).
// Re-scoping an offering row TO model/variant/config is unaffected — it
// already goes through the strict path since the new subject isn't
// "offering". Create never grandfathers; only PUT has an existing row to
// compare against.
func validateBenchmarkUpdate(b benchmarkBody, existing store.Benchmark) map[string]string {
	grandfathered := existing.SubjectType == "offering" &&
		b.SubjectType == "offering" && b.SubjectID == existing.SubjectID
	return validateBenchmarkFields(b, grandfathered)
}

func validateBenchmarkFields(b benchmarkBody, allowExistingOffering bool) map[string]string {
	fields := map[string]string{}
	if b.Metric == "" {
		fields["metric"] = "is required"
	}
	if b.Value == "" {
		fields["value"] = "is required"
	}
	switch b.Source {
	case "published", "self_measured", "provider_reported":
	default:
		fields["source"] = "must be published, self_measured, or provider_reported"
	}
	// F7 gate: published requires source_url + source_date.
	if b.Source == "published" {
		if b.SourceURL == "" {
			fields["source_url"] = "is required when source is 'published'"
		}
		if b.SourceDate == "" {
			fields["source_date"] = "is required when source is 'published'"
		}
	}
	switch b.SubjectType {
	case "model", "variant", "config":
	case "offering":
		if !allowExistingOffering {
			fields["subject_type"] = "must be model, variant, or config"
		}
	default:
		fields["subject_type"] = "must be model, variant, or config"
	}
	if b.SubjectID == 0 {
		fields["subject_id"] = "is required"
	}
	if b.SourceDate != "" {
		if _, err := time.Parse("2006-01-02", b.SourceDate); err != nil {
			fields["source_date"] = "must be YYYY-MM-DD"
		}
	}
	return fields
}

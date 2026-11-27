// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_benchmarks.go — Benchmark CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/launch"
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
	// Tuple is the launch tuple a tuple-bound figure was measured at (launch.MemoryTuple JSON); omitted for every other benchmark.
	Tuple string `json:"tuple,omitempty"`
}

func benchmarkToJSON(b store.Benchmark) benchmarkJSON {
	return benchmarkJSON{
		ID: b.ID, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate, SubjectType: b.SubjectType,
		SubjectID: b.SubjectID, Notes: b.Notes, Tuple: b.Tuple,
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
			writeValidationErrorCodes(w, map[string]string{"subject_id": "must be an integer"}, map[string]string{"subject_id": "must_be_integer"})
			return
		}
		list, err = cat.ListBenchmarksForSubject(ctx, st, sid)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "benchmarks query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListBenchmarks(ctx)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "benchmarks query failed")
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
	b, err := cat.GetBenchmark(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "benchmark"}, "benchmark not found")
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
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b benchmarkBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	fields, codes := validateBenchmark(b)
	if err := s.validateBenchmarkTuple(ctx, b, fields, codes); err != nil {
		writeInternalError(w, err)
		return
	}
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	id, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes, Tuple: b.Tuple,
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
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	var b benchmarkBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
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
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "benchmark"}, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	fields, codes := validateBenchmarkUpdate(b, existing)
	if err := s.validateBenchmarkTuple(ctx, b, fields, codes); err != nil {
		writeInternalError(w, err)
		return
	}
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
		return
	}
	err = cat.UpdateBenchmark(ctx, store.Benchmark{
		ID: id, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes, Tuple: b.Tuple,
	})
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "benchmark"}, "benchmark not found")
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
	if err := cat.DeleteBenchmark(ctx, id); err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "benchmark"}, "benchmark not found")
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
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	fields, codes := validateBenchmark(b)
	if id := validateExcludeID(r); id != 0 && s.deps.Catalog != nil {
		ctx, cancel := catalogCtx(r)
		defer cancel()
		existing, err := s.deps.Catalog.GetBenchmark(ctx, id)
		if err != nil {
			if isNotFound(err) {
				writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "benchmark"}, "benchmark not found")
				return
			}
			writeInternalError(w, err)
			return
		}
		fields, codes = validateBenchmarkUpdate(b, existing)
	}
	{
		ctx, cancel := catalogCtx(r)
		defer cancel()
		if err := s.validateBenchmarkTuple(ctx, b, fields, codes); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if len(fields) > 0 {
		writeValidationErrorCodes(w, fields, codes)
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
	Tuple       string `json:"tuple"`
}

// validateBenchmark enforces the F7 fabrication-prevention gate (published
// requires source_url + source_date) + subject/value field constraints.
// subject_type is restricted to model/variant/config (Phase 8, pre-release
// feedback sprint): registry.go's loadSnapshot only ever indexed those
// three — an "offering"-scoped benchmark was validated, stored, and
// listable, but reached no card anywhere. See validateBenchmarkUpdate for
// why UPDATE still needs to accept "offering" in one narrow case.
func validateBenchmark(b benchmarkBody) (map[string]string, map[string]string) {
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
func validateBenchmarkUpdate(b benchmarkBody, existing store.Benchmark) (map[string]string, map[string]string) {
	grandfathered := existing.SubjectType == "offering" &&
		b.SubjectType == "offering" && b.SubjectID == existing.SubjectID
	return validateBenchmarkFields(b, grandfathered)
}

// validateBenchmarkFields' second return value is codes' i18n Phase 2
// companion (field -> stable error code). The source/subject_type enum
// messages and the source_url/source_date "required when published"
// messages are deliberately left without a code: must_be_one_of would
// render the frontend's raw unresolved "{{allowed}}" template
// (writeValidationErrorCodes' codes map has no per-field params channel —
// see validateConfig's doc comment in catalog_configs.go for the same
// constraint), and the "required when published" qualifier carries meaning
// the bare `required` code's generic text would lose.
func validateBenchmarkFields(b benchmarkBody, allowExistingOffering bool) (map[string]string, map[string]string) {
	fields := map[string]string{}
	codes := map[string]string{}
	if b.Metric == "" {
		fields["metric"] = "is required"
		codes["metric"] = "required"
	}
	if b.Value == "" {
		fields["value"] = "is required"
		codes["value"] = "required"
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
		codes["subject_id"] = "required"
	}
	if b.SourceDate != "" {
		if _, err := time.Parse("2006-01-02", b.SourceDate); err != nil {
			fields["source_date"] = "must be YYYY-MM-DD"
		}
	}
	return fields, codes
}

// maxBenchmarkTupleBytes bounds a stored tuple (a handful of ids and flags).
const maxBenchmarkTupleBytes = 4096

// validateBenchmarkTuple adds tuple errors to fields/codes. A tuple, when present, must parse as a launch.MemoryTuple (unknown fields refused) and
// belongs only to a config-scoped safe_memory_bytes figure. A safe_memory_bytes figure for a config whose engine kind has Spec.NeedsCuratedMemory
// MUST carry one: without it the fit gate could never match the figure to a launch, and a figure that looks registered but is ignored is a trap. The
// returned error is a catalog read failure (the check could not be made), never a validation result.
func (s *Server) validateBenchmarkTuple(ctx context.Context, b benchmarkBody, fields, codes map[string]string) error {
	if b.Tuple != "" {
		switch {
		case len(b.Tuple) > maxBenchmarkTupleBytes:
			fields["tuple"] = "is too large"
		case b.Metric != "safe_memory_bytes" || b.SubjectType != "config":
			fields["tuple"] = "applies only to a config-scoped safe_memory_bytes benchmark"
		default:
			if _, err := launch.ParseMemoryTuple(b.Tuple); err != nil {
				fields["tuple"] = err.Error()
			}
		}
	}
	if b.Metric != "safe_memory_bytes" || b.Tuple != "" || s.deps.Catalog == nil {
		return nil
	}
	kind, err := s.benchmarkSubjectKind(ctx, b)
	if err != nil || kind == "" {
		return err
	}
	if spec, serr := enginekind.Lookup(kind); serr == nil && spec.NeedsCuratedMemory {
		fields["tuple"], codes["tuple"] = "is required: this engine's memory depends on its launch tuple, so the figure must record the tuple it was measured at", "required"
	}
	return nil
}

// benchmarkSubjectKind is the engine kind of the config a config-scoped benchmark describes ("" for any other subject). A subject that does not
// exist is left to the existing rules (no error here).
func (s *Server) benchmarkSubjectKind(ctx context.Context, b benchmarkBody) (string, error) {
	if b.SubjectType != "config" || b.SubjectID == 0 {
		return "", nil
	}
	cfg, err := s.deps.Catalog.GetConfig(ctx, b.SubjectID)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	engines, err := s.deps.Catalog.ListEngines(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range engines {
		if e.ID == cfg.EngineID {
			return e.Kind, nil
		}
	}
	return "", nil
}

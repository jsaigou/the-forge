// SPDX-License-Identifier: Apache-2.0

package httpapi

// hf_handlers.go — HF model-acquisition API (go/internal/hfdownload):
// search, recursive file tree, pre-flight validation, and the download
// job queue (start/approve/pause/resume/cancel + reads). See httpapi.go's
// route table for the exact role/step-up gating on each route.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/store"
)

// intQueryParam parses an integer query parameter, returning def on a
// missing or malformed value.
func intQueryParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func (s *Server) hfOK(w http.ResponseWriter) bool {
	if s.deps.HFDownload == nil {
		writeError(w, http.StatusServiceUnavailable, "hf model acquisition not wired")
		return false
	}
	return true
}

func hfCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

// ── search / tree ────────────────────────────────────────────────────────

type hfSearchResultJSON struct {
	ID           string   `json:"id"`
	Author       string   `json:"author"`
	Downloads    int64    `json:"downloads"`
	Likes        int64    `json:"likes"`
	Tags         []string `json:"tags"`
	Gated        bool     `json:"gated"`
	PipelineTag  string   `json:"pipeline_tag"`
	LastModified string   `json:"last_modified"`
	// NoGGUF marks the synthetic "true publisher, no compatible GGUF"
	// entry Search injects when nobody has quantized this family yet —
	// see hf.Model.NoGGUF's doc comment.
	NoGGUF bool `json:"no_gguf,omitempty"`
}

type hfSearchResponse struct {
	Results []hfSearchResultJSON `json:"results"`
}

// handleHFSearch — GET /api/v1/hf/search?q=&limit=
func (s *Server) handleHFSearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.HFClient == nil {
		writeError(w, http.StatusServiceUnavailable, "hf client not wired")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeValidationError(w, map[string]string{"q": "required"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	results, err := s.deps.HFClient.Search(ctx, hf.Query{Text: q, Limit: intQueryParam(r, "limit", 0)})
	if err != nil {
		writeError(w, http.StatusBadGateway, "hf search failed: "+err.Error())
		return
	}
	out := make([]hfSearchResultJSON, len(results))
	for i, m := range results {
		out[i] = hfSearchResultJSON{
			ID: m.ID, Author: m.Author, Downloads: m.Downloads, Likes: m.Likes,
			Tags: m.Tags, Gated: m.Gated, PipelineTag: m.PipelineTag, LastModified: m.LastModified,
			NoGGUF: m.NoGGUF,
		}
	}
	writeJSON(w, http.StatusOK, hfSearchResponse{Results: out})
}

type hfTreeResponse struct {
	Repo       string                `json:"repo"`
	Candidates []hf.QuantCandidate   `json:"candidates"`
	Recommended *hf.QuantCandidate   `json:"recommended,omitempty"`
}

// handleHFTree — GET /api/v1/hf/tree?repo=&revision=&budget_bytes=
// Returns ranked GGUF candidates (the same shape smith's sourcing/evaluate
// endpoint returns), but built from a full recursive tree read — the fix
// over that endpoint's root-of-main-only listing.
func (s *Server) handleHFTree(w http.ResponseWriter, r *http.Request) {
	if s.deps.HFClient == nil {
		writeError(w, http.StatusServiceUnavailable, "hf client not wired")
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo == "" {
		writeValidationError(w, map[string]string{"repo": "required"})
		return
	}
	revision := r.URL.Query().Get("revision")
	ctx, cancel := hfCtx(r)
	defer cancel()
	files, err := s.deps.HFClient.Tree(ctx, repo, revision)
	if err != nil {
		writeError(w, http.StatusBadGateway, "hf tree failed: "+err.Error())
		return
	}
	budget := int64(intQueryParam(r, "budget_bytes", 0))
	if budget == 0 && s.deps.Snapshots != nil {
		if snap := s.deps.Snapshots.Current(); snap != nil && snap.Metrics.GTTTotalBytes != nil {
			budget = *snap.Metrics.GTTTotalBytes
		}
	}
	candidates, rec := hf.RankCandidates(files, budget)
	writeJSON(w, http.StatusOK, hfTreeResponse{Repo: repo, Candidates: candidates, Recommended: rec})
}

// ── preflight ────────────────────────────────────────────────────────────

type hfPreflightBody struct {
	Repo    string                     `json:"repo"`
	Files   []hfdownload.PreflightFile `json:"files"`
	DestDir string                     `json:"dest_dir"`
}

// handleHFPreflight — POST /api/v1/hf/preflight
func (s *Server) handleHFPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	var b hfPreflightBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Repo == "" || len(b.Files) == 0 {
		writeValidationError(w, map[string]string{"repo": "required", "files": "at least one file is required"})
		return
	}
	if b.DestDir == "" {
		b.DestDir = hfdownload.DefaultDestDir(b.Repo, len(b.Files))
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	report, err := s.deps.HFDownload.Preflight(ctx, b.Repo, b.Files, b.DestDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, "preflight failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// ── downloads ────────────────────────────────────────────────────────────

type hfDownloadJSON struct {
	ID              int64                          `json:"id"`
	Repo            string                         `json:"repo"`
	Revision        string                         `json:"revision"`
	DestDir         string                         `json:"dest_dir"`
	ConfigName      string                         `json:"config_name,omitempty"`
	State           string                         `json:"state"`
	BytesDone       int64                          `json:"bytes_done"`
	BytesTotal      int64                          `json:"bytes_total"`
	Error           string                         `json:"error,omitempty"`
	Attempts        int                            `json:"attempts"`
	ProposedBy      string                         `json:"proposed_by,omitempty"`
	CreatedConfigID int64                          `json:"created_config_id,omitempty"`
	CreatedAt       int64                          `json:"created_at"`
	UpdatedAt       int64                          `json:"updated_at"`
	Files           []hfDownloadFileJSON           `json:"files,omitempty"`
}

type hfDownloadFileJSON struct {
	Filename    string `json:"filename"`
	DestRelPath string `json:"dest_rel_path"`
	BytesDone   int64  `json:"bytes_done"`
	BytesTotal  int64  `json:"bytes_total"`
	State       string `json:"state"`
}

func hfDownloadToJSON(d store.ModelDownloadRow, files []store.ModelDownloadFileRow) hfDownloadJSON {
	out := hfDownloadJSON{
		ID: d.ID, Repo: d.Repo, Revision: d.Revision, DestDir: d.DestDir, ConfigName: d.ConfigName,
		State: d.State, BytesDone: d.BytesDone, BytesTotal: d.BytesTotal, Error: d.Error,
		Attempts: d.Attempts, ProposedBy: d.ProposedBy, CreatedConfigID: d.CreatedConfigID,
		CreatedAt: d.CreatedAt.Unix(), UpdatedAt: d.UpdatedAt.Unix(),
	}
	for _, f := range files {
		out.Files = append(out.Files, hfDownloadFileJSON{
			Filename: f.Filename, DestRelPath: f.DestRelPath,
			BytesDone: f.BytesDone, BytesTotal: f.BytesTotal, State: f.State,
		})
	}
	return out
}

type hfDownloadsListResponse struct {
	Downloads []hfDownloadJSON `json:"downloads"`
}

// handleHFDownloadsList — GET /api/v1/hf/downloads
func (s *Server) handleHFDownloadsList(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	jobs, err := s.deps.HFDownload.List(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	out := make([]hfDownloadJSON, len(jobs))
	for i, j := range jobs {
		out[i] = hfDownloadToJSON(j, nil)
	}
	writeJSON(w, http.StatusOK, hfDownloadsListResponse{Downloads: out})
}

// handleHFDownloadGet — GET /api/v1/hf/downloads/{id}, files included.
func (s *Server) handleHFDownloadGet(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	job, err := s.deps.HFDownload.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "download not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	files, err := s.deps.HFDownload.ListFiles(ctx, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hfDownloadToJSON(job, files))
}

type hfDownloadStartBody struct {
	Repo       string                     `json:"repo"`
	Revision   string                     `json:"revision"`
	Files      []hfdownload.PreflightFile `json:"files"`
	DestDir    string                     `json:"dest_dir"`
	ConfigName string                     `json:"config_name"`
}

func (b hfDownloadStartBody) toRequest(actor string) hfdownload.CreateJobRequest {
	destDir := b.DestDir
	if destDir == "" {
		destDir = hfdownload.DefaultDestDir(b.Repo, len(b.Files))
	}
	return hfdownload.CreateJobRequest{
		Repo: b.Repo, Revision: b.Revision, Files: b.Files, DestDir: destDir,
		ConfigName: b.ConfigName, ProposedBy: actor,
	}
}

// handleHFDownloadStart — POST /api/v1/hf/downloads. The operator-initiated
// path: creates the job queued AND starts it immediately (no approval
// gate — that's specifically for smith's propose-only download_start
// tool, ApproveJob's route below).
func (s *Server) handleHFDownloadStart(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	var b hfDownloadStartBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Repo == "" || len(b.Files) == 0 {
		writeValidationError(w, map[string]string{"repo": "required", "files": "at least one file is required"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	if b.ConfigName != "" {
		// Repoint mode isn't checked again until the whole download
		// finishes (registerAndFinish) — a typo here would otherwise waste
		// a possibly multi-GB, multi-hour download before failing.
		if _, err := s.deps.Catalog.ConfigByName(ctx, b.ConfigName); err != nil {
			writeValidationError(w, map[string]string{"config_name": "no config with this name exists"})
			return
		}
	}
	id, err := s.deps.HFDownload.StartJob(ctx, b.toRequest(""))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, identity(r).Name, "hf_download_start", strings.TrimSpace(strconv.FormatInt(id, 10)), b.Repo)
	job, _ := s.deps.HFDownload.Get(ctx, id)
	writeJSON(w, http.StatusAccepted, hfDownloadToJSON(job, nil))
}

// handleHFDownloadApprove — POST /api/v1/hf/downloads/{id}/approve. Moves
// a smith-proposed pending_approval job to queued and starts it.
func (s *Server) handleHFDownloadApprove(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	if err := s.deps.HFDownload.ApproveJob(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "download not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, identity(r).Name, "hf_download_approve", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleHFDownloadPause — POST /api/v1/hf/downloads/{id}/pause.
func (s *Server) handleHFDownloadPause(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	paused := s.deps.HFDownload.Pause(id)
	s.audit(r, identity(r).Name, "hf_download_pause", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"success": true, "was_running": paused})
}

// handleHFDownloadResume — POST /api/v1/hf/downloads/{id}/resume.
func (s *Server) handleHFDownloadResume(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	s.deps.HFDownload.Start(id)
	s.audit(r, identity(r).Name, "hf_download_resume", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleHFDownloadCancel — POST /api/v1/hf/downloads/{id}/cancel.
func (s *Server) handleHFDownloadCancel(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	if err := s.deps.HFDownload.Cancel(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "download not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "hf_download_cancel", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handleHFDownloadDelete — DELETE /api/v1/hf/downloads/{id}. Only removes a
// terminal job's row; the service rejects a still-running one.
func (s *Server) handleHFDownloadDelete(w http.ResponseWriter, r *http.Request) {
	if !s.hfOK(w) {
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	if err := s.deps.HFDownload.Delete(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "download not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(r, identity(r).Name, "hf_download_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// ── HF token ─────────────────────────────────────────────────────────────

type hfTokenBody struct {
	Token string `json:"token"`
}

type hfTokenResponse struct {
	Token       string `json:"token"` // masked
	Configured  bool   `json:"configured"`
}

// handleHFTokenGet — GET /api/v1/hf/token. Never returns the real value.
func (s *Server) handleHFTokenGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not wired")
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	cfg := hf.LoadTokenConfig(ctx, s.deps.Settings)
	writeJSON(w, http.StatusOK, hfTokenResponse{Token: maskSecret(cfg.Token), Configured: cfg.Token != ""})
}

// handleHFTokenPut — PUT /api/v1/hf/token. Follows the same masked-value-
// means-unchanged contract as smith's web-provider keys
// (resolveAPIKeyWrite, smith_chat.go) — a UI round-trip (GET then PUT the
// same struct back) can never clobber a real token with its own mask.
func (s *Server) handleHFTokenPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not wired")
		return
	}
	var b hfTokenBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := hfCtx(r)
	defer cancel()
	current := hf.LoadTokenConfig(ctx, s.deps.Settings)
	resolved := resolveAPIKeyWrite(b.Token, current.Token)
	if err := hf.SaveTokenConfig(ctx, s.deps.Settings, hf.TokenConfig{Token: resolved}); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "hf_token_set", "", "")
	writeJSON(w, http.StatusOK, hfTokenResponse{Token: maskSecret(resolved), Configured: resolved != ""})
}

// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// worker.go — the download loop: resume semantics lifted verbatim from
// smith/fetch_model_ops.go's opFetchDownload/fetchDownloadAttempt (a
// 206+resumeFrom>0 appends, a 200 truncates and restarts — never a
// corrupted splice), extended with progress reporting, pause/cancel via
// context, and a per-file retry budget instead of one blocking procedure
// step.

const (
	fileAttemptTimeout = 2 * time.Hour // one HTTP attempt; a multi-GB fetch on a slow pipe is legitimately hours
	maxAttemptsPerFile = 3
	retryWait          = 5 * time.Second
	progressThrottle   = time.Second
)

// CreateJobRequest is Start/Propose's shared input.
type CreateJobRequest struct {
	Repo       string
	Revision   string
	Files      []PreflightFile
	DestDir    string
	ConfigName string // "" = auto-register a new Model/Variant/Artifact/Config
	ProposedBy string // "" = an operator started this directly
}

// StartJob creates the job row (state=queued) and immediately launches the
// worker — the operator-initiated path from the Models page. Preflight is
// re-run here (never trust a client-side report — the same "checked at
// proposal time AND re-checked at dispatch time" convention this repo uses
// everywhere else) and a blocked report is refused.
func (s *Service) StartJob(ctx context.Context, req CreateJobRequest) (int64, error) {
	id, err := s.createJob(ctx, req, "queued")
	if err != nil {
		return 0, err
	}
	s.Start(id)
	return id, nil
}

// ProposeJob creates the job row in pending_approval and does NOT start
// it — the path smith's download_start tool drives. Nothing downloads
// until ApproveJob is called by an operator.
func (s *Service) ProposeJob(ctx context.Context, req CreateJobRequest) (int64, error) {
	return s.createJob(ctx, req, "pending_approval")
}

func (s *Service) createJob(ctx context.Context, req CreateJobRequest, initialState string) (int64, error) {
	repo := strings.Trim(strings.TrimSpace(req.Repo), "/")
	if repo == "" {
		return 0, errors.New("hfdownload: repo is required")
	}
	if len(req.Files) == 0 {
		return 0, errors.New("hfdownload: at least one file is required")
	}
	report, err := s.Preflight(ctx, repo, req.Files, req.DestDir)
	if err != nil {
		return 0, fmt.Errorf("hfdownload: preflight: %w", err)
	}
	if report.Blocked {
		return 0, fmt.Errorf("hfdownload: preflight blocked this download: %s", blockedSummary(report))
	}

	revision := req.Revision
	if revision == "" {
		revision = "main"
	}
	files := make([]store.ModelDownloadFileRow, len(req.Files))
	for i, f := range req.Files {
		files[i] = store.ModelDownloadFileRow{
			Filename: f.Filename, DestRelPath: DestRelPath(req.DestDir, f.Filename),
			BytesTotal: f.SizeBytes, SortOrder: i,
		}
	}
	var totalBytes int64
	for _, f := range req.Files {
		totalBytes += f.SizeBytes
	}
	id, err := s.d.Store.ModelDownloads().Create(ctx, store.ModelDownloadRow{
		Repo: repo, Revision: revision, DestDir: req.DestDir, ConfigName: req.ConfigName,
		State: initialState, BytesTotal: totalBytes, ProposedBy: req.ProposedBy,
	}, files)
	if err != nil {
		return 0, fmt.Errorf("hfdownload: create job: %w", err)
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": id, "state": initialState, "error": ""})
	return id, nil
}

func blockedSummary(r PreflightReport) string {
	var reasons []string
	for _, c := range r.Checks {
		if c.Severity == "block" {
			reasons = append(reasons, c.Summary)
		}
	}
	return strings.Join(reasons, "; ")
}

// ApproveJob moves a pending_approval job to queued and starts it — the
// operator-approval path for a job smith proposed.
func (s *Service) ApproveJob(ctx context.Context, jobID int64) error {
	job, err := s.d.Store.ModelDownloads().Get(ctx, jobID)
	if err != nil {
		return err
	}
	if job.State != "pending_approval" {
		return fmt.Errorf("hfdownload: job %d is %q, not pending_approval", jobID, job.State)
	}
	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "queued", ""); err != nil {
		return err
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "queued", "error": ""})
	s.Start(jobID)
	return nil
}

// Start launches (or resumes) jobID's worker if one isn't already running.
// Safe to call on a queued or paused job; a no-op (not an error) if the
// job is already running, so a client can call it optimistically.
func (s *Service) Start(jobID int64) {
	if s.isRunning(jobID) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.setActive(jobID, cancel)
	goSafe(s.d.logf, fmt.Sprintf("hfdownload-job-%d", jobID), func() {
		s.runWorker(ctx, jobID)
	})
}

// Pause cancels jobID's active download (if any) and leaves it resumable.
// A no-op if the job isn't currently running.
func (s *Service) Pause(jobID int64) bool {
	s.setIntent(jobID, "paused")
	return s.cancelActive(jobID)
}

// Cancel stops jobID (if running), deletes any partial download files, and
// marks the job cancelled.
func (s *Service) Cancel(ctx context.Context, jobID int64) error {
	s.setIntent(jobID, "cancelled")
	wasRunning := s.cancelActive(jobID)
	if !wasRunning {
		// Not mid-flight — just tidy up any .part files and mark it.
		if err := s.deletePartFiles(ctx, jobID); err != nil {
			s.d.logf("hfdownload: cancel %d: cleanup: %v", jobID, err)
		}
		if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "cancelled", ""); err != nil {
			return err
		}
		s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "cancelled", "error": ""})
	}
	// If it WAS running, runWorker's own cleanup (below) sees the
	// "cancelled" signal and finishes the job — including part-file
	// deletion — after its in-flight HTTP call actually unwinds.
	return nil
}

// Delete permanently removes jobID's row (and its per-file rows, via the
// store's FK cascade). Refuses a job with a live worker goroutine — Cancel
// (or letting it reach a terminal state) first avoids deleting the row out
// from under an in-flight download that would otherwise keep writing to a
// destination path nothing tracks anymore.
func (s *Service) Delete(ctx context.Context, jobID int64) error {
	if s.isRunning(jobID) {
		return fmt.Errorf("hfdownload: job %d is still running — cancel it first", jobID)
	}
	if err := s.d.Store.ModelDownloads().Delete(ctx, jobID); err != nil {
		return err
	}
	s.d.publish(EventDeleted, map[string]any{"job_id": jobID})
	return nil
}

func (s *Service) deletePartFiles(ctx context.Context, jobID int64) error {
	cfg := s.d.Cfg()
	if cfg == nil || cfg.Paths.ModelsDir == "" {
		return nil
	}
	files, err := s.d.Store.ModelDownloads().ListFiles(ctx, jobID)
	if err != nil {
		return err
	}
	for _, f := range files {
		part := filepath.Join(cfg.Paths.ModelsDir, f.DestRelPath) + ".part"
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			s.d.logf("hfdownload: remove %s: %v", part, err)
		}
	}
	return nil
}

// runWorker drives jobID to completion, pause, or failure. Every path out
// clears the active-goroutine entry.
func (s *Service) runWorker(ctx context.Context, jobID int64) {
	defer s.clearActive(jobID)

	job, err := s.d.Store.ModelDownloads().Get(ctx, jobID)
	if err != nil {
		s.d.logf("hfdownload: worker %d: load job: %v", jobID, err)
		return
	}
	cfg := s.d.Cfg()
	if cfg == nil || cfg.Paths.ModelsDir == "" {
		s.failJob(context.Background(), jobID, errors.New("models dir not configured (infra.paths.models_dir)"))
		return
	}
	if s.d.HF == nil {
		s.failJob(context.Background(), jobID, errors.New("hf client not wired"))
		return
	}

	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "running", ""); err != nil {
		s.d.logf("hfdownload: worker %d: state->running: %v", jobID, err)
		return
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "running", "error": ""})

	files, err := s.d.Store.ModelDownloads().ListFiles(ctx, jobID)
	if err != nil {
		s.failJob(context.Background(), jobID, fmt.Errorf("load files: %w", err))
		return
	}

	// Phase 4 asset enrichment starts NOW, concurrently with the download
	// that follows — its whole point. Buffered 1 so the goroutine never
	// blocks even if nobody reads the channel (a failed/cancelled job).
	// Its own context is independently bounded (never context.Background()
	// with no deadline) — registerAndFinish only waits up to
	// enrichWaitTimeout, but an unbounded context here would still leak
	// the goroutine (and its open HTTP connection) forever if HF simply
	// never answers.
	enrichCh := make(chan Enrichment, 1)
	goSafe(s.d.logf, fmt.Sprintf("hfdownload-enrich-%d", jobID), func() {
		ectx, cancel := context.WithTimeout(context.Background(), enrichWaitTimeout)
		defer cancel()
		enrichCh <- s.enrich(ectx, job.Repo)
	})

	prog := &rateTracker{}
	for _, f := range files {
		if f.State == "verified" {
			continue
		}
		if err := s.downloadAndVerifyFile(ctx, job, f, prog); err != nil {
			if errors.Is(err, context.Canceled) {
				s.onCancelledOrPaused(jobID)
				return
			}
			s.failJob(context.Background(), jobID, fmt.Errorf("%s: %w", f.Filename, err))
			return
		}
	}

	s.registerAndFinish(context.Background(), jobID, job, enrichCh)
}

// onCancelledOrPaused runs after the worker's context died from an
// intentional Pause/Cancel (never from a real error) — it always uses a
// fresh background context, since ctx itself is already cancelled.
func (s *Service) onCancelledOrPaused(jobID int64) {
	intent := s.takeIntentOrDefault(jobID, "paused")
	ctx := context.Background()
	if intent == "cancelled" {
		if err := s.deletePartFiles(ctx, jobID); err != nil {
			s.d.logf("hfdownload: cancel %d: cleanup: %v", jobID, err)
		}
	}
	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, intent, ""); err != nil {
		s.d.logf("hfdownload: worker %d: state->%s: %v", jobID, intent, err)
		return
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": intent, "error": ""})
}

func (s *Service) failJob(ctx context.Context, jobID int64, cause error) {
	msg := cause.Error()
	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "failed", msg); err != nil {
		s.d.logf("hfdownload: worker %d: state->failed: %v", jobID, err)
	}
	s.d.publish(EventFailed, map[string]any{"job_id": jobID, "error": msg})
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "failed", "error": msg})
}

// downloadAndVerifyFile downloads f (with bounded retries + Range resume)
// then verifies it: sha256 when expected, and a mandatory GGUF header read
// for any .gguf file (the same evidence opFetchVerify captured).
func (s *Service) downloadAndVerifyFile(ctx context.Context, job store.ModelDownloadRow, f store.ModelDownloadFileRow, prog *rateTracker) error {
	cfg := s.d.Cfg()
	final := filepath.Join(cfg.Paths.ModelsDir, f.DestRelPath)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("destination %s already exists — refusing to overwrite", final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	part := final + ".part"

	client := s.hfHTTPClient()
	url := s.d.HF.BaseURL
	if url == "" {
		url = "https://huggingface.co"
	}
	url += "/" + job.Repo + "/resolve/" + job.Revision + "/" + f.Filename

	onProgress := func(written int64) {
		completedBytes := int64(0)
		// bytes_total across files already downloaded (state==verified)
		// plus this file's live progress — cheap enough to recompute each
		// throttled sample (at most once/second).
		filesNow, _ := s.d.Store.ModelDownloads().ListFiles(context.Background(), job.ID)
		for _, other := range filesNow {
			if other.ID == f.ID {
				completedBytes += written
			} else if other.State == "verified" {
				completedBytes += other.BytesTotal
			}
		}
		rate, eta := prog.sample(completedBytes, job.BytesTotal)
		_ = s.d.Store.ModelDownloads().UpdateFileProgress(context.Background(), f.ID, written, f.BytesTotal)
		_ = s.d.Store.ModelDownloads().UpdateProgress(context.Background(), job.ID, completedBytes, job.BytesTotal)
		s.d.publish(EventProgress, map[string]any{
			"job_id": job.ID, "state": "running", "bytes_done": completedBytes, "bytes_total": job.BytesTotal,
			"bytes_per_sec": rate, "eta_s": eta, "current_file": f.Filename,
		})
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttemptsPerFile; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := downloadFileAttempt(ctx, client, url, part, onProgress)
		if err == nil {
			lastErr = nil
			break
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		lastErr = err
		_ = s.d.Store.ModelDownloads().IncrementAttempts(context.Background(), job.ID)
		if attempt < maxAttemptsPerFile {
			select {
			case <-time.After(retryWait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if lastErr != nil {
		_ = s.d.Store.ModelDownloads().UpdateFileState(context.Background(), f.ID, "failed", "")
		return fmt.Errorf("download failed after %d attempt(s): %w", maxAttemptsPerFile, lastErr)
	}

	// ── verify ──
	if err := s.d.Store.ModelDownloads().UpdateState(context.Background(), job.ID, "verifying", ""); err != nil {
		s.d.logf("hfdownload: worker %d: state->verifying: %v", job.ID, err)
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": job.ID, "state": "verifying", "error": ""})

	if f.SHA256Expected != "" {
		got, err := sha256File(part)
		if err != nil {
			return fmt.Errorf("hash %s: %w", part, err)
		}
		if !strings.EqualFold(got, f.SHA256Expected) {
			_ = s.d.Store.ModelDownloads().UpdateFileState(context.Background(), f.ID, "failed", got)
			return fmt.Errorf("checksum mismatch: expected %s, got %s", f.SHA256Expected, got)
		}
	}
	var sha256Actual string
	if strings.HasSuffix(strings.ToLower(f.Filename), ".gguf") {
		md, err := gguf.ReadMetadata(part)
		if err != nil {
			_ = s.d.Store.ModelDownloads().UpdateFileState(context.Background(), f.ID, "failed", "")
			return fmt.Errorf("%s is not a readable GGUF: %w", f.Filename, err)
		}
		_ = md // metadata is re-read fresh at registration time (house rule: every step re-derives)
	}
	if f.SHA256Expected != "" {
		sha256Actual = strings.ToLower(f.SHA256Expected)
	}

	// ── finalize: atomic same-directory rename ──
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("finalize rename %s -> %s: %w", part, final, err)
	}
	if err := s.d.Store.ModelDownloads().UpdateFileState(context.Background(), f.ID, "verified", sha256Actual); err != nil {
		s.d.logf("hfdownload: worker %d: file %d state->verified: %v", job.ID, f.ID, err)
	}
	return nil
}

// hfHTTPClient borrows only the wired client's Transport — never its
// blanket Timeout, which on this daemon's shared client is sized for
// loopback health probes (3s), the same trap smith/fetch_model_ops.go
// avoids. Individual attempts are bounded by fileAttemptTimeout via
// context instead.
func (s *Service) hfHTTPClient() *http.Client {
	if s.d.HF != nil && s.d.HF.HTTP != nil {
		return &http.Client{Transport: s.d.HF.HTTP.Transport}
	}
	return &http.Client{}
}

// downloadFileAttempt performs one HTTP GET with Range resume, calling
// onProgress with the cumulative bytes written to part (including any
// resumed prefix) at most once per progressThrottle. Semantics ported
// verbatim from smith/fetch_model_ops.go's fetchDownloadAttempt: a 206
// (resumeFrom>0) appends, a 200 truncates and restarts from zero — never a
// corrupted splice.
func downloadFileAttempt(ctx context.Context, client *http.Client, rawURL, part string, onProgress func(written int64)) (int64, error) {
	actx, cancel := context.WithTimeout(ctx, fileAttemptTimeout)
	defer cancel()

	resumeFrom := int64(0)
	if st, err := os.Stat(part); err == nil {
		resumeFrom = st.Size()
	}

	req, err := http.NewRequestWithContext(actx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Forge-Requested-By", "forge-hfdownload")
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}
	resp, err := client.Do(req)
	if err != nil {
		if actx.Err() == context.Canceled {
			return 0, context.Canceled
		}
		return 0, err
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case resp.StatusCode == http.StatusPartialContent && resumeFrom > 0:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC
		resumeFrom = 0
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(part, flags, 0o600) //nolint:gosec // part path contained by DestRelPath
	if err != nil {
		return 0, err
	}
	defer f.Close()

	pw := &progressWriter{base: resumeFrom, onProgress: onProgress}
	n, err := io.Copy(io.MultiWriter(f, pw), resp.Body)
	if err != nil {
		if errors.Is(err, context.Canceled) || actx.Err() == context.Canceled {
			return resumeFrom + n, context.Canceled
		}
		return resumeFrom + n, err
	}
	if onProgress != nil {
		onProgress(resumeFrom + n) // final, unthrottled sample so bytes_done ends up exact
	}
	return resumeFrom + n, nil
}

// progressWriter counts bytes and calls onProgress at most once per
// progressThrottle — never blocks the copy loop on the callback's own
// work (store write + SSE publish), since onProgress runs synchronously
// on the copy goroutine; callers keep it cheap.
type progressWriter struct {
	base       int64
	written    int64
	onProgress func(totalWritten int64)
	lastReport time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	now := time.Now()
	if w.onProgress != nil && now.Sub(w.lastReport) >= progressThrottle {
		w.lastReport = now
		w.onProgress(w.base + w.written)
	}
	return n, nil
}

// rateTracker computes a smoothed bytes/sec and a naive ETA from
// consecutive progress samples. Not goroutine-safe by design — one worker
// goroutine owns one job at a time.
type rateTracker struct {
	lastTime  time.Time
	lastBytes int64
	rateEWMA  float64
}

const rateEWMAAlpha = 0.3

func (r *rateTracker) sample(bytesDone, bytesTotal int64) (bytesPerSec float64, etaSeconds float64) {
	now := time.Now()
	if !r.lastTime.IsZero() {
		dt := now.Sub(r.lastTime).Seconds()
		if dt > 0 {
			instant := float64(bytesDone-r.lastBytes) / dt
			if instant < 0 {
				instant = 0
			}
			if r.rateEWMA == 0 {
				r.rateEWMA = instant
			} else {
				r.rateEWMA = rateEWMAAlpha*instant + (1-rateEWMAAlpha)*r.rateEWMA
			}
		}
	}
	r.lastTime = now
	r.lastBytes = bytesDone
	if r.rateEWMA > 0 && bytesTotal > bytesDone {
		etaSeconds = float64(bytesTotal-bytesDone) / r.rateEWMA
	}
	return r.rateEWMA, etaSeconds
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

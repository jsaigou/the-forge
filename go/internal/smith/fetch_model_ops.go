// SPDX-License-Identifier: Apache-2.0

package smith

// fetch_model_ops.go implements every native Op the fetch_model procedure
// (procedures/fetch_model.go) declares, wired into runNativeOp's switch
// (procedure.go) — P3smith. Same posture as build_refresh_ops.go: every op
// resolves its target fresh from params + live config each call, shallow
// validators are re-checked at dispatch time (the real safety checks — path
// containment under ModelsDir, catalog existence — live HERE, not in
// Param.Allowed), and nothing shells out: the download is a plain
// net/http GET through Deps.HTTPClient's Transport.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
)

const (
	// fetchModelAttemptTimeout bounds ONE HTTP attempt of the download — a
	// multi-GB fetch on a slow pipe is legitimately hours, so this is not
	// the place for a loopback-sized leash. The caller's own context still
	// wins.
	fetchModelAttemptTimeout = 2 * time.Hour
	// fetchModelMaxAttempts bounds retry: one clean pass plus two retries
	// covers transient connection resets without ever looping tight.
	fetchModelMaxAttempts = 3
	fetchModelRetryWait   = 5 * time.Second
)

// fetchModelBaseURL is the HF resolve root. Package-level var so tests can
// point it at a fake server; production code never writes it.
var fetchModelBaseURL = "https://huggingface.co"

// fetchModelTarget is one resolved fetch_model param set.
type fetchModelTarget struct {
	Repo        string
	Filename    string
	DestRelPath string // defaults to filepath.Base(Filename)
	SHA256      string // "" = unchecked
	ConfigName  string // "" = no catalog write
}

func resolveFetchModelTarget(params map[string]string) (fetchModelTarget, error) {
	repo := strings.Trim(strings.TrimSpace(params["hf_repo"]), "/")
	if repo == "" {
		return fetchModelTarget{}, errors.New("smith: fetch_model requires hf_repo")
	}
	if !procedures.HFRepoAllowed(repo) {
		return fetchModelTarget{}, fmt.Errorf("smith: fetch_model hf_repo %q not allowed", repo)
	}
	filename := strings.TrimSpace(params["filename"])
	if filename == "" {
		return fetchModelTarget{}, errors.New("smith: fetch_model requires filename")
	}
	if !procedures.FilenameAllowed(filename) {
		return fetchModelTarget{}, fmt.Errorf("smith: fetch_model filename %q not allowed", filename)
	}
	dest := params["dest_rel_path"]
	if dest == "" {
		dest = filepath.Base(filename)
	}
	if !procedures.DestRelPathAllowed(dest) {
		return fetchModelTarget{}, fmt.Errorf("smith: fetch_model dest_rel_path %q not allowed", dest)
	}
	t := fetchModelTarget{
		Repo: repo, Filename: filename, DestRelPath: dest,
		SHA256:     params["sha256"],
		ConfigName: params["config_name"],
	}
	if t.SHA256 != "" && !procedures.SHA256Allowed(t.SHA256) {
		return fetchModelTarget{}, errors.New("smith: fetch_model sha256 must be 64 hex chars")
	}
	if t.ConfigName != "" && !procedures.CatalogNameAllowed(t.ConfigName) {
		return fetchModelTarget{}, fmt.Errorf("smith: fetch_model config_name %q not allowed", t.ConfigName)
	}
	return t, nil
}

// fetchModelsDir reads the models dir from the live infra config
// (cfg.Paths.ModelsDir — the same value the registry resolves artifact
// paths against). Empty/unwired fails closed: guessing a default would
// write tens of GB somewhere nobody configured.
func (s *Smith) fetchModelsDir() (string, error) {
	cfg := s.cfg()
	if cfg == nil || cfg.Paths.ModelsDir == "" {
		return "", errors.New("smith: fetch_model: models dir not configured (infra.paths.models_dir)")
	}
	return cfg.Paths.ModelsDir, nil
}

// fetchFinalPath joins ModelsDir + DestRelPath and proves containment (the
// join cannot escape when dest_rel_path passed its validator, but this is
// the load-bearing re-check — EvalSymlinks-free because the destination may
// not exist yet).
func fetchFinalPath(modelsDir, destRel string) (string, error) {
	final := filepath.Join(modelsDir, destRel)
	rel, err := filepath.Rel(modelsDir, final)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("smith: fetch_model: dest_rel_path %q escapes the models dir", destRel)
	}
	return final, nil
}

// ── step 0: download ────────────────────────────────────────────────────────

// opFetchDownload streams <base>/<repo>/resolve/main/<filename> to
// <models_dir>/<dest>.part with HTTP Range resume and bounded retries.
// Resume semantics are deterministic regardless of server behavior: a 206
// continues appending to the existing .part; a 200 (server ignored the
// Range) truncates and restarts from zero — never a corrupted splice. An
// existing FINAL file fails closed (this procedure does not silently
// overwrite a model something may be serving).
func (s *Smith) opFetchDownload(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	start := time.Now()
	target, err := resolveFetchModelTarget(params)
	if err != nil {
		return procedures.StepResult{}, err
	}
	modelsDir, err := s.fetchModelsDir()
	if err != nil {
		return procedures.StepResult{}, err
	}
	final, err := fetchFinalPath(modelsDir, target.DestRelPath)
	if err != nil {
		return procedures.StepResult{}, err
	}
	if _, err := os.Stat(final); err == nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: destination %s already exists — refusing to overwrite", final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: stat destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: mkdir: %w", err)
	}
	part := final + ".part"

	if s.d.HTTPClient == nil {
		return procedures.StepResult{}, ErrHTTPUnwired
	}
	client := &http.Client{Transport: s.d.HTTPClient.Transport}
	url := fetchModelBaseURL + "/" + target.Repo + "/resolve/main/" + target.Filename

	var lastErr error
	for attempt := 1; attempt <= fetchModelMaxAttempts; attempt++ {
		written, rerr := fetchDownloadAttempt(ctx, client, url, part)
		if rerr == nil {
			out := fmt.Sprintf("downloaded %s (%s) -> %s", url, humanBytes(written), part)
			if written == -1 {
				out = fmt.Sprintf("downloaded %s (resumed stream, byte count unavailable) -> %s", url, part)
			}
			return procedures.StepResult{Stdout: out, Duration: time.Since(start)}, nil
		}
		lastErr = rerr
		if ctx.Err() != nil {
			break // caller cancelled or gave up — do not burn retries
		}
		if attempt < fetchModelMaxAttempts {
			select {
			case <-time.After(fetchModelRetryWait):
			case <-ctx.Done():
				return procedures.StepResult{}, ctx.Err()
			}
		}
	}
	return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: download failed after %d attempt(s): %w", fetchModelMaxAttempts, lastErr)
}

// fetchDownloadAttempt performs one HTTP GET with Range resume. Returns the
// number of bytes written THIS attempt (-1 when unknowable mid-append is
// fine — callers only use it for display), or an error.
func fetchDownloadAttempt(ctx context.Context, client *http.Client, url, part string) (int64, error) {
	actx, cancel := context.WithTimeout(ctx, fetchModelAttemptTimeout)
	defer cancel()

	resumeFrom := int64(0)
	if st, err := os.Stat(part); err == nil {
		resumeFrom = st.Size()
	}

	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Forge-Requested-By", "smith")
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	flags := os.O_WRONLY | os.O_CREATE
	switch {
	case resp.StatusCode == http.StatusPartialContent && resumeFrom > 0:
		flags |= os.O_APPEND // server honored the Range — continue
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC // server ignored Range (or fresh start) — rewrite from zero
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return n, err
	}
	return n, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ── step 1: verify ────────────────────────────────────────────────────────

// opFetchVerify checks the .part file: sha256 when the param was provided,
// and for any .gguf filename ALWAYS a full header read via internal/gguf
// (whose magic check fails the run on non-GGUF content) surfacing trained
// n_ctx + parameter count into the result — the evidence the checkpoint's
// operator sees. Re-reads the file rather than trusting anything threaded
// from the download step (house rule: every op re-derives fresh).
func (s *Smith) opFetchVerify(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	target, err := resolveFetchModelTarget(params)
	if err != nil {
		return procedures.StepResult{}, err
	}
	modelsDir, err := s.fetchModelsDir()
	if err != nil {
		return procedures.StepResult{}, err
	}
	final, err := fetchFinalPath(modelsDir, target.DestRelPath)
	if err != nil {
		return procedures.StepResult{}, err
	}
	part := final + ".part"

	var log strings.Builder
	if target.SHA256 != "" {
		got, ferr := sha256File(part)
		if ferr != nil {
			return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: hash %s: %w", part, ferr)
		}
		if !strings.EqualFold(got, target.SHA256) {
			return procedures.StepResult{}, fmt.Errorf(
				"smith: fetch_model: checksum mismatch for %s: expected %s, got %s",
				target.Filename, target.SHA256, got)
		}
		fmt.Fprintf(&log, "sha256 verified: %s\n", got)
	} else {
		log.WriteString("sha256: not provided by the action — skipped (GGUF header check still applies)\n")
	}

	if strings.HasSuffix(strings.ToLower(target.Filename), ".gguf") {
		md, merr := gguf.ReadMetadata(part)
		if merr != nil {
			return procedures.StepResult{Stdout: log.String()}, fmt.Errorf("smith: fetch_model: %s is not a readable GGUF: %w", target.Filename, merr)
		}
		fmt.Fprintf(&log, "gguf header: arch=%s name=%q trained_ctx=%d parameters=%s quant=%s size=%s\n",
			md.Architecture, md.Name, md.TrainedCtx, ggufCountOrUnknown(md.ParameterCount), md.QuantType, humanBytes(md.FileSizeBytes))
	}

	st, serr := os.Stat(part)
	if serr != nil {
		return procedures.StepResult{Stdout: log.String()}, fmt.Errorf("smith: fetch_model: stat %s: %w", part, serr)
	}
	out := procedures.StepResult{Stdout: log.String()}
	out.CheckpointNote = fmt.Sprintf(
		"%s verified (%s on disk). Approve to move it to %s%s.",
		target.Filename, humanBytes(st.Size()), final,
		map[bool]string{true: " and repoint the catalog artifact for " + target.ConfigName, false: ""}[target.ConfigName != ""])
	return out, nil
}

func ggufCountOrUnknown(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%.2fB", float64(n)/1e9)
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

// ── step 2: finalize (atomic rename) ──────────────────────────────────────

func (s *Smith) opFetchFinalize(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	start := time.Now()
	target, err := resolveFetchModelTarget(params)
	if err != nil {
		return procedures.StepResult{}, err
	}
	modelsDir, err := s.fetchModelsDir()
	if err != nil {
		return procedures.StepResult{}, err
	}
	final, err := fetchFinalPath(modelsDir, target.DestRelPath)
	if err != nil {
		return procedures.StepResult{}, err
	}
	part := final + ".part"
	if _, err := os.Stat(part); err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: %s missing — did the download+verify steps run?", part)
	}
	// Same-directory rename: atomic on POSIX — a concurrent reader either
	// sees nothing at `final` or the complete file, never a partial one.
	if err := os.Rename(part, final); err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: rename %s -> %s: %w", part, final, err)
	}
	return procedures.StepResult{
		Stdout:   fmt.Sprintf("moved %s -> %s", part, final),
		Duration: time.Since(start),
	}, nil
}

// ── step 3: catalog link ──────────────────────────────────────────────────

// opFetchCatalogLink repoints config_name's weight artifact row at the new
// file THROUGH execute.go's catalog-change scaffolding (dispatch seam:
// applyCatalogChange, which P3smith minimally implemented for artifact
// rows — see sourcing.go). Everything it writes is re-derived fresh from
// the finalized file and the live catalog; nothing is threaded from
// earlier steps.
func (s *Smith) opFetchCatalogLink(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	start := time.Now()
	target, err := resolveFetchModelTarget(params)
	if err != nil {
		return procedures.StepResult{}, err
	}
	if target.ConfigName == "" {
		return procedures.StepResult{Stdout: "no config_name param — catalog linking intentionally skipped"}, nil
	}
	modelsDir, err := s.fetchModelsDir()
	if err != nil {
		return procedures.StepResult{}, err
	}
	final, err := fetchFinalPath(modelsDir, target.DestRelPath)
	if err != nil {
		return procedures.StepResult{}, err
	}
	if s.d.Catalog == nil {
		return procedures.StepResult{}, ErrCatalogChangeUnwired
	}
	cfgRow, err := s.d.Catalog.ConfigByName(ctx, target.ConfigName)
	if err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: resolve config %q: %w", target.ConfigName, err)
	}
	art, err := s.d.Catalog.GetArtifact(ctx, cfgRow.WeightArtifactID)
	if err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: weight artifact for config %q: %w", target.ConfigName, err)
	}

	updated := art
	updated.FilePath = target.DestRelPath // relative — same convention the registry resolves against ModelsDir
	if st, serr := os.Stat(final); serr == nil {
		updated.FileSizeBytes = st.Size()
	}
	if strings.HasSuffix(strings.ToLower(target.Filename), ".gguf") {
		if md, merr := gguf.ReadMetadata(final); merr == nil {
			updated.GGUFArch = md.Architecture
			updated.GGUFTrainedCtx = md.TrainedCtx
			updated.GGUFParameterCount = fmt.Sprintf("%d", md.ParameterCount)
			updated.GGUFQuantType = md.QuantType
		} else {
			return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: finalized file failed GGUF re-read: %w", merr)
		}
	}
	if target.SHA256 != "" {
		updated.SHA256 = strings.ToLower(target.SHA256)
	}

	rowJSON, jerr := json.Marshal(updated)
	if jerr != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: marshal artifact row: %w", jerr)
	}
	// The dispatch seam: exactly what KindCatalogChange actions flow through
	// (execute.go's dispatchCatalogChange -> applyCatalogChange), invoked
	// here as a library call since a procedure step has no Action row of its
	// own to hang a detail off.
	detail := catalogChangeDetail{Op: "update", Table: "artifact", Row: rowJSON}
	if err := s.applyCatalogChange(ctx, detail); err != nil {
		return procedures.StepResult{}, fmt.Errorf("smith: fetch_model: catalog write for %q: %w", target.ConfigName, err)
	}
	return procedures.StepResult{
		Stdout:   fmt.Sprintf("repointed config %q weight artifact id %d -> %s via the catalog-change dispatch seam", target.ConfigName, art.ID, updated.FilePath),
		Duration: time.Since(start),
	}, nil
}

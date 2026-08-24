// SPDX-License-Identifier: Apache-2.0

package procedures

import (
	"regexp"
	"strings"
	"time"
)

// fetchModelProcedure turns P6's model-sourcing runbook ("here is the hf
// download command — an operator types it") into an executable native
// procedure (P3smith). sourcing.go's Evaluate() remains read-only research;
// this is the write path it always stopped short of: download one GGUF from
// HuggingFace into the configured models dir, verify it, move it into
// place, and optionally repoint a catalog config's weight artifact at the
// new file through execute.go's catalog-change scaffolding.
//
// Risk classification follows build_refresh's pattern for large disk
// writes rather than its maintenance-window pattern: the genuinely risky
// resource here is DISK (a model file can be tens of GB), so `disk_space`
// is a hard precondition and Impact.NeedsMaintenance stays false — nothing
// in this procedure touches a slot, live traffic, or any service a0
// fronts. The download itself runs entirely OUTSIDE any window (it can
// take hours; holding one that long would be disclosure theater).
//
// One mandatory checkpoint, after download+verification and BEFORE either
// irreversible-ish step (the rename that puts tens of GB at the final path,
// or any catalog write): the operator sees the real byte count, checksum
// result, and parsed GGUF header before approving placement. This mirrors
// build-refresh.md's own framing of what deserves a human read — evidence
// first, mutation second.
var fetchModelProcedure = Procedure{
	ID:    "fetch_model",
	Title: "Fetch a model file from HuggingFace into the models dir",
	Impact: Impact{
		NeedsMaintenance: false,
		// Honest generous estimate for a multi-GB fetch on a residential
		// pipe — same "errs generous" posture as build_refresh's comment;
		// no maintenance window is requested, so this is pure disclosure.
		EstDuration: 2 * time.Hour,
	},
	Preconditions: []string{"disk_space"},
	Params: []Param{
		{Name: "hf_repo", Allowed: hfRepoAllowed},
		{Name: "filename", Allowed: filenameAllowed},
		{Name: "dest_rel_path", Allowed: destRelPathAllowed, Optional: true},
		{Name: "sha256", Allowed: sha256Allowed, Optional: true},
		{Name: "config_name", Allowed: catalogNameAllowed, Optional: true},
	},
	Steps: []Step{
		{
			Title:  "Download to <dest>.part",
			Why:    "streams https://huggingface.co/<repo>/resolve/main/<file> to a .part sibling with Range resume and bounded retries — never writing the final path until verified.",
			Op:     "fetch_download",
			OnFail: FailAbort,
		},
		{
			Title: "Verify checksum and GGUF header",
			Why:   "sha256 when provided; GGUF magic + header KV (trained n_ctx, parameter count) whenever the filename says .gguf — the evidence the operator approves placement against.",
			Op:    "fetch_verify",
			// The checkpoint: pause AFTER download+verify succeeded, BEFORE
			// the rename or any catalog write.
			Checkpoint: true,
			OnFail:     FailAbort,
		},
		{
			Title:  "Move into place",
			Why:    "atomic same-directory rename of <dest>.part to <dest> — readers never observe a partial file at the final path.",
			Op:     "fetch_finalize",
			OnFail: FailAbort,
		},
		{
			Title:  "Link the catalog artifact",
			Why:    "when config_name was supplied, repoints that config's weight artifact row at the new file through the existing catalog-change dispatch seam; without it, this step is an honest no-op.",
			Op:     "fetch_catalog_link",
			OnFail: FailAbort,
		},
	},
}

func init() {
	Register(fetchModelProcedure)
}

// ── shallow param validators ────────────────────────────────────────────────
//
// Deliberately charset/shape only (Param.Allowed's contract) — the real
// safety checks (path containment under ModelsDir, catalog existence) are
// re-done in the op handlers themselves, per the "checked at proposal time
// AND re-checked at dispatch time" convention.

// hfRepoRe matches HuggingFace repo ids ("org/name" or bare "name"). The
// Allowed func still rejects ".." explicitly — dots are legal inside HF
// names, so the regex alone doesn't rule out a ".." segment.
var hfRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

func hfRepoAllowed(v string) bool {
	return !strings.Contains(v, "..") && hfRepoRe.MatchString(v)
}

// HFRepoAllowed is the exported alias of fetch_model's hf_repo Param check —
// smith's op handlers re-run every shallow validator at dispatch time (the
// "checked at proposal time AND re-checked at dispatch time" convention),
// and these live here because they belong to the declared Param set.
func HFRepoAllowed(v string) bool { return hfRepoAllowed(v) }

// FilenameAllowed is fetch_model's filename param check, exported for
// smith's dispatch-time re-validation (see HFRepoAllowed).
func FilenameAllowed(v string) bool { return filenameAllowed(v) }

// DestRelPathAllowed is fetch_model's dest_rel_path param check, exported
// for smith's dispatch-time re-validation (see HFRepoAllowed).
func DestRelPathAllowed(v string) bool { return destRelPathAllowed(v) }

// SHA256Allowed is fetch_model's sha256 param check, exported for smith's
// dispatch-time re-validation (see HFRepoAllowed).
func SHA256Allowed(v string) bool { return sha256Allowed(v) }

// CatalogNameAllowed is fetch_model's config_name param check, exported for
// smith's dispatch-time re-validation (see HFRepoAllowed).
func CatalogNameAllowed(v string) bool { return catalogNameAllowed(v) }

// safeSegmentRe is one filename/path segment: no separators, no shell
// metacharacters, no leading dot tricks beyond a normal extension dot.
var safeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func filenameAllowed(v string) bool {
	return v != "." && v != ".." && !strings.ContainsAny(v, "/\\") && safeSegmentRe.MatchString(v)
}

func destRelPathAllowed(v string) bool {
	if v == "" || strings.HasPrefix(v, "/") {
		return false
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == "" || seg == "." || seg == ".." || !safeSegmentRe.MatchString(seg) {
			return false
		}
	}
	return true
}

func sha256Allowed(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// catalogNameAllowed is the same shallow shape check ConfigByName's callers
// rely on elsewhere: printable, no whitespace/control characters, bounded
// length. Existence is checked live against the catalog by the op.
func catalogNameAllowed(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

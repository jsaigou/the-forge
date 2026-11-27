// SPDX-License-Identifier: Apache-2.0

package store

// Writes to an Artifact (create, update, delete) with their rules in one place, the same shape as ConfigWrites and BuildWrites. An Artifact is one
// file (or directory, for safetensors) a Variant ships as. The rules: a known Variant, Format and Quantization, a type of weight, mmproj or
// tokenizer, a file that exists under the models directory, and for GGUF a readable header, whose architecture, trained context, parameter
// count, quantization and size are stored from the file rather than trusted from the caller. An Artifact a Config uses cannot be deleted.
//
// Errors: *Invalid for the operator's mistakes, *ArtifactInUseError, ErrNotFound, and the store's own error when a read the rules depend on
// fails: a failed check is never a passed check (the duplicate guard and the in-use guard used to be skipped when their lists failed to read).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/gguf"
)

// ArtifactInUseError refuses deleting an Artifact that Configs still use as their weight or mmproj.
type ArtifactInUseError struct {
	ID      int64
	Configs []string
}

func (e *ArtifactInUseError) Error() string {
	return fmt.Sprintf("artifact %d is used by %d config(s): %s — repoint or delete them first", e.ID, len(e.Configs), strings.Join(e.Configs, ", "))
}

// ArtifactWrites applies the rules and saves. It holds no state; build one per use.
type ArtifactWrites struct {
	Cat Catalog
	// Changed, if set, runs after every successful write (the cache invalidation hook).
	Changed func()
	// ModelsDir is the directory artifact files must live under; "" (or nil) means it is not configured, which refuses every write.
	ModelsDir func() string
}

func (w ArtifactWrites) changed() {
	if w.Changed != nil {
		w.Changed()
	}
}

// ResolveFile maps an artifact's file_path to a path under the models directory, rejecting anything that escapes it (a relative path is joined to
// it; an absolute one must already be inside). The second result is what is wrong, "" when the path is fine.
func (w ArtifactWrites) ResolveFile(path string) (string, string) {
	root := ""
	if w.ModelsDir != nil {
		root = w.ModelsDir()
	}
	if root == "" {
		return "", "the models directory is not configured on this server (infra.paths)"
	}
	if strings.ContainsAny(path, "\t\n;|&$`") {
		return "", "contains disallowed characters"
	}
	full := path
	if !filepath.IsAbs(path) {
		full = filepath.Join(root, path)
	}
	full = filepath.Clean(full)
	rel, err := filepath.Rel(filepath.Clean(root), full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "must be inside the models directory (" + root + ")"
	}
	return full, ""
}

// Validate checks an Artifact as it would be written and reads the file facts off the file. currentID is the Artifact being edited (0 for a new
// one). On success the returned Artifact is a with its file facts filled in (size and, for GGUF, architecture, trained context, parameter count and
// quantization) and IsAuxiliary set for an mmproj; that is the row to store.
func (w ArtifactWrites) Validate(ctx context.Context, a Artifact, currentID int64) (*Invalid, Artifact, error) {
	fields := map[string]string{}
	codes := map[string]string{}
	cat := w.Cat
	// exists reports whether a lookup found its row; any failure other than "not found" is an error, never "does not exist".
	exists := func(err error) (bool, error) {
		if err == nil {
			return true, nil
		}
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if a.VariantID == 0 {
		fields["variant_id"], codes["variant_id"] = "is required", "required"
	} else {
		_, err := cat.GetVariant(ctx, a.VariantID)
		ok, err := exists(err)
		if err != nil {
			return nil, a, err
		}
		if !ok {
			fields["variant_id"] = "does not exist"
		}
	}
	switch a.ArtifactType {
	case "weight", "mmproj", "tokenizer":
	default:
		fields["artifact_type"] = "must be one of: weight, mmproj, tokenizer"
	}
	var format Format
	if a.FormatID == 0 {
		fields["format_id"], codes["format_id"] = "is required", "required"
	} else {
		formats, err := cat.ListFormats(ctx)
		if err != nil {
			return nil, a, err
		}
		for _, f := range formats {
			if f.ID == a.FormatID {
				format = f
			}
		}
		if format.ID == 0 {
			fields["format_id"] = "does not exist"
		}
	}
	if a.QuantizationID != 0 {
		quants, err := cat.ListQuantizations(ctx)
		if err != nil {
			return nil, a, err
		}
		found := false
		for _, q := range quants {
			found = found || q.ID == a.QuantizationID
		}
		if !found {
			fields["quantization_id"] = "does not exist"
		}
	}
	if a.FilePath == "" {
		fields["file_path"], codes["file_path"] = "is required", "required"
		return &Invalid{Fields: fields, Codes: codes}, a, nil
	}
	full, msg := w.ResolveFile(a.FilePath)
	if msg != "" {
		fields["file_path"] = msg
		return &Invalid{Fields: fields, Codes: codes}, a, nil
	}
	info, err := os.Stat(full)
	if err != nil {
		fields["file_path"] = "does not exist on this host: " + full
		return &Invalid{Fields: fields, Codes: codes}, a, nil
	}
	a.FileSizeBytes, a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType = 0, "", 0, "", ""
	isGGUF := strings.EqualFold(format.Name, "GGUF")
	switch {
	case isGGUF && info.IsDir():
		fields["file_path"] = "a GGUF artifact must be a file, not a directory"
	case isGGUF:
		md, err := gguf.ReadMetadata(full)
		if err != nil {
			fields["file_path"] = "is not a readable GGUF file: " + err.Error()
			break
		}
		a.FileSizeBytes, a.GGUFArch, a.GGUFQuantType, a.GGUFTrainedCtx = md.FileSizeBytes, md.Architecture, md.QuantType, md.TrainedCtx
		if md.ParameterCount > 0 {
			a.GGUFParameterCount = strconv.FormatInt(md.ParameterCount, 10)
		}
	case !info.IsDir():
		a.FileSizeBytes = info.Size()
	}
	// Two live artifacts for one file would double-count it in every memory estimate.
	all, err := cat.ListArtifacts(ctx)
	if err != nil {
		return nil, a, err
	}
	for _, o := range all {
		if o.ID == currentID || o.VariantID != a.VariantID {
			continue
		}
		if other, _ := w.ResolveFile(o.FilePath); other == full && o.ArtifactType == a.ArtifactType {
			fields["file_path"] = fmt.Sprintf("this variant already has a %s artifact for that file (id %d)", a.ArtifactType, o.ID)
		}
	}
	a.IsAuxiliary = a.IsAuxiliary || a.ArtifactType == "mmproj"
	if len(fields) > 0 {
		return &Invalid{Fields: fields, Codes: codes}, a, nil
	}
	return nil, a, nil
}

// Create validates and saves a new Artifact and returns it as stored.
func (w ArtifactWrites) Create(ctx context.Context, a Artifact) (Artifact, error) {
	inv, row, err := w.Validate(ctx, a, 0)
	if err != nil {
		return Artifact{}, err
	}
	if inv != nil {
		return Artifact{}, inv
	}
	row.ID = 0
	id, err := w.Cat.CreateArtifact(ctx, row)
	if err != nil {
		return Artifact{}, err
	}
	w.changed()
	return w.Cat.GetArtifact(ctx, id)
}

// Update validates and replaces the Artifact with the given id (a full replace; the file facts are re-read from the file) and returns it as stored.
func (w ArtifactWrites) Update(ctx context.Context, id int64, a Artifact) (Artifact, error) {
	if _, err := w.Cat.GetArtifact(ctx, id); err != nil {
		return Artifact{}, err
	}
	inv, row, err := w.Validate(ctx, a, id)
	if err != nil {
		return Artifact{}, err
	}
	if inv != nil {
		return Artifact{}, inv
	}
	row.ID = id
	if err := w.Cat.UpdateArtifact(ctx, row); err != nil {
		return Artifact{}, err
	}
	w.changed()
	return w.Cat.GetArtifact(ctx, id)
}

// Delete removes the Artifact row, never the file on disk, and refuses while a Config uses it as its weight or mmproj. A failed Config list refuses it
// too: "nothing uses it" is not the same as "could not tell".
func (w ArtifactWrites) Delete(ctx context.Context, id int64) error {
	cfgs, err := w.Cat.ListConfigs(ctx)
	if err != nil {
		return err
	}
	var users []string
	for _, c := range cfgs {
		if c.WeightArtifactID == id || c.MMProjArtifactID == id {
			users = append(users, c.Name)
		}
	}
	if len(users) > 0 {
		return &ArtifactInUseError{ID: id, Configs: users}
	}
	if err := w.Cat.DeleteArtifact(ctx, id); err != nil {
		return err
	}
	w.changed()
	return nil
}

// ArtifactGGUFFacts are the facts a GGUF header gives about an Artifact.
type ArtifactGGUFFacts struct {
	Arch           string
	TrainedCtx     int
	ParameterCount string
	QuantType      string
}

// ArtifactFile is what a (re)downloaded file changes about its Artifact: where it is and what the file says about itself.
type ArtifactFile struct {
	FilePath      string
	FileSizeBytes int64
	SHA256        string
	// GGUF, when set, replaces the stored header facts; nil leaves them.
	GGUF *ArtifactGGUFFacts
}

// SetFile points an Artifact at a new file and records its size, hash and header facts, changing nothing else. It re-reads the row first, so a
// writer that read the Artifact before a long download cannot write that old copy back over an edit made in the meantime. The caller has just
// written the file, so unlike Update this does not re-inspect it.
func (w ArtifactWrites) SetFile(ctx context.Context, id int64, f ArtifactFile) (Artifact, error) {
	a, err := w.Cat.GetArtifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	a.FilePath, a.FileSizeBytes, a.SHA256 = f.FilePath, f.FileSizeBytes, f.SHA256
	if f.GGUF != nil {
		a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType = f.GGUF.Arch, f.GGUF.TrainedCtx, f.GGUF.ParameterCount, f.GGUF.QuantType
	}
	if err := w.Cat.UpdateArtifact(ctx, a); err != nil {
		return Artifact{}, err
	}
	w.changed()
	return a, nil
}

// BackfillGGUFFacts fills in the header facts of an Artifact that has none, and only then: it re-reads the row and does nothing (false) when the
// facts were set in the meantime, so a backfill that listed the Artifacts earlier cannot overwrite a newer value.
func (w ArtifactWrites) BackfillGGUFFacts(ctx context.Context, id int64, facts ArtifactGGUFFacts) (bool, error) {
	a, err := w.Cat.GetArtifact(ctx, id)
	if err != nil {
		return false, err
	}
	if a.GGUFArch != "" {
		return false, nil
	}
	a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType = facts.Arch, facts.TrainedCtx, facts.ParameterCount, facts.QuantType
	if err := w.Cat.UpdateArtifact(ctx, a); err != nil {
		return false, err
	}
	w.changed()
	return true, nil
}

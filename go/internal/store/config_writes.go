// SPDX-License-Identifier: Apache-2.0

package store

// Writes to a Config (create, update, delete) with their rules in one place. The HTTP handler decodes and translates; this module decides what
// a valid Config is, saves it, and tells the world it changed. Anything else that writes Configs (smith's build refresh, the CLI) can use the
// same module instead of writing around it (ADR-0020, rule 1); today only the HTTP handlers do.
//
// A write returns *ConfigInvalid for the operator's mistakes (field -> message, optionally field -> stable code), ErrConfigLoaded for a delete
// of a Config that is running, ErrNotFound when the Config does not exist, and the underlying error when the store itself fails: a failed check
// is never a passed check.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// ModeNamePattern is what a Config (or model alias, or service) name must match: a lowercase identifier.
const ModeNamePattern = `^[a-z0-9][a-z0-9\-_]{0,63}$`

var modeNameRE = regexp.MustCompile(ModeNamePattern)

var validModalities = map[string]bool{"text": true, "vision": true, "audio": true}

// validOutputModalities is the output-side enum (ADR-0021): what a model can produce.
var validOutputModalities = map[string]bool{OutputText: true, OutputAudio: true, OutputDecisions: true}

// ValidateModalityList checks every entry against the input-modality enum and reports the first offender by name ("" when all are known).
func ValidateModalityList(mods []string) string {
	for _, m := range mods {
		if !validModalities[m] {
			return fmt.Sprintf("unknown modality %q (must be text, vision, or audio)", m)
		}
	}
	return ""
}

// ValidateOutputModalityList is ValidateModalityList for the output enum.
func ValidateOutputModalityList(mods []string) string {
	for _, m := range mods {
		if !validOutputModalities[m] {
			return fmt.Sprintf("unknown output modality %q (must be text, audio, or decisions)", m)
		}
	}
	return ""
}

// Invalid is a catalog row the rules refuse: field name -> what is wrong, and for some fields a stable code the UI translates. Config, Build and
// Artifact writes all return it.
type Invalid struct {
	Fields map[string]string
	Codes  map[string]string
}

func (e *Invalid) Error() string { return fmt.Sprintf("store: invalid: %v", e.Fields) }

// ConfigInvalid is Invalid under the name the Config writes used first.
type ConfigInvalid = Invalid

// ErrConfigLoaded refuses deleting a Config that a slot is running; Force overrides it.
var ErrConfigLoaded = errors.New("store: config is currently loaded")

// ConfigWrites applies the rules and saves. It holds no state; build one per use.
type ConfigWrites struct {
	Cat Catalog
	// Changed, if set, runs after every successful write (the cache invalidation hook).
	Changed func()
	// Loaded reports whether a Config name is running in a slot; nil means nothing is.
	Loaded func(name string) bool
}

// Validate checks a Config against the field rules and the catalog. excludeID is the Config being edited (0 for a new one), so it doesn't
// collide with its own name. The returned *ConfigInvalid is nil when the Config is acceptable; err is a failure to check at all.
func (w ConfigWrites) Validate(ctx context.Context, c Config, excludeID int64) (*ConfigInvalid, error) {
	fields := map[string]string{}
	codes := map[string]string{}
	// must_be_one_of is deliberately not used for the status, visibility and reasoning_effort_default checks: a field-level code carries no
	// {{allowed}} parameter, so the UI would render the raw template. Those fields keep their plain message.
	if !modeNameRE.MatchString(c.Name) {
		fields["name"] = "must match " + ModeNamePattern
	}
	if c.Modalities != nil {
		if msg := ValidateModalityList(*c.Modalities); msg != "" {
			fields["modalities"] = msg
		}
	}
	if c.OutputModalities != nil {
		if msg := ValidateOutputModalityList(*c.OutputModalities); msg != "" {
			fields["output_modalities"] = msg
		}
	}
	if err := w.checkReferences(ctx, c, excludeID, fields, codes); err != nil {
		return nil, err
	}
	if c.NCtx < 0 {
		fields["n_ctx"] = "must be ≥ 0"
		codes["n_ctx"] = "must_be_non_negative_integer"
	}
	switch c.Status {
	case "", "unverified", "verified":
	default:
		fields["status"] = "must be unverified or verified"
	}
	switch c.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	switch c.ReasoningEffortDefault {
	case "", "none", "low", "medium", "high":
	default:
		fields["reasoning_effort_default"] = "must be none, low, medium, or high"
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &ConfigInvalid{Fields: fields, Codes: codes}, nil
}

// checkReferences is the referential part of Validate: name uniqueness and that every id the Config points at exists (and is the right type).
func (w ConfigWrites) checkReferences(ctx context.Context, c Config, excludeID int64, fields, codes map[string]string) error {
	cat := w.Cat
	// Name uniqueness (unless updating the same config).
	existing, err := cat.ConfigByName(ctx, c.Name)
	switch {
	case err == nil && existing.ID != excludeID:
		fields["name"] = "already exists"
		codes["name"] = "already_exists"
	case err != nil && !errors.Is(err, ErrNotFound):
		return err
	}
	// refExists reports whether a row looked up by id is there; a lookup that fails for any reason other than "not found" is an error.
	refExists := func(err error) (bool, error) {
		if err == nil {
			return true, nil
		}
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if c.VariantID == 0 {
		fields["variant_id"], codes["variant_id"] = "is required", "required"
	} else {
		_, err := cat.GetVariant(ctx, c.VariantID)
		ok, err := refExists(err)
		if err != nil {
			return err
		}
		if !ok {
			fields["variant_id"] = "does not exist"
		}
	}
	if c.WeightArtifactID == 0 {
		fields["weight_artifact_id"], codes["weight_artifact_id"] = "is required", "required"
	} else {
		a, err := cat.GetArtifact(ctx, c.WeightArtifactID)
		ok, err := refExists(err)
		if err != nil {
			return err
		}
		if !ok {
			fields["weight_artifact_id"] = "does not exist"
		} else if a.ArtifactType != "weight" {
			fields["weight_artifact_id"] = "must be a weight artifact"
		}
	}
	if c.EngineID == 0 {
		fields["engine_id"], codes["engine_id"] = "is required", "required"
	} else {
		// There is no get-by-id for engines; the list is a handful of rows.
		engines, err := cat.ListEngines(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, e := range engines {
			found = found || e.ID == c.EngineID
		}
		if !found {
			fields["engine_id"] = "does not exist"
		}
	}
	// A Config with no Build has no defined backend or binary; every Config must reference a real Build explicitly.
	if c.BuildID == 0 {
		fields["build_id"], codes["build_id"] = "is required", "required"
	} else {
		_, err := cat.GetBuild(ctx, c.BuildID)
		ok, err := refExists(err)
		if err != nil {
			return err
		}
		if !ok {
			fields["build_id"] = "does not exist"
		}
	}
	if c.MMProjArtifactID != 0 {
		a, err := cat.GetArtifact(ctx, c.MMProjArtifactID)
		ok, err := refExists(err)
		if err != nil {
			return err
		}
		if !ok {
			fields["mmproj_artifact_id"] = "does not exist"
		} else if a.ArtifactType != "mmproj" {
			fields["mmproj_artifact_id"] = "must be an mmproj artifact"
		}
	}
	if c.CapabilityTierID != 0 {
		_, err := cat.GetCapabilityTier(ctx, c.CapabilityTierID)
		ok, err := refExists(err)
		if err != nil {
			return err
		}
		if !ok {
			fields["capability_tier_id"] = "does not exist"
		}
	}
	return nil
}

func (w ConfigWrites) changed() {
	if w.Changed != nil {
		w.Changed()
	}
}

// Create validates and saves a new Config and returns it as stored.
func (w ConfigWrites) Create(ctx context.Context, c Config) (Config, error) {
	if inv, err := w.Validate(ctx, c, 0); err != nil {
		return Config{}, err
	} else if inv != nil {
		return Config{}, inv
	}
	id, err := w.Cat.CreateConfig(ctx, c)
	if err != nil {
		return Config{}, err
	}
	w.changed()
	return w.Cat.GetConfig(ctx, id)
}

// Update validates and replaces the Config with c.ID (a full replace: c carries every writable field) and returns it as stored.
func (w ConfigWrites) Update(ctx context.Context, c Config) (Config, error) {
	if inv, err := w.Validate(ctx, c, c.ID); err != nil {
		return Config{}, err
	} else if inv != nil {
		return Config{}, inv
	}
	if err := w.Cat.UpdateConfig(ctx, c); err != nil {
		return Config{}, err
	}
	w.changed()
	return w.Cat.GetConfig(ctx, c.ID)
}

// Delete removes the Config with the given id and returns what it removed. A Config that is running is refused (ErrConfigLoaded) unless force.
func (w ConfigWrites) Delete(ctx context.Context, id int64, force bool) (Config, error) {
	c, err := w.Cat.GetConfig(ctx, id)
	if err != nil {
		return Config{}, err
	}
	if !force && w.Loaded != nil && w.Loaded(c.Name) {
		return c, ErrConfigLoaded
	}
	if err := w.Cat.DeleteConfig(ctx, id); err != nil {
		return Config{}, err
	}
	w.changed()
	return c, nil
}

// SetBuild repoints a Config at another Build and changes nothing else. It re-reads the Config first, so it never writes back a stale copy: a
// writer that holds a Config for minutes (smith's build refresh benchmarks a candidate build before it promotes it) must not overwrite an edit made
// in the meantime, which is what a full UpdateConfig of its old copy would do. Only the new Build is checked (it must exist); the rest of the
// Config is left as it is, valid or not, so a revert can never be refused for an unrelated field.
func (w ConfigWrites) SetBuild(ctx context.Context, configID, buildID int64) (Config, error) {
	c, err := w.Cat.GetConfig(ctx, configID)
	if err != nil {
		return Config{}, err
	}
	if buildID == 0 {
		return Config{}, &ConfigInvalid{Fields: map[string]string{"build_id": "is required"}, Codes: map[string]string{"build_id": "required"}}
	}
	if _, err := w.Cat.GetBuild(ctx, buildID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Config{}, &ConfigInvalid{Fields: map[string]string{"build_id": "does not exist"}}
		}
		return Config{}, err
	}
	if c.BuildID == buildID {
		return c, nil
	}
	c.BuildID = buildID
	if err := w.Cat.UpdateConfig(ctx, c); err != nil {
		return Config{}, err
	}
	w.changed()
	return c, nil
}

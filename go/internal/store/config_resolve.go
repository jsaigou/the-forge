// SPDX-License-Identifier: Apache-2.0

package store

// The catalog join from a Config to what it can do: Config -> Variant -> Model, plus the linked mmproj Artifact. Four callers used to walk it
// themselves (a0's request gates, a0's /v1/models, and the dashboard cards), each with its own idea of what a failed lookup means. They now share
// this one function, so a0 and the dashboard can never disagree about a Config, and there is one failure rule:
//
//   - a lookup that fails (the store errors, or the Variant or Model is not there) is an error: the answer is UNKNOWN. The caller decides what
//     unknown means for it (a0's gates fail open: an unresolvable catalog never blocks a request; /v1/models leaves `architecture` out);
//   - a linked mmproj Artifact that does not exist counts as missing, so the Config does not claim a capability whose file is gone. (With foreign
//     keys on the column is cleared when its Artifact is deleted, so this is a defensive rule, not a path real data takes.)
//
// The lookup is a seam with two adapters: CatalogLookup reads live, by id, for the per-request gates; SnapshotLookup answers from rows the caller
// already loaded, for listings and cards. This module caches nothing; each caller keeps its own freshness.

import (
	"context"
	"errors"
	"fmt"
)

// ConfigLookup is the rows the join needs, by id. A row that is not there is an error wrapping ErrNotFound.
type ConfigLookup interface {
	Variant(ctx context.Context, id int64) (Variant, error)
	Model(ctx context.Context, id int64) (Model, error)
	Artifact(ctx context.Context, id int64) (Artifact, error)
}

// ResolvedConfig is what one Config can do, with the Model it resolves to.
type ResolvedConfig struct {
	Model  Model
	Input  ModalityResolution
	Output OutputModalityResolution
}

// ResolveConfig walks Config -> Variant -> Model (-> mmproj Artifact) and applies ResolveModalities and ResolveOutputModalities. The error means
// the answer is unknown; see the package comment above for what each caller does with that.
func ResolveConfig(ctx context.Context, lk ConfigLookup, c Config) (ResolvedConfig, error) {
	v, err := lk.Variant(ctx, c.VariantID)
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("resolve config %q: variant: %w", c.Name, err)
	}
	mdl, err := lk.Model(ctx, v.ModelID)
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("resolve config %q: model: %w", c.Name, err)
	}
	mmprojMissing := false
	if c.MMProjArtifactID != 0 {
		a, err := lk.Artifact(ctx, c.MMProjArtifactID)
		switch {
		case err == nil:
			mmprojMissing = a.Missing
		case errors.Is(err, ErrNotFound):
			mmprojMissing = true // the linked file's row is gone: do not claim what it would have enabled
		default:
			return ResolvedConfig{}, fmt.Errorf("resolve config %q: mmproj: %w", c.Name, err)
		}
	}
	return ResolvedConfig{
		Model:  mdl,
		Input:  ResolveModalities(c, mdl, mmprojMissing),
		Output: ResolveOutputModalities(c, mdl, mmprojMissing),
	}, nil
}

// CatalogLookup is the live adapter: one point query per row, for callers on the request path.
func CatalogLookup(cat Catalog) ConfigLookup { return catalogLookup{cat} }

type catalogLookup struct{ cat Catalog }

func (l catalogLookup) Variant(ctx context.Context, id int64) (Variant, error) {
	return l.cat.GetVariant(ctx, id)
}
func (l catalogLookup) Model(ctx context.Context, id int64) (Model, error) {
	return l.cat.GetModel(ctx, id)
}
func (l catalogLookup) Artifact(ctx context.Context, id int64) (Artifact, error) {
	return l.cat.GetArtifact(ctx, id)
}

// SnapshotLookup is the in-memory adapter over rows the caller already holds, for listings and cards that resolve many Configs at once.
func SnapshotLookup(variants []Variant, models []Model, artifacts []Artifact) ConfigLookup {
	l := snapshotLookup{
		variants:  make(map[int64]Variant, len(variants)),
		models:    make(map[int64]Model, len(models)),
		artifacts: make(map[int64]Artifact, len(artifacts)),
	}
	for _, v := range variants {
		l.variants[v.ID] = v
	}
	for _, m := range models {
		l.models[m.ID] = m
	}
	for _, a := range artifacts {
		l.artifacts[a.ID] = a
	}
	return l
}

type snapshotLookup struct {
	variants  map[int64]Variant
	models    map[int64]Model
	artifacts map[int64]Artifact
}

func (l snapshotLookup) Variant(_ context.Context, id int64) (Variant, error) {
	if v, ok := l.variants[id]; ok {
		return v, nil
	}
	return Variant{}, fmt.Errorf("%w: variant %d", ErrNotFound, id)
}
func (l snapshotLookup) Model(_ context.Context, id int64) (Model, error) {
	if m, ok := l.models[id]; ok {
		return m, nil
	}
	return Model{}, fmt.Errorf("%w: model %d", ErrNotFound, id)
}
func (l snapshotLookup) Artifact(_ context.Context, id int64) (Artifact, error) {
	if a, ok := l.artifacts[id]; ok {
		return a, nil
	}
	return Artifact{}, fmt.Errorf("%w: artifact %d", ErrNotFound, id)
}

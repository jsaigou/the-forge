// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// failingLookup is a ConfigLookup whose chosen table errors with a store failure (not a missing row).
type failingLookup struct {
	store.ConfigLookup
	failVariant, failModel, failArtifact bool
}

var errDisk = errors.New("disk I/O error")

func (l failingLookup) Variant(ctx context.Context, id int64) (store.Variant, error) {
	if l.failVariant {
		return store.Variant{}, errDisk
	}
	return l.ConfigLookup.Variant(ctx, id)
}
func (l failingLookup) Model(ctx context.Context, id int64) (store.Model, error) {
	if l.failModel {
		return store.Model{}, errDisk
	}
	return l.ConfigLookup.Model(ctx, id)
}
func (l failingLookup) Artifact(ctx context.Context, id int64) (store.Artifact, error) {
	if l.failArtifact {
		return store.Artifact{}, errDisk
	}
	return l.ConfigLookup.Artifact(ctx, id)
}

func TestResolveConfig(t *testing.T) {
	ctx := context.Background()
	visionModel := store.Model{ID: 10, Modalities: []string{"text", "vision"}}
	variants := []store.Variant{{ID: 1, ModelID: 10}}
	present := store.Artifact{ID: 5, ArtifactType: "mmproj"}
	gone := store.Artifact{ID: 6, ArtifactType: "mmproj", Missing: true}
	lk := store.SnapshotLookup(variants, []store.Model{visionModel}, []store.Artifact{present, gone})

	cases := []struct {
		name       string
		cfg        store.Config
		lk         store.ConfigLookup
		wantInput  []string
		wantNotFnd bool // the error wraps ErrNotFound
		wantErr    bool
	}{
		{name: "mmproj present inherits the model", cfg: store.Config{VariantID: 1, MMProjArtifactID: 5}, lk: lk, wantInput: []string{"text", "vision"}},
		{name: "no mmproj is text only", cfg: store.Config{VariantID: 1}, lk: lk, wantInput: []string{"text"}},
		{name: "mmproj file flagged missing is text only", cfg: store.Config{VariantID: 1, MMProjArtifactID: 6}, lk: lk, wantInput: []string{"text"}},
		{name: "mmproj row gone counts as missing", cfg: store.Config{VariantID: 1, MMProjArtifactID: 99}, lk: lk, wantInput: []string{"text"}},
		{name: "unknown variant is unknown, not text only", cfg: store.Config{VariantID: 2}, lk: lk, wantErr: true, wantNotFnd: true},
		{name: "unknown model is unknown", cfg: store.Config{VariantID: 1}, lk: store.SnapshotLookup(variants, nil, nil), wantErr: true, wantNotFnd: true},
		{name: "variant read failure is unknown", cfg: store.Config{VariantID: 1}, lk: failingLookup{ConfigLookup: lk, failVariant: true}, wantErr: true},
		{name: "model read failure is unknown", cfg: store.Config{VariantID: 1}, lk: failingLookup{ConfigLookup: lk, failModel: true}, wantErr: true},
		// A failing artifact read must not be mistaken for "the row is gone": it is unknown, never "missing".
		{name: "mmproj read failure is unknown", cfg: store.Config{VariantID: 1, MMProjArtifactID: 5}, lk: failingLookup{ConfigLookup: lk, failArtifact: true}, wantErr: true},
		{name: "no mmproj never reads the artifact table", cfg: store.Config{VariantID: 1}, lk: failingLookup{ConfigLookup: lk, failArtifact: true}, wantInput: []string{"text"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ResolveConfig(ctx, tc.lk, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				if tc.wantNotFnd != errors.Is(err, store.ErrNotFound) {
					t.Errorf("ErrNotFound = %v, want %v (%v)", errors.Is(err, store.ErrNotFound), tc.wantNotFnd, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Input.Enabled, tc.wantInput) {
				t.Errorf("input = %v, want %v", got.Input.Enabled, tc.wantInput)
			}
			if got.Model.ID != 10 {
				t.Errorf("model = %+v", got.Model)
			}
		})
	}
}

// The two adapters agree: the live one reads a missing row as ErrNotFound, exactly like the in-memory one.
func TestCatalogLookupMissingRowIsNotFound(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lk := store.CatalogLookup(db.Catalog())
	ctx := context.Background()
	if _, err := lk.Variant(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("variant: %v", err)
	}
	if _, err := lk.Model(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("model: %v", err)
	}
	if _, err := lk.Artifact(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("artifact: %v", err)
	}
}

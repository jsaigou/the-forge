// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"reflect"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func TestResolveModalities(t *testing.T) {
	visionModel := store.Model{Modalities: []string{"text", "vision"}}
	textOnlyModel := store.Model{Modalities: []string{"text"}}
	emptyModalitiesModel := store.Model{Modalities: nil}

	cases := []struct {
		name          string
		cfg           store.Config
		mdl           store.Model
		mmprojMissing bool
		wantEnabled   []string
		wantUnavail   []string
		wantReason    string
	}{
		{
			name:        "explicit override wins verbatim",
			cfg:         store.Config{MMProjArtifactID: 0, Modalities: ptr([]string{"text", "vision"})},
			mdl:         textOnlyModel, // override should win even though the model itself is text-only
			wantEnabled: []string{"text", "vision"},
		},
		{
			name:        "explicit EMPTY override forces text-only",
			cfg:         store.Config{MMProjArtifactID: 42, Modalities: ptr([]string{})},
			mdl:         visionModel, // override should win even though model+mmproj would otherwise allow vision
			wantEnabled: []string{"text"},
		},
		{
			name:        "no mmproj linked is text-only with a gap",
			cfg:         store.Config{MMProjArtifactID: 0, Modalities: nil},
			mdl:         visionModel,
			wantEnabled: []string{"text"},
			wantUnavail: []string{"vision"},
			wantReason:  store.ModalityReasonNoMMProj,
		},
		{
			name:        "no mmproj linked, text-only model has no gap to report",
			cfg:         store.Config{MMProjArtifactID: 0, Modalities: nil},
			mdl:         textOnlyModel,
			wantEnabled: []string{"text"},
		},
		{
			name:          "mmproj missing on disk is text-only with a gap",
			cfg:           store.Config{MMProjArtifactID: 7, Modalities: nil},
			mdl:           visionModel,
			mmprojMissing: true,
			wantEnabled:   []string{"text"},
			wantUnavail:   []string{"vision"},
			wantReason:    store.ModalityReasonMMProjMissing,
		},
		{
			name:        "mmproj linked and present inherits model modalities",
			cfg:         store.Config{MMProjArtifactID: 7, Modalities: nil},
			mdl:         visionModel,
			wantEnabled: []string{"text", "vision"},
		},
		{
			name:        "model with nil modalities resolves to text only",
			cfg:         store.Config{MMProjArtifactID: 7, Modalities: nil},
			mdl:         emptyModalitiesModel,
			wantEnabled: []string{"text"},
		},
		{
			name:        "unknown/zero artifact id must be treated as not-missing",
			cfg:         store.Config{MMProjArtifactID: 0, Modalities: nil},
			mdl:         visionModel,
			// mmprojMissing=true here would be a caller bug (MMProjArtifactID
			// is 0, so there's no artifact to be missing) — confirms the
			// "no mmproj linked" branch is checked first regardless.
			mmprojMissing: true,
			wantEnabled:   []string{"text"},
			wantUnavail:   []string{"vision"},
			wantReason:    store.ModalityReasonNoMMProj,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := store.ResolveModalities(tc.cfg, tc.mdl, tc.mmprojMissing)
			if !reflect.DeepEqual(res.Enabled, tc.wantEnabled) {
				t.Errorf("Enabled = %v, want %v", res.Enabled, tc.wantEnabled)
			}
			if !reflect.DeepEqual(res.Unavailable, tc.wantUnavail) && !(len(res.Unavailable) == 0 && len(tc.wantUnavail) == 0) {
				t.Errorf("Unavailable = %v, want %v", res.Unavailable, tc.wantUnavail)
			}
			if res.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", res.Reason, tc.wantReason)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

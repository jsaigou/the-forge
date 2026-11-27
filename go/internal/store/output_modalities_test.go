package store

import (
	"context"
	"reflect"
	"testing"
)

func TestResolveOutputModalities(t *testing.T) {
	ptr := func(xs ...string) *[]string { return &xs }
	cases := []struct {
		name        string
		c           Config
		m           Model
		mmprojGone  bool
		wantEnabled []string
		wantUnavail []string
		wantReason  string
	}{
		{"default text model", Config{}, Model{}, false, []string{"text"}, nil, ""},
		{"empty model outputs default to text", Config{}, Model{OutputModalities: []string{}}, false, []string{"text"}, nil, ""},
		{"decision model keeps no text", Config{}, Model{OutputModalities: []string{"decisions"}}, false, []string{"decisions"}, nil, ""},
		{"decision model needs no mmproj", Config{MMProjArtifactID: 0}, Model{OutputModalities: []string{"decisions"}}, false, []string{"decisions"}, nil, ""},
		{"speech model with mmproj", Config{MMProjArtifactID: 7}, Model{OutputModalities: []string{"text", "audio"}}, false, []string{"text", "audio"}, nil, ""},
		{"speech model without mmproj loses audio", Config{}, Model{OutputModalities: []string{"text", "audio"}}, false, []string{"text"}, []string{"audio"}, ModalityReasonNoMMProj},
		{"speech model with missing mmproj file", Config{MMProjArtifactID: 7}, Model{OutputModalities: []string{"text", "audio"}}, true, []string{"text"}, []string{"audio"}, ModalityReasonMMProjMissing},
		{"audio-only output with no mmproj falls back to text", Config{}, Model{OutputModalities: []string{"audio"}}, false, []string{"text"}, []string{"audio"}, ModalityReasonNoMMProj},
		{"explicit override wins even without mmproj", Config{OutputModalities: ptr("text", "audio")}, Model{OutputModalities: []string{"text"}}, false, []string{"text", "audio"}, nil, ""},
		{"explicit empty override falls back to text", Config{OutputModalities: ptr()}, Model{OutputModalities: []string{"decisions"}}, false, []string{"text"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOutputModalities(tc.c, tc.m, tc.mmprojGone)
			if !reflect.DeepEqual(got.Enabled, tc.wantEnabled) {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.wantEnabled)
			}
			if !reflect.DeepEqual(got.Unavailable, tc.wantUnavail) {
				t.Errorf("Unavailable = %v, want %v", got.Unavailable, tc.wantUnavail)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// TestOutputModalitiesPersist checks the column plumbing end to end: the default for a model written without outputs,
// an explicit decision model, a nullable config override, and that an update round-trips.
func TestOutputModalitiesPersist(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	plainID, err := cat.CreateModel(ctx, Model{Name: "Plain"})
	if err != nil {
		t.Fatalf("CreateModel plain: %v", err)
	}
	d1ID, err := cat.CreateModel(ctx, Model{Name: "D1", OutputModalities: []string{"decisions"}})
	if err != nil {
		t.Fatalf("CreateModel d1: %v", err)
	}
	plain, err := cat.GetModel(ctx, plainID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain.OutputModalities, []string{"text"}) {
		t.Errorf("model written without outputs = %v, want [text]", plain.OutputModalities)
	}
	d1, err := cat.GetModel(ctx, d1ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d1.OutputModalities, []string{"decisions"}) {
		t.Errorf("d1 outputs = %v, want [decisions]", d1.OutputModalities)
	}

	// ListModels sees the same values.
	all, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, m := range all {
		seen[m.Name] = m.OutputModalities
	}
	if !reflect.DeepEqual(seen["D1"], []string{"decisions"}) || !reflect.DeepEqual(seen["Plain"], []string{"text"}) {
		t.Errorf("ListModels outputs = %v", seen)
	}

	// UpdateModel round-trips and an emptied set normalizes back to text.
	d1.OutputModalities = []string{"text", "audio"}
	if err := cat.UpdateModel(ctx, d1); err != nil {
		t.Fatal(err)
	}
	d1, _ = cat.GetModel(ctx, d1ID)
	if !reflect.DeepEqual(d1.OutputModalities, []string{"text", "audio"}) {
		t.Errorf("after update = %v", d1.OutputModalities)
	}
	d1.OutputModalities = nil
	if err := cat.UpdateModel(ctx, d1); err != nil {
		t.Fatal(err)
	}
	d1, _ = cat.GetModel(ctx, d1ID)
	if !reflect.DeepEqual(d1.OutputModalities, []string{"text"}) {
		t.Errorf("emptied set = %v, want [text]", d1.OutputModalities)
	}

	// Config override: nil stays nil, an explicit set round-trips, and ListConfigs/ConfigByName/GetConfig all agree.
	varID, err := cat.CreateVariant(ctx, Variant{ModelID: d1ID, Name: "v"})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := cat.QuantizationByName(ctx, "Q8_0")
	f, _ := cat.FormatByName(ctx, "GGUF")
	weightID, err := cat.CreateArtifact(ctx, Artifact{VariantID: varID, QuantizationID: q.ID, FormatID: f.ID, FilePath: "d1.gguf", ArtifactType: "weight"})
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := cat.EngineByName(ctx, "llama.cpp")
	override := []string{"decisions"}
	for name, ov := range map[string]*[]string{"cfg-inherit": nil, "cfg-override": &override} {
		id, err := cat.CreateConfig(ctx, Config{Name: name, VariantID: varID, WeightArtifactID: weightID, EngineID: eng.ID, NCtx: 4096, OutputModalities: ov})
		if err != nil {
			t.Fatalf("CreateConfig %s: %v", name, err)
		}
		byID, _ := cat.GetConfig(ctx, id)
		byName, _ := cat.ConfigByName(ctx, name)
		for label, got := range map[string]*[]string{"GetConfig": byID.OutputModalities, "ConfigByName": byName.OutputModalities} {
			if (ov == nil) != (got == nil) || (ov != nil && !reflect.DeepEqual(*got, *ov)) {
				t.Errorf("%s %s OutputModalities = %v, want %v", name, label, got, ov)
			}
		}
	}
	cfgs, err := cat.ListConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfgs {
		if c.Name == "cfg-override" && (c.OutputModalities == nil || !reflect.DeepEqual(*c.OutputModalities, override)) {
			t.Errorf("ListConfigs override = %v", c.OutputModalities)
		}
		if c.Name == "cfg-inherit" && c.OutputModalities != nil {
			t.Errorf("ListConfigs inherit = %v, want nil", *c.OutputModalities)
		}
	}
}

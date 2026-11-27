package registry

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func badgeIDs(bs []Badge) map[string]bool {
	out := map[string]bool{}
	for _, b := range bs {
		out[b.ID] = true
	}
	return out
}

// Output modalities (ADR-0021) earn badges, plain text output does not, and the output badges do not displace the
// existing input ones until the 4-glyph cap forces a choice.
func TestDeriveBadgesOutputModalities(t *testing.T) {
	cases := []struct {
		name      string
		inputs    []string
		outputs   []string
		wantHas   []string
		wantNot   []string
		wantFirst string
	}{
		{"text output has no output badge", []string{"text"}, []string{"text"}, nil, []string{"speech", "decisions"}, ""},
		{"nil outputs have no output badge", []string{"text"}, nil, nil, []string{"speech", "decisions"}, ""},
		{"decision model", []string{"text", "vision"}, []string{store.OutputDecisions}, []string{"decisions", "vision"}, []string{"speech"}, "vision"},
		{"speech model", []string{"text", "audio"}, []string{"text", store.OutputAudio}, []string{"speech", "hearing"}, []string{"decisions"}, "hearing"},
		{"omni keeps all three, input first", []string{"text", "vision", "audio"}, []string{"text", store.OutputAudio}, []string{"vision", "hearing", "speech"}, nil, "vision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveBadges(nil, nil, tc.inputs, tc.outputs)
			ids := badgeIDs(got)
			for _, w := range tc.wantHas {
				if !ids[w] {
					t.Errorf("missing badge %q in %v", w, got)
				}
			}
			for _, n := range tc.wantNot {
				if ids[n] {
					t.Errorf("unexpected badge %q in %v", n, got)
				}
			}
			if tc.wantFirst != "" && (len(got) == 0 || got[0].ID != tc.wantFirst) {
				t.Errorf("first badge = %v, want %q", got, tc.wantFirst)
			}
		})
	}
}

func TestModelOutputModalitiesDefault(t *testing.T) {
	if got := modelOutputModalities(store.Model{}); len(got) != 1 || got[0] != store.OutputText {
		t.Errorf("empty model outputs = %v, want [text]", got)
	}
	if got := modelOutputModalities(store.Model{OutputModalities: []string{store.OutputDecisions}}); len(got) != 1 || got[0] != store.OutputDecisions {
		t.Errorf("decision model outputs = %v", got)
	}
}

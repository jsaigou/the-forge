// SPDX-License-Identifier: Apache-2.0

package speechsetup

import (
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// install writes stand-ins for a recipe's files under root: the right sizes (sparse), the right modes, no contents.
func install(t *testing.T, root, id string) {
	t.Helper()
	r, ok := RecipeByID(id)
	if !ok {
		t.Fatalf("no recipe %s", id)
	}
	for _, f := range r.Files {
		p := path.Join(root, f.Path)
		if err := os.MkdirAll(path.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if f.Executable {
			mode = 0o755
		}
		if err := os.WriteFile(p, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, f.Size); err != nil {
			t.Fatal(err)
		}
	}
}

// Each recipe must be something the pool accepts and the launcher runs: a drift between the recipes and the launch kinds would register engines
// that can never start.
func TestRecipesMatchTheLaunchKinds(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Recipes() {
		if seen[r.ID] || seen["engine "+r.Engine] {
			t.Errorf("%s: id or engine name used twice", r.ID)
		}
		seen[r.ID], seen["engine "+r.Engine] = true, true
		o := r.Occupant(specialist.EngineRoot)
		if err := specialist.Validate(o, specialist.ModelRoots("/var/lib/forge/models", specialist.EngineRoot)); err != nil {
			t.Errorf("%s: the pool refuses it: %v", r.ID, err)
		}
		if mode, ok := specialist.SpeechModeOf(o); !ok || mode != r.Mode || !r.Matches(o) {
			t.Errorf("%s: renders %q (%v), the recipe says %q", r.ID, mode, ok, r.Mode)
		}
		kind, _ := specialist.KindByName(r.Kind)
		files := map[string]bool{}
		for _, f := range r.Files {
			files[path.Join(specialist.EngineRoot, f.Path)] = true
			if (f.Size > 0) != (len(f.SHA256) == 64) {
				t.Errorf("%s: %s must be pinned by both size and digest, or by neither (size %d, digest %q)", r.ID, f.Path, f.Size, f.SHA256)
			}
		}
		if !files[kind.Binary] {
			t.Errorf("%s: the files do not include the program the launcher runs (%s)", r.ID, kind.Binary)
		}
		for name, rel := range r.Paths {
			if !files[path.Join(specialist.EngineRoot, rel)] {
				t.Errorf("%s: parameter %s names a file the recipe does not list: %s", r.ID, name, rel)
			}
		}
	}
	for _, c := range capabilities {
		found := false
		for _, r := range Recipes() {
			found = found || r.Mode == c.mode
		}
		if !found {
			t.Errorf("no recipe provides %s", c.id)
		}
	}
}

func TestCheckNamesEveryProblem(t *testing.T) {
	root := t.TempDir()
	r, _ := RecipeByID(RecipeQwenVoiceDesign)
	if got := r.Check(root); len(got) != len(r.Files) || got[0].Problem != ProblemMissing {
		t.Fatalf("empty root: %+v", got)
	}
	install(t, root, r.ID)
	if got := r.Check(root); len(got) != 0 {
		t.Fatalf("complete install: %+v", got)
	}
	if err := os.Chmod(path.Join(root, qwenServer), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path.Join(root, qwenCodec), 7); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range r.Check(root) {
		got[strings.TrimPrefix(p.Path, root+"/")] = p.Problem
	}
	if len(got) != 2 || got[qwenServer] != ProblemNotExecutable || got[qwenCodec] != ProblemWrongSize {
		t.Fatalf("problems: %v", got)
	}
}

func TestEngineFilesFollowsTheRegistration(t *testing.T) {
	root := t.TempDir()
	r, _ := RecipeByID(RecipeZonos)
	o := r.Occupant(root)
	if got := EngineFiles(root, o); len(got) != len(r.Paths)+1 { // every parameter file and the program
		t.Fatalf("empty root: %+v", got)
	}
	install(t, root, r.ID)
	// The operator's own weights are fine: a registered engine is judged by what it names, not by the recipe's sizes.
	if err := os.Truncate(o.Params["model"], 3); err != nil {
		t.Fatal(err)
	}
	if got := EngineFiles(root, o); len(got) != 0 {
		t.Fatalf("complete: %+v", got)
	}
	// Kokoro names no files in its registration; its recipe says what it reads.
	k, _ := RecipeByID(RecipeKokoro)
	if got := EngineFiles(root, k.Occupant(root)); len(got) != len(k.Files) {
		t.Fatalf("kokoro: %+v", got)
	}
	if got := EngineFiles(root, store.Occupant{Kind: "unheard-of"}); len(got) != 0 {
		t.Fatalf("unknown kind: %+v", got)
	}
}

func TestStatuses(t *testing.T) {
	root := t.TempDir()
	install(t, root, RecipeKokoro)
	other := engine("my-design", "qwen-tts-server", "voicedesign", store.PolicyOnDemand, "")
	by := map[string]RecipeStatus{}
	for _, s := range Statuses(root, []store.Occupant{other}, nil) {
		by[s.ID] = s
	}
	if s := by[RecipeKokoro]; s.State != RecipePresent || s.Problems != nil || s.DownloadBytes != 325532387+28214398 {
		t.Errorf("kokoro: %+v", s)
	}
	if s := by[RecipeQwenVoiceDesign]; s.State != RecipeRegistered || s.Engine != "my-design" {
		t.Errorf("an engine under another name counts: %+v", s)
	}
	if s := by[RecipeZonos]; s.State != RecipeAbsent || len(s.Problems) == 0 {
		t.Errorf("zonos: %+v", s)
	}
}

func TestEvaluate_SaysWhetherToInstallRegisterOrRestore(t *testing.T) {
	root := t.TempDir()
	c := capByID(t, Evaluate(Input{Recipes: Statuses(root, nil, nil)}), CapClone)
	if c.State != StateNotInstalled || c.Code != CodeNotInstalled || c.Action != ActionInstall || c.Recipe != RecipeZonos {
		t.Errorf("nothing there: %+v", c)
	}
	install(t, root, RecipeQwenBase) // the second choice for cloning is the one that is here
	c = capByID(t, Evaluate(Input{Recipes: Statuses(root, nil, nil)}), CapClone)
	if c.State != StateNotRegistered || c.Action != ActionRegister || c.Recipe != RecipeQwenBase {
		t.Errorf("files here: %+v", c)
	}

	z := engine("z", specialist.KindZonos3Server, "", store.PolicyOnDemand, "")
	missing := map[string][]FileProblem{"z": {{Path: "/x/model.gguf", Problem: ProblemMissing}}}
	slots := []specialist.SlotState{{Slot: "s1", State: specialist.StateEmpty}}
	c = capByID(t, Evaluate(Input{Engines: []store.Occupant{z}, Slots: slots, Missing: missing}), CapClone)
	if c.State != StateFilesMissing || c.Usable || c.Action != ActionRestore || !strings.Contains(c.Detail, "/x/model.gguf") {
		t.Errorf("files gone: %+v", c)
	}
	slots = []specialist.SlotState{{Slot: "s1", Occupant: "z", State: specialist.StateRunning}}
	if c = capByID(t, Evaluate(Input{Engines: []store.Occupant{z}, Slots: slots, Missing: missing}), CapClone); c.State != StateReady {
		t.Errorf("a running engine has what it needs: %+v", c)
	}
}

// The installer's lists of engines follow the recipes.
func TestInstallerScriptKnowsTheRecipes(t *testing.T) {
	raw, err := os.ReadFile("../../../installer/speech.sh")
	if err != nil {
		t.Skipf("the installer is not in this tree: %v", err)
	}
	list := func(name string) []string {
		m := regexp.MustCompile(`(?m)^` + name + `="([^"]*)"`).FindStringSubmatch(string(raw))
		if m == nil {
			t.Fatalf("installer/speech.sh has no %s", name)
		}
		got := strings.Fields(m[1])
		sort.Strings(got)
		return got
	}
	var known, installable []string
	for _, r := range Recipes() {
		known = append(known, r.ID)
		if r.Setup != nil {
			installable = append(installable, r.ID)
		}
	}
	sort.Strings(known)
	sort.Strings(installable)
	if got := list("SPEECH_KNOWN"); !slices.Equal(got, known) {
		t.Errorf("SPEECH_KNOWN %v, recipes %v", got, known)
	}
	if got := list("SPEECH_INSTALLABLE"); !slices.Equal(got, installable) {
		t.Errorf("SPEECH_INSTALLABLE %v, installable recipes %v", got, installable)
	}
}

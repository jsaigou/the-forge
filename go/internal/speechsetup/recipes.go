// SPDX-License-Identifier: Apache-2.0

package speechsetup

// Engine recipes (docs/speech-setup-plan.md section 2.1): what a speech engine consists of on disk and how it is registered. They are compiled data,
// reviewed like code, and never editable from the database: a recipe decides which files count as an engine and what the daemon registers for it.
//
// Every path is relative to the engine root (specialist.EngineRoot). Sizes and digests are those of the pinned downloads (the plan's appendix).
// Files built on the host carry neither: only their presence can be checked. How an engine is made on a host is its Setup (setup.go).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// File is one file an engine needs.
type File struct {
	Path       string `json:"path"`             // relative to the engine root
	Size       int64  `json:"size,omitempty"`   // 0: built on the host, any size
	SHA256     string `json:"sha256,omitempty"` // checked after a download, not on every report: the files are gigabytes
	Executable bool   `json:"executable,omitempty"`
}

// When a newly registered engine is kept loaded.
const (
	KeepAlways           = "always"             // always on
	KeepWhenMemoryAllows = "when_memory_allows" // always on if it fits in free memory at registration, otherwise on demand
	KeepOnDemand         = "on_demand"
)

// Recipe is one speech engine this server knows how to set up.
type Recipe struct {
	ID    string
	Label string
	Mode  string // the kind of speech it renders (specialist.Speech*)
	Kind  string // the pool's launch kind
	// Engine is the name the engine is registered under.
	Engine      string
	MemoryBytes int64 // estimate until measured
	Keep        string
	// Paths are the launch parameters that name files (relative to the engine root); Words are the others.
	Paths   map[string]string
	Words   map[string]string
	Files   []File
	License string
	// Setup is how `forge speech install` makes the engine; nil: it can only be registered from files that are there (setup.go).
	Setup *Setup
}

// Recipe ids.
const (
	RecipeKokoro          = "kokoro"
	RecipeZonos           = "zonos"
	RecipeQwenVoiceDesign = "qwen-voicedesign"
	RecipeQwenBase        = "qwen-base"
	RecipeQwenCustomVoice = "qwen-customvoice"
)

const gib = int64(1) << 30

const (
	qwenServer = "qwentts.cpp/build/tts-server"
	qwenCodec  = "qwentts.cpp/models/qwen-tokenizer-12hz-Q8_0.gguf"
	zonosDir   = "models/zonos3-mini/"
)

// qwen builds the recipe of one Qwen3-TTS checkpoint; the three share the server binary and the codec.
func qwen(id, label, engine, mode, keep string, memory, size int64, sha string) Recipe {
	model := "qwentts.cpp/models/qwen-talker-1.7b-" + mode + "-Q8_0.gguf"
	return Recipe{
		ID: id, Label: label, Mode: mode, Kind: "qwen-tts-server", Engine: engine, MemoryBytes: memory, Keep: keep,
		Paths: map[string]string{"model": model, "codec": qwenCodec},
		Words: map[string]string{"alias": mode},
		Files: []File{
			{Path: qwenServer, Executable: true},
			{Path: qwenCodec, Size: 291150624, SHA256: "1883beeed99348fc35e23dd225e9082f93f6f8c109330a33d935baa8acdbfd94"},
			{Path: model, Size: size, SHA256: sha},
		},
		License: "Code MIT, weights Apache-2.0",
	}
}

// recipes is in order of preference: the first recipe of a mode is the one offered when that kind of speech has no engine.
var recipes = []Recipe{
	{
		ID: RecipeKokoro, Label: "Kokoro", Mode: specialist.SpeechKokoro, Kind: "kokoro", Engine: "kokoro", MemoryBytes: 2 * gib, Keep: KeepAlways,
		Files: []File{
			{Path: "kokoro/venv312/bin/python", Executable: true},
			{Path: "kokoro/server.py"},
			{Path: "kokoro/models/kokoro-v1.0.onnx", Size: 325532387, SHA256: "7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5"},
			{Path: "kokoro/models/voices-v1.0.bin", Size: 28214398, SHA256: "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"},
		},
		License: "Model Apache-2.0, kokoro-onnx MIT; installs GPL-3.0 phonemizer-fork",
		Setup:   kokoroSetup,
	},
	{
		ID: RecipeZonos, Label: "ZONOS3-Mini", Mode: specialist.SpeechBase, Kind: specialist.KindZonos3Server, Engine: "zonos3-mini-clone",
		MemoryBytes: 5 * gib / 2, Keep: KeepWhenMemoryAllows,
		Paths: map[string]string{
			"model": zonosDir + "backbone-ptq.gguf", "zac": zonosDir + "zac-keep2.gguf", "spk_encoder": zonosDir + "spk-encoder.gguf",
			"zac_encoder": zonosDir + "zac-encoder.gguf", "ssl_teacher": zonosDir + "ssl-teacher.gguf",
			"laci_en": zonosDir + "laci/zonos3_mini_heads.json", "laci_ja": zonosDir + "laci/zonos3_mini_heads_ja.json",
		},
		Files: []File{
			{Path: "zonos3.cpp/build/zonos3-server", Executable: true},
			{Path: zonosDir + "backbone-ptq.gguf", Size: 663150624, SHA256: "66fceb6d73a8c30bde80249e4112bb308380ab7f3384fa11c8e4048115e4ceec"},
			{Path: zonosDir + "zac-keep2.gguf", Size: 452325440, SHA256: "834e5b70b88294afc3c702a478487e0664dcc6b574c16fc6167d72444aabeafd"},
			{Path: zonosDir + "spk-encoder.gguf", Size: 45385728, SHA256: "9539f340b70be246005403b9c254732e180e85e0b3c57fb1973f0d6bc6c59bdc"},
			{Path: zonosDir + "zac-encoder.gguf", Size: 4034646368, SHA256: "4c4335196486fd24cc56b24a40ac4548f8cc58a6400bce8e4af4e72160b18053"},
			{Path: zonosDir + "ssl-teacher.gguf", Size: 1548700000, SHA256: "59e4f150e1ccdf54e6349b06605c2274f2262139678688025b6614f1393e415a"},
			// Not a launch parameter: the engine reads it from beside the backbone.
			{Path: zonosDir + "steer.gguf", Size: 204960, SHA256: "6e8e3f86e89a09801ce083b9286f43263ec696acfaff72e25aca537aa9efa90c"},
			{Path: zonosDir + "laci/zonos3_mini_heads.json", Size: 7495, SHA256: "407e47472224c603b9c3a969e7175f35b637e0c1995eb606d5128ec1b2e88528"},
			{Path: zonosDir + "laci/zonos3_mini_heads_ja.json", Size: 7584, SHA256: "1da810b373a7ddc591ffc654de363d8024821a407665083748c3eef5bf3e233a"},
		},
		License: "Code MIT, weights Apache-2.0",
	},
	qwen(RecipeQwenVoiceDesign, "Qwen3-TTS voice design", "qwen3-tts-voicedesign", specialist.SpeechVoiceDesign, KeepWhenMemoryAllows,
		3*gib, 2042833824, "575610ab1ddcca4dca6bd9a64bcd859d93bbad8764f9cab24e1dbc0c51f62276"),
	qwen(RecipeQwenBase, "Qwen3-TTS cloning", "qwen3-tts-base", specialist.SpeechBase, KeepOnDemand,
		4*gib, 2079448256, "4b9a33a236908dd9435a42f7a396e38038329d053b704342a6413c08544c4fda"),
	qwen(RecipeQwenCustomVoice, "Qwen3-TTS preset speakers", "qwen3-tts-customvoice", specialist.SpeechCustomVoice, KeepOnDemand,
		4*gib, 2042834304, "cab2cff67a0a557310febe558dc83076b28ed790e491867eb2751759f4cd89fa"),
}

// Recipes returns every recipe, in order of preference. The slices and maps inside are shared: do not change them.
func Recipes() []Recipe { return append([]Recipe(nil), recipes...) }

// RecipeByID returns one recipe.
func RecipeByID(id string) (Recipe, bool) {
	for _, r := range recipes {
		if r.ID == id {
			return r, true
		}
	}
	return Recipe{}, false
}

// DownloadBytes is the total size of the files the recipe downloads.
func (r Recipe) DownloadBytes() int64 {
	var n int64
	for _, f := range r.Files {
		n += f.Size
	}
	return n
}

// Occupant is the pool registration of the recipe's engine under root.
func (r Recipe) Occupant(root string) store.Occupant {
	params := map[string]string{}
	for k, v := range r.Paths {
		params[k] = path.Join(root, v)
	}
	for k, v := range r.Words {
		params[k] = v
	}
	return store.Occupant{
		Name: r.Engine, Label: r.Label, Kind: r.Kind, Params: params, Functions: []string{store.FnSpeech}, MemoryBytes: r.MemoryBytes,
		Policy: store.PolicyOnDemand, Enabled: true, Notes: "registered from recipe " + r.ID,
	}
}

// Matches reports whether o is an engine of this recipe's sort: the same launch kind rendering the same kind of speech, whatever its name and files.
func (r Recipe) Matches(o store.Occupant) bool {
	mode, ok := specialist.SpeechModeOf(o)
	return ok && o.Kind == r.Kind && mode == r.Mode
}

// What is wrong with a file.
const (
	ProblemMissing       = "missing"
	ProblemNotAFile      = "not_a_file"
	ProblemNotExecutable = "not_executable"
	ProblemWrongSize     = "wrong_size"
	ProblemUnreadable    = "unreadable"
)

// FileProblem is one file that is not as an engine needs it.
type FileProblem struct {
	Path    string `json:"path"` // absolute
	Problem string `json:"problem"`
	Detail  string `json:"detail,omitempty"`
}

func checkFile(abs string, want File) (FileProblem, bool) {
	bad := func(problem, detail string) (FileProblem, bool) {
		return FileProblem{Path: abs, Problem: problem, Detail: detail}, true
	}
	st, err := os.Stat(abs) // follows links: a virtualenv's interpreter is one
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return bad(ProblemMissing, "")
	case err != nil:
		return bad(ProblemUnreadable, err.Error())
	case !st.Mode().IsRegular():
		return bad(ProblemNotAFile, "")
	case want.Executable && st.Mode().Perm()&0o111 == 0:
		return bad(ProblemNotExecutable, "")
	case want.Size > 0 && st.Size() != want.Size:
		return bad(ProblemWrongSize, fmt.Sprintf("%d bytes, expected %d", st.Size(), want.Size))
	}
	return FileProblem{}, false
}

// Check lists what keeps the recipe's files under root from being a complete engine; empty means complete. It reads no file contents.
func (r Recipe) Check(root string) []FileProblem {
	out := []FileProblem{}
	for _, f := range r.Files {
		if p, bad := checkFile(path.Join(root, f.Path), f); bad {
			out = append(out, p)
		}
	}
	return out
}

// EngineFiles lists what a registered speech engine needs and does not have: its program, the files its parameters name, and for an engine whose
// files are not parameters (Kokoro) its recipe's. Sizes are not checked: the operator may run other weights than a recipe pins.
func EngineFiles(root string, o store.Occupant) []FileProblem {
	need := map[string]File{}
	add := func(abs string, executable bool) {
		f := need[abs]
		f.Executable = f.Executable || executable
		need[abs] = f
	}
	kind, ok := specialist.KindByName(o.Kind)
	if !ok {
		return []FileProblem{}
	}
	if rel, under := strings.CutPrefix(kind.Binary, specialist.EngineRoot+"/"); under {
		add(path.Join(root, rel), true)
	}
	for name, spec := range kind.Params {
		if v := o.Params[name]; spec.Type == specialist.ParamPath && v != "" {
			add(v, false)
		}
	}
	for _, r := range recipes {
		if r.Kind == o.Kind && len(r.Paths) == 0 {
			for _, f := range r.Files {
				add(path.Join(root, f.Path), f.Executable)
			}
		}
	}
	out := []FileProblem{}
	for abs, f := range need {
		if p, bad := checkFile(abs, f); bad {
			out = append(out, p)
		}
	}
	sortProblems(out)
	return out
}

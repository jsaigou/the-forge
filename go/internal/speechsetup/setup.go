// SPDX-License-Identifier: Apache-2.0

package speechsetup

// How `forge speech install` makes a recipe's engine on a host (docs/speech-setup-plan.md section 5.2). Compiled data like the recipes themselves:
// what is built, from which pinned inputs, and how the result is tried out. A recipe without a Setup can only be registered from files that are there.

import "embed"

//go:embed kokoro/server.py kokoro/requirements.lock
var assets embed.FS

// Command is a program the build needs on the PATH, with the package that provides it.
type Command struct {
	Name string
	DNF  string // package on Fedora and relatives
	APT  string // package on Debian and relatives; "" = not packaged there
	// Hint says how to get it where no package is known.
	Hint string
}

// TrialRequest is one sentence the finished engine must turn into sound before it is put in place.
type TrialRequest struct {
	Name     string
	Input    string
	Voice    string
	Language string
}

// Trial is the trial run: the engine is started on a spare port and asked to speak.
type Trial struct {
	Program  string // under the engine directory
	Argument string // under the engine directory
	PortEnv  string // the environment variable that moves its port
	Health   string
	Speech   []TrialRequest
	// Check is run with the engine's own interpreter and must exit 0: it covers what a trial sentence cannot, a part that fails silently.
	Check string
}

// Setup is how a recipe's engine is made.
type Setup struct {
	// Dir is the engine's directory under the root. Everything the installer makes is in it, and every file of the recipe lies under it.
	Dir       string
	Commands  []Command
	DiskBytes int64 // free space the whole build needs
	// Sources are files written from this program, by their name under Dir.
	Sources map[string]string
	// Python is the version of the interpreter fetched for the engine's virtual environment (Venv, under Dir), filled from the hash-locked Lock.
	Python string
	Venv   string
	Lock   string
	// URLs says where each downloaded file of the recipe comes from (by its path in Recipe.Files).
	URLs  map[string]string
	Trial Trial
}

const kokoroRelease = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/"

var kokoroSetup = &Setup{
	Dir: "kokoro",
	Commands: []Command{
		{Name: "uv", DNF: "uv", Hint: "https://docs.astral.sh/uv/getting-started/installation/"},
		// pyopenjtalk, which the Japanese front end imports, has no prebuilt package: it is compiled.
		{Name: "cmake", DNF: "cmake", APT: "cmake"},
		{Name: "c++", DNF: "gcc-c++", APT: "g++"},
		{Name: "make", DNF: "make", APT: "make"},
	},
	DiskBytes: 3 * gib,
	Sources:   map[string]string{"server.py": "kokoro/server.py"},
	Python:    "3.12",
	Venv:      "venv312",
	Lock:      "kokoro/requirements.lock",
	URLs: map[string]string{
		"kokoro/models/kokoro-v1.0.onnx": kokoroRelease + "kokoro-v1.0.onnx",
		"kokoro/models/voices-v1.0.bin":  kokoroRelease + "voices-v1.0.bin",
	},
	Trial: Trial{
		Program: "venv312/bin/python", Argument: "server.py", PortEnv: "KOKORO_PORT", Health: "/health",
		Speech: []TrialRequest{
			{Name: "English", Input: "The speech engine is ready.", Voice: "af_heart", Language: "English"},
			{Name: "Japanese", Input: "音声エンジンの準備ができました。", Voice: "jf_alpha", Language: "Japanese"},
		},
		Check: `from misaki import ja
phonemes, _ = ja.JAG2P(version="cutlet")("こんにちは、世界。")
assert phonemes.strip(), "no phonemes"`,
	},
}

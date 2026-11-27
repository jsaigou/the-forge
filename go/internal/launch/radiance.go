// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/jsaigou/the-forge/internal/radianceyaml"
	"github.com/jsaigou/the-forge/internal/store"
)

// radianceOwnedFlags are the flags Forge itself supplies (or must never let a config set) on a Radiance command line (ADR-0026 section 5). A config's
// extra_args that repeat one are refused, never merged: the daemon, not the config, decides the model, the served name, the bind address and port, the
// context bound and the access policy. Radiance runs keyless on 127.0.0.1 (operator decision 3), so every spelling of an API key is refused rather than
// forwarded; --radiance-home is refused because the host layout comes from the build row's launch_env, not from a flag a config can point elsewhere.
var radianceOwnedFlags = map[string]string{
	"--model":             "the weight file comes from the config's artifact",
	"--served-model-name": "the served name is the config name",
	"--host":              "Radiance listens on 127.0.0.1 only",
	"--port":              "the port belongs to the slot",
	"--max-model-len":     "the context bound is the config's n_ctx",
	"--api-key":           "Radiance runs keyless on 127.0.0.1; a key never goes in argv",
	"--api-key-env":       "Radiance runs keyless on 127.0.0.1",
	"--radiance-home":     "the host layout comes from the build's launch_env",
	"--help":              "it would print usage and exit",
	"-h":                  "it would print usage and exit",
}

// radianceRequired are the flags every Radiance config must carry explicitly, each with the rule for its value. They set the footprint or the
// behaviour of the engine, so each is a per-config decision under the plan's Config derivation procedure and is never supplied, defaulted or inferred
// here: --max-num-seqs sizes the pools; --num-speculative-tokens defaults to "auto" in the binary (not 0); --tp has no stated default in --help.
// (--max-model-len is always passed by the builder, since 0 would mean the model's training context.)
var radianceRequired = []struct {
	flag  string
	check func(v string) bool
	want  string
}{
	{"--max-num-seqs", positiveInt, "a positive integer"},
	{"--num-speculative-tokens", func(v string) bool { return v == "auto" || nonNegInt(v) }, "auto or a non-negative integer"},
	{"--tp", positiveInt, "a positive integer"},
}

func nonNegInt(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n >= 0 && strconv.Itoa(n) == v
}

func positiveInt(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n >= 1 && strconv.Itoa(n) == v
}

// buildRadiance builds the Radiance launch:
//
//	exe  = the build's binary
//	argv = --model P --served-model-name A --host 127.0.0.1 --port N --max-model-len C <extra_args...>
//	env  = the build row's launch_env, and nothing else
//
// No card-budget flag is supplied and none is refused: --gpu-memory-mib and friends change the footprint, so they belong to the config's tuple (the
// fit gate compares the whole flag set), and whether one helps is a per-model decision. --tp, --num-speculative-tokens and --max-num-seqs are
// required in extra_args and never supplied (see radianceRequired): the binary's --help states no default for --tp, defaults speculation to "auto",
// and sizes its pools from seqs.
//
// Radiance matches flag names exactly, so "--flag=value" is not understood by it; the builder refuses that spelling instead of letting the engine fail
// at start. Every element of extra_args is one token (no whitespace): nothing is ever interpreted by a shell.
func buildRadiance(in Input) (Launch, error) {
	if err := checkRadianceInput(in); err != nil {
		return Launch{}, err
	}
	if err := checkRadianceArgs(in.ExtraArgs); err != nil {
		return Launch{}, err
	}
	argv := []string{
		"--model", in.ModelPath,
		"--served-model-name", in.Alias,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(in.Port),
		"--max-model-len", strconv.Itoa(in.Context),
	}
	argv = append(argv, in.ExtraArgs...)
	env := make(map[string]string, len(in.LaunchEnv))
	for k, v := range in.LaunchEnv {
		env[k] = v
	}
	return Launch{Exe: in.Binary, Argv: argv, Env: env}, nil
}

// buildRadiance2 is buildRadiance for a Radiance 2.x engine. The row stores exactly the same flag list (so the spec hash, launch-check and drift
// detection mean what they always meant); `forge slot-exec` turns it into the YAML file the 2.x binary reads when the slot launches. The conversion is
// tried here as well, so a config that could not be launched is refused when the row is built and not later, from a unit that is restarting.
func buildRadiance2(in Input) (Launch, error) {
	// The 2.x engine refuses a model id with a ":" (reserved for base:adapter) as well as whitespace (checked for every Radiance kind): "'x:y' cannot
	// be a model id" is what `radiance --config FILE --check` says, so the row is refused here instead of the engine at start.
	if strings.Contains(in.Alias, ":") {
		return Launch{}, fmt.Errorf("%w: alias %q contains ':', which a Radiance 2.x model id may not (it is reserved for base:adapter)", ErrInvalid, in.Alias)
	}
	l, err := buildRadiance(in)
	if err != nil {
		return Launch{}, err
	}
	if _, err := radianceyaml.FromArgv(l.Argv); err != nil {
		return Launch{}, fmt.Errorf("%w: the arguments cannot be written as a Radiance 2.x settings file: %v", ErrInvalid, err)
	}
	return l, nil
}

func checkRadianceInput(in Input) error {
	if !store.ValidSlotLaunchSlot(in.Slot) {
		return fmt.Errorf("%w: slot %q must match ^[a-z0-9][a-z0-9-]{0,31}$", ErrInvalid, in.Slot)
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%w: port %d must be between 1 and 65535", ErrInvalid, in.Port)
	}
	if in.Binary == "" {
		return fmt.Errorf("%w: a Radiance build needs a binary path", ErrInvalid)
	}
	// 0 would make Radiance take the model's training context, an unrecorded value the memory tuple could not name.
	if in.Context < 1 {
		return fmt.Errorf("%w: context %d must be positive: the Radiance memory figure is recorded per --max-model-len", ErrInvalid, in.Context)
	}
	if in.Alias == "" {
		return fmt.Errorf("%w: alias is required", ErrInvalid)
	}
	// Radiance accepts a repo id where a path is expected and would fetch it; only a local file is allowed.
	if in.ModelPath == "" || !filepath.IsAbs(in.ModelPath) {
		return fmt.Errorf("%w: model path %q must be an absolute path to a local file (Radiance would treat anything else as a hub repo id to download)", ErrInvalid, in.ModelPath)
	}
	if hasControlOrSpace(in.Alias) {
		return fmt.Errorf("%w: alias must not contain whitespace or control characters", ErrInvalid)
	}
	if strings.ContainsAny(in.ModelPath, "\x00\n\r") {
		return fmt.Errorf("%w: model path contains NUL or a newline", ErrInvalid)
	}
	return nil
}

func hasControlOrSpace(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func checkRadianceArgs(args []string) error {
	for i, a := range args {
		if a == "" || hasControlOrSpace(a) {
			return fmt.Errorf("%w: extra arg %d must be one token without whitespace or control characters", ErrInvalid, i)
		}
	}
	seen := map[string]int{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !isFlagToken(a) {
			if i == 0 {
				return fmt.Errorf("%w: extra args must start with a flag, got %q", ErrInvalid, a)
			}
			continue // a value of the previous flag
		}
		if strings.HasPrefix(a, "--") && strings.Contains(a, "=") {
			return fmt.Errorf("%w: extra arg %d (%q) uses the --flag=value spelling; Radiance matches flag names exactly, give the flag and its value as two elements", ErrInvalid, i, flagName(a))
		}
		if why, owned := radianceOwnedFlags[a]; owned {
			return fmt.Errorf("%w: %s is set by Forge (%s) and cannot be given in extra_args", ErrInvalid, a, why)
		}
		for _, r := range radianceRequired {
			if a != r.flag {
				continue
			}
			seen[a]++
			if seen[a] > 1 {
				return fmt.Errorf("%w: %s is given more than once", ErrInvalid, a)
			}
			if i+1 >= len(args) || isFlagToken(args[i+1]) {
				return fmt.Errorf("%w: %s needs a value", ErrInvalid, a)
			}
			if !r.check(args[i+1]) {
				return fmt.Errorf("%w: %s value %q must be %s", ErrInvalid, a, args[i+1], r.want)
			}
		}
	}
	for _, r := range radianceRequired {
		if seen[r.flag] == 0 {
			return fmt.Errorf("%w: %s is required for a Radiance config: it is an explicit per-config decision (it sets the engine's footprint or behaviour) and is never defaulted", ErrInvalid, r.flag)
		}
	}
	return nil
}

func flagName(a string) string {
	name, _, _ := strings.Cut(a, "=")
	return name
}

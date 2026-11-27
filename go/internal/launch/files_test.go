// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSlotFiles reads an env/args file pair in the format the retired daemon writer produced (/etc/sysconfig/forge-<slot>-env and -args, no longer
// written since Phase 1b step d2) the way the legacy static launcher saw them, and returns the Input a builder would be given for the same service.
// It lives in a _test file: only the golden tests that run the frozen legacy scripts use it.
//
// Env file: KEY=VALUE lines as systemd's EnvironmentFile delivers them for the values checkInput allows (no quotes or backslashes, trimmed); keys the
// launcher does not read (preserved host lines such as HSA_* or LD_LIBRARY_PATH) are ignored here and returned in the second result so the caller can
// see what a row would not carry. Args file: every line that is not blank, verbatim.
func ParseSlotFiles(slot string, port int, envFile, argsFile string) (Input, map[string]string, error) {
	in := Input{Slot: slot, Port: port}
	other := map[string]string{}
	var ctx string
	for _, line := range strings.Split(envFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "FORGE_MODEL_PATH":
			in.ModelPath = v
		case "FORGE_MODEL_ALIAS":
			in.Alias = v
		case "FORGE_CONTEXT":
			ctx = v
		case "FORGE_MMPROJ":
			in.MmprojPath = v
		case "FORGE_BACKEND":
			in.Backend = v
		case "FORGE_LLAMA_BIN":
			in.Binary = v
		default:
			other[k] = v
		}
	}
	if ctx != "" {
		n, err := strconv.Atoi(ctx)
		if err != nil {
			return Input{}, nil, fmt.Errorf("%w: FORGE_CONTEXT %q is not a number", ErrInvalid, ctx)
		}
		in.Context = n
	}
	// ${EXTRA_ARGS[@]} of start-aN.sh: every line of the args file that is not blank (whitespace-only), unmodified.
	for _, l := range strings.Split(argsFile, "\n") {
		if strings.TrimSpace(l) != "" {
			in.ExtraArgs = append(in.ExtraArgs, l)
		}
	}
	return in, other, nil
}

// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"fmt"
	"strconv"
)

// buildLlamaCpp reproduces the llama.cpp tail of start-aN.sh:
//
//	LLAMA_BIN = FORGE_LLAMA_BIN, else the ROCm plain build when FORGE_BACKEND=rocm, else the Vulkan plain build (empty backend = vulkan)
//	exec $LLAMA_BIN --model P --alias A --host 127.0.0.1 --port N --ctx-size C -ngl 99 --metrics [--mmproj M] <args file lines...>
//
// The scripts treat any other backend value as Vulkan; the builder refuses it (a typo must not silently pick a GPU path). The environment added is
// empty: the scripts export nothing on this path.
func buildLlamaCpp(in Input) (Launch, error) {
	exe := in.Binary
	switch in.Backend {
	case "rocm":
		if exe == "" {
			exe = DefaultRocmLlamaServer
		}
	case "", "vulkan":
		if exe == "" {
			exe = DefaultVulkanLlamaServer
		}
	default:
		return Launch{}, fmt.Errorf("%w: llama.cpp backend %q must be vulkan, rocm or empty", ErrInvalid, in.Backend)
	}
	argv := []string{
		"--model", in.ModelPath,
		"--alias", in.Alias,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(in.Port),
		"--ctx-size", strconv.Itoa(in.Context),
		"-ngl", "99",
		"--metrics",
	}
	if in.MmprojPath != "" {
		argv = append(argv, "--mmproj", in.MmprojPath)
	}
	argv = append(argv, in.ExtraArgs...)
	return Launch{Exe: exe, Argv: argv, Env: map[string]string{}}, nil
}

// SPDX-License-Identifier: Apache-2.0

package launch

import "strconv"

// Hard-coded by the vLLM branch of start-aN.sh.
const (
	vllmExe      = "/opt/forge/venv-vllm/bin/vllm"
	vllmRocmRoot = "/opt/rocm-therock-7.13"
)

// buildVLLM reproduces the vLLM branch of start-aN.sh VERBATIM.
//
// KNOWN-DEAD PATH PENDING REINVESTIGATION, REPRODUCED VERBATIM. /opt/rocm-therock-7.13 no longer exists on ForgeHost, and the operator deferred the vLLM
// reinvestigation (ADR-0026 "Deferred: reinvestigate vLLM"). Nothing here is evidence that vLLM launches on this host; it only pins what the script
// did so the conversion changes nothing. Do not "fix" the paths here without that reinvestigation.
//
// Like the script it ignores the binary override and the mmproj, and passes the model as vllm's positional argument:
//
//	LD_LIBRARY_PATH=/opt/rocm-therock-7.13/lib[:$inherited]  ROCM_PATH=HIP_PATH=/opt/rocm-therock-7.13
//	exec /opt/forge/venv-vllm/bin/vllm serve P --host 127.0.0.1 --port N --served-model-name A --max-model-len C <args file lines...>
//
// The script reaches this branch only when FORGE_BACKEND is exactly "vllm"; the kind-based dispatch here also covers kind vllm with backend rocm,
// which the script would have run as llama-server (a "what it should have been" case, not a regression).
func buildVLLM(in Input) (Launch, error) {
	ld := vllmRocmRoot + "/lib"
	if in.InheritedLDLibraryPath != "" {
		ld += ":" + in.InheritedLDLibraryPath
	}
	argv := []string{
		"serve", in.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(in.Port),
		"--served-model-name", in.Alias,
		"--max-model-len", strconv.Itoa(in.Context),
	}
	argv = append(argv, in.ExtraArgs...)
	return Launch{
		Exe:  vllmExe,
		Argv: argv,
		Env: map[string]string{
			"LD_LIBRARY_PATH": ld,
			"ROCM_PATH":       vllmRocmRoot,
			"HIP_PATH":        vllmRocmRoot,
		},
	}, nil
}

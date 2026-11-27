// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// checkInput refuses values for which the builder could not be identical to "daemon writes env + args files, systemd parses the env file, the script
// reads the args file". Each rule names the file-format reason, so a refusal is explainable and a relaxed rule has to argue with it.
func checkInput(in Input) error {
	if !store.ValidSlotLaunchSlot(in.Slot) {
		return fmt.Errorf("%w: slot %q must match ^[a-z0-9][a-z0-9-]{0,31}$", ErrInvalid, in.Slot)
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%w: port %d must be between 1 and 65535", ErrInvalid, in.Port)
	}
	if in.Context < 0 {
		return fmt.Errorf("%w: context %d must not be negative", ErrInvalid, in.Context)
	}
	if in.ModelPath == "" {
		return fmt.Errorf("%w: model path is required", ErrInvalid)
	}
	if in.Alias == "" {
		return fmt.Errorf("%w: alias is required", ErrInvalid)
	}
	// These travel through a systemd EnvironmentFile, which strips surrounding whitespace and interprets quotes and backslashes.
	for name, v := range map[string]string{"model path": in.ModelPath, "alias": in.Alias, "mmproj path": in.MmprojPath, "binary": in.Binary} {
		if err := envFileSafe(name, v); err != nil {
			return err
		}
	}
	// Extra args travel through a one-argument-per-line file whose reader drops blank lines.
	for i, a := range in.ExtraArgs {
		switch {
		case strings.TrimSpace(a) == "":
			return fmt.Errorf("%w: extra arg %d is empty or whitespace-only; the args file cannot carry it (the launcher drops blank lines)", ErrInvalid, i)
		case strings.ContainsAny(a, "\n\x00"):
			return fmt.Errorf("%w: extra arg %d contains a newline or NUL; the args file holds one argument per line", ErrInvalid, i)
		}
	}
	return nil
}

func envFileSafe(name, v string) error {
	if v == "" {
		return nil
	}
	if strings.ContainsAny(v, "\"'\\\n\r\x00") {
		return fmt.Errorf("%w: %s contains a quote, backslash, newline or NUL; the systemd env file would rewrite it", ErrInvalid, name)
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%w: %s has leading or trailing whitespace; the systemd env file would strip it", ErrInvalid, name)
	}
	return nil
}

// secretFlag reports whether a is a flag whose name says it carries a credential. ADR-0026 says a flag whose name contains "key", "token", "secret"
// or "password" is refused; that literal rule would refuse legitimate vLLM flags (--max-num-batched-tokens, --tokenizer), so tokens are matched as
// whole words (--hf-token, --auth-token, --token) and key only as an API key.
func secretFlag(a string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
	name = strings.ReplaceAll(strings.ToLower(name), "_", "-")
	for _, w := range []string{"api-key", "apikey", "secret", "password", "passwd", "credential"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return name == "token" || strings.HasSuffix(name, "-token")
}

// secretEnvName is the ADR's env screen: a name containing KEY, TOKEN, SECRET, PASSWORD or CREDENTIAL.
func secretEnvName(k string) bool {
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

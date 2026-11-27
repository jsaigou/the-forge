// SPDX-License-Identifier: Apache-2.0

package buildguard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Limits on a build's launch_env (ADR-0026 section 5, decided 2026-11-11). The values are caps on what the guarded write API accepts; they are not
// properties of any engine.
const (
	MaxLaunchEnvEntries    = 32
	MaxLaunchEnvNameLen    = 64
	MaxLaunchEnvValueLen   = 4096 // a library path is a colon-separated list
	MaxLaunchEnvTotalBytes = 16384
)

// SettingLaunchEnvRoots ("catalog.launch_env_roots") is the JSON array of directories, besides catalog.build_roots, that a path in a build's
// launch_env may live under. Unset means none: only the build roots count. See CheckLaunchEnv for why it is a separate list.
const SettingLaunchEnvRoots = "catalog.launch_env_roots"

var launchEnvNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// secretNameWords mirrors launch.Check's screen on the same names (ADR-0026 "Common rules"): such a variable is never written to a row.
var secretNameWords = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"}

// reservedNamePrefixes are the namespaces `forge slot-exec` strips from the inherited environment, so a launch_env entry there could never reach
// the engine; refuse it instead of accepting something that is silently dropped.
var reservedNamePrefixes = []string{"FORGE_", "FOUNDRY_"}

// loaderInjectionNames let a process load code chosen by the environment before its own. An engine's host environment never needs them, and
// an allowed-roots check on their values would still leave the choice of arbitrary preloaded code to whoever can write a build row.
var loaderInjectionNames = map[string]bool{"LD_PRELOAD": true, "LD_AUDIT": true, "DYLD_INSERT_LIBRARIES": true}

// secretValuePrefixes are the credential spellings Forge itself issues or handles (its own key prefix, the hub token prefix, HTTP auth schemes);
// a value starting with one (after trimming) is refused whatever the variable is called. The variable-name screen is the general rule.
var secretValuePrefixes = []string{"sk-", "hf_", "bearer ", "basic "}

// pathLike reports whether a launch_env value is meant as a path or a list of paths: any ':'-separated segment starting with '/', '.' or '~'.
func pathLike(v string) bool {
	for _, seg := range strings.Split(v, ":") {
		if seg != "" && (seg[0] == '/' || seg[0] == '.' || seg[0] == '~') {
			return true
		}
	}
	return false
}

// CheckLaunchEnv validates a build's launch_env as it would be stored and returns one message per problem, keyed by variable name ("" for a
// problem with the object as a whole). An empty result means acceptable. It is pure: roots are passed in.
//
// Rules: at most MaxLaunchEnvEntries entries; names match ^[A-Z][A-Z0-9_]*$ and are at most MaxLaunchEnvNameLen long, do not contain KEY, TOKEN,
// SECRET, PASSWORD or CREDENTIAL, do not start with FORGE_ or FOUNDRY_ and are not a code-injection loader variable (LD_PRELOAD, LD_AUDIT);
// values are non-empty strings of at most MaxLaunchEnvValueLen bytes (MaxLaunchEnvTotalBytes for the whole object) with no NUL, no newline or
// carriage return and no other control character, and do not begin with a well-known credential prefix.
//
// A value that looks like a path (see pathLike) must have every ':'-separated segment non-empty (an empty LD_LIBRARY_PATH segment means the
// current directory), absolute, free of '..' and clean, and under pathRoots. pathRoots is catalog.build_roots plus catalog.launch_env_roots:
// build_roots is the trust boundary for what may be EXECUTED and drives slot-exec's second wall for the engine binary; the host libraries an
// engine loads (a ROCm tree, a kernel directory) are code too but live in trees that are not, and should not become, places a build binary may
// be registered from, so the operator names them in a second list that grants no build registration. The check is textual (the host may not hold the
// path when the API is called); slot-exec's own exe wall is separate.
func CheckLaunchEnv(env map[string]string, pathRoots []string) map[string]string {
	problems := map[string]string{}
	if len(env) > MaxLaunchEnvEntries {
		problems[""] = fmt.Sprintf("has %d entries; at most %d", len(env), MaxLaunchEnvEntries)
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	total := 0
	for _, k := range names {
		v := env[k]
		total += len(k) + len(v)
		switch {
		case len(k) > MaxLaunchEnvNameLen:
			problems[k] = fmt.Sprintf("name is longer than %d bytes", MaxLaunchEnvNameLen)
			continue
		case !launchEnvNameRe.MatchString(k):
			problems[k] = "name must match ^[A-Z][A-Z0-9_]*$"
			continue
		case hasAnyWord(k, secretNameWords):
			problems[k] = "name looks like a secret; secrets never go in a build row"
			continue
		case hasAnyPrefix(k, reservedNamePrefixes):
			problems[k] = "FORGE_ and FOUNDRY_ variables are stripped from the engine environment; they cannot be set here"
			continue
		case loaderInjectionNames[k]:
			problems[k] = "loader injection variables are not allowed"
			continue
		}
		if msg := checkLaunchEnvValue(v, pathRoots); msg != "" {
			problems[k] = msg
		}
	}
	if total > MaxLaunchEnvTotalBytes {
		problems[""] = fmt.Sprintf("is %d bytes in total; at most %d", total, MaxLaunchEnvTotalBytes)
	}
	return problems
}

func checkLaunchEnvValue(v string, pathRoots []string) string {
	if v == "" {
		return "value must not be empty"
	}
	if len(v) > MaxLaunchEnvValueLen {
		return fmt.Sprintf("value is longer than %d bytes", MaxLaunchEnvValueLen)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "value contains a control character (NUL, newline and carriage return included)"
		}
	}
	lower := strings.ToLower(strings.TrimSpace(v))
	for _, p := range secretValuePrefixes {
		if strings.HasPrefix(lower, p) {
			return "value looks like a credential; secrets never go in a build row"
		}
	}
	if !pathLike(v) {
		return ""
	}
	for _, seg := range strings.Split(v, ":") {
		switch {
		case seg == "":
			return "path list has an empty segment (it would mean the current directory)"
		case !filepath.IsAbs(seg):
			return fmt.Sprintf("path %q must be absolute", seg)
		case seg != filepath.Clean(seg):
			return fmt.Sprintf("path %q must be clean (no '..', '.', duplicate or trailing slashes)", seg)
		case !underRoot(seg, pathRoots):
			return fmt.Sprintf("path %q must be under an allowed root (%s; settings catalog.build_roots, %s)", seg, strings.Join(pathRoots, ", "), SettingLaunchEnvRoots)
		}
	}
	return ""
}

func hasAnyWord(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

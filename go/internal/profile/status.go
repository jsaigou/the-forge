// SPDX-License-Identifier: Apache-2.0

package profile

// What state a config's measurement is in, in one place. The old single "stale" flag was also set on configs that had never been measured and on
// engines that cannot be measured at all, so most of the list read "stale".
//
//	never       profilable, no measurement yet
//	current     measured, and nothing that shapes the numbers has changed since
//	outdated    measured, but the model file, context size, backend, engine binary or a launch flag that shapes the numbers changed since
//	unsupported this engine cannot be profiled
//
// Request-time defaults (sampling, chat template, reasoning budget) are not among the things that make a measurement outdated: they change what
// the model says, not how much memory it takes or how fast it runs.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
)

// Measurement states.
const (
	StateNever       = "never"
	StateCurrent     = "current"
	StateOutdated    = "outdated"
	StateUnsupported = "unsupported"
)

// Reasons a measurement is outdated.
const (
	OutdatedContext = "context"
	OutdatedBackend = "backend"
	OutdatedLaunch  = "launch" // model file, engine binary or launch flags
)

// requestFlags maps a launch flag that only sets a request-time default to whether it takes a value.
var requestFlags = map[string]bool{
	"--temp": true, "--top-k": true, "--top-p": true, "--min-p": true, "--seed": true,
	"--repeat-penalty": true, "--repeat-last-n": true, "--presence-penalty": true, "--frequency-penalty": true,
	"--dry-multiplier": true, "--dry-base": true, "--dry-allowed-length": true, "--dry-penalty-last-n": true,
	"--reasoning": true, "--reasoning-budget": true, "--reasoning-format": true,
	"--chat-template": true, "--chat-template-file": true, "--chat-template-kwargs": true,
	"--alias": true, "--jinja": false,
}

const fingerprintV2 = "v2:"

func hashParts(parts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// measurementFlags returns the launch flags that shape a measurement, each kept together with its value, in a stable order.
func measurementFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		flag, inline, hasInline := strings.Cut(args[i], "=")
		group := flag
		if hasInline {
			group += " " + inline
		}
		// Everything up to the next flag belongs to this one. A negative number ("-1") is a value, not a flag.
		for !hasInline && i+1 < len(args) && !isFlag(args[i+1]) {
			i++
			group += " " + args[i]
		}
		if _, requestOnly := requestFlags[flag]; requestOnly {
			continue
		}
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}

func isFlag(s string) bool {
	return len(s) > 1 && s[0] == '-' && (s[1] < '0' || s[1] > '9') && s[1] != '.'
}

// profileFingerprint is what a new profile records.
func (r *Runner) profileFingerprint(mode string) (string, error) {
	parts, args, err := r.fingerprintParts(mode)
	if err != nil {
		return "", err
	}
	return fingerprintV2 + hashParts(append(parts, strings.Join(measurementFlags(args), ","))), nil
}

// fingerprintMatches reports whether a stored fingerprint, of either form, still describes the config. Profiles recorded before v2 stay valid
// exactly as long as they did.
func (r *Runner) fingerprintMatches(mode, stored string) bool {
	var now string
	var err error
	if strings.HasPrefix(stored, fingerprintV2) {
		now, err = r.profileFingerprint(mode)
	} else {
		now, err = r.Fingerprint(mode)
	}
	if err != nil {
		r.logf("fingerprint of %q: %v", mode, err)
		return false
	}
	return now == stored
}

// Status is one config's measurement state.
type Status struct {
	Mode     string
	ConfigID int64
	State    string
	// Reason: for outdated, one of the Outdated constants; for unsupported, the engine's name.
	Reason string
	// Result is set when a measurement exists (current or outdated).
	Result *Result
	// NCtx is the configured context, WeightBytes the size of the weights file (0 when unreadable).
	NCtx        int
	WeightBytes int64
}

// Statuses lists every inference config with its measurement state, ordered by name.
func (r *Runner) Statuses() []Status {
	cfg := r.d.Cfg()
	out := []Status{}
	for name, mode := range cfg.Modes {
		if mode.Type == "service" || len(mode.Services) == 0 {
			continue
		}
		svc := mode.Services[0]
		st := Status{Mode: name, ConfigID: mode.ConfigID, State: StateNever, NCtx: svc.Context}
		if svc.Model != "" {
			if info, err := os.Stat(cfg.Paths.ResolveModelPath(svc.Model)); err == nil && !info.IsDir() {
				st.WeightBytes = info.Size()
			}
		}
		if res, ok := r.Get(name); ok {
			st.Result = &res
			st.State = StateCurrent
			if res.Stale {
				st.State = StateOutdated
				switch {
				case res.NCtx != svc.Context:
					st.Reason = OutdatedContext
				case res.Backend != svc.Backend:
					st.Reason = OutdatedBackend
				default:
					st.Reason = OutdatedLaunch
				}
			}
		}
		spec, err := svc.Engine()
		switch {
		case err != nil:
			st.State, st.Reason = StateUnsupported, err.Error()
		case !spec.Profilable:
			st.State, st.Reason = StateUnsupported, spec.Name
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mode < out[j].Mode })
	return out
}

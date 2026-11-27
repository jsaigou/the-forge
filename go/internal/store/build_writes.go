// SPDX-License-Identifier: Apache-2.0

package store

// Writes to a Build (create, update, retire, delete) with their rules in one place, the same shape as ConfigWrites. A Build is the compiled
// llama.cpp (or vLLM) a Config runs on. The rules: a name, a known backend and engine, a binary that passes the build guard (ADR-0020 F8), and
// the lifecycle: a Build in use by a Config cannot be retired, and only a retired Build can be deleted.
//
// Errors: *Invalid for the operator's mistakes, *BuildInUseError, *BuildNotRetiredError, ErrNotFound, and the store's own error when a read the
// rules depend on fails: a failed check is never a passed check (the in-use guard used to read "no Config uses it" when the Config list failed).

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsaigou/the-forge/internal/enginekind"
)

// RetiredBuildPrefix is the legacy "(retired) " name prefix; retirement is the Retired column now and the prefix is stripped from any incoming name.
const RetiredBuildPrefix = "(retired) "

// BuildInUseError refuses retiring or deleting a Build that Configs still run on.
type BuildInUseError struct {
	Build   string
	Configs []string
}

func (e *BuildInUseError) Error() string {
	return fmt.Sprintf("build %q is used by %d config(s): %s — repoint them first", e.Build, len(e.Configs), strings.Join(e.Configs, ", "))
}

// BuildNotRetiredError refuses deleting a live Build: retire it first, then delete.
type BuildNotRetiredError struct{ Build string }

func (e *BuildNotRetiredError) Error() string {
	return fmt.Sprintf("build %q is live: retire it first, then delete", e.Build)
}

// BuildWrites applies the rules and saves. It holds no state; build one per use.
type BuildWrites struct {
	Cat Catalog
	// Changed, if set, runs after every successful write (the cache invalidation hook).
	Changed func()
	// CheckBinary vets a Build's binary path against the build guard (internal/buildguard, over the configured roots) and returns what is wrong
	// with it, "" when it is acceptable. It is injected because the guard imports this package. A non-nil error means the roots could not be
	// read, which refuses the write rather than falling back to defaults.
	CheckBinary func(ctx context.Context, path string) (string, error)
	// CheckLaunchEnv vets a Build's launch_env (internal/buildguard.CheckLaunchEnv over the configured roots) and returns the problems keyed
	// by variable name ("" for the object as a whole); empty means acceptable. Injected for the same reason as CheckBinary. When it is nil a
	// non-empty launch_env is refused: an unchecked environment is never written.
	CheckLaunchEnv func(ctx context.Context, env map[string]string) (map[string]string, error)
}

func (w BuildWrites) changed() {
	if w.Changed != nil {
		w.Changed()
	}
}

// normalize is the row to write: no legacy prefix, a clean path, and no path at all for a retired Build (smith's path check skips a Build with
// no binary).
func normalizeBuild(id int64, b Build) Build {
	row := Build{ID: id, EngineID: b.EngineID, Name: strings.TrimPrefix(b.Name, RetiredBuildPrefix), Backend: b.Backend, Reason: b.Reason, Retired: b.Retired}
	if b.BinaryPath != "" && !b.Retired {
		row.BinaryPath = filepath.Clean(b.BinaryPath)
	}
	// A retired build keeps no environment either: it launches nothing, and its paths are no longer vetted.
	if len(b.LaunchEnv) > 0 && !b.Retired {
		row.LaunchEnv = make(map[string]string, len(b.LaunchEnv))
		for k, v := range b.LaunchEnv {
			row.LaunchEnv[k] = v
		}
	}
	return row
}

// Validate checks a Build as it would be written. currentID is the Build being edited (0 for a new one), so it does not collide with its own name.
func (w BuildWrites) Validate(ctx context.Context, b Build, currentID int64) (*Invalid, error) {
	fields := map[string]string{}
	codes := map[string]string{}
	name := strings.TrimPrefix(b.Name, RetiredBuildPrefix)
	if strings.TrimSpace(name) == "" {
		fields["name"], codes["name"] = "is required", "required"
	}
	switch b.Backend {
	case "vulkan", "rocm", "vllm", "":
	default:
		fields["backend"] = "must be one of: vulkan, rocm, vllm (or empty)"
	}
	binaryRequired := true // until the engine row says otherwise
	usesLaunchEnv := false // until the engine row says otherwise
	if b.EngineID == 0 {
		fields["engine_id"], codes["engine_id"] = "is required", "required"
	} else {
		engines, err := w.Cat.ListEngines(ctx)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range engines {
			if e.ID != b.EngineID {
				continue
			}
			found = true
			spec, err := enginekind.Resolve(e.Kind, b.Backend)
			if err != nil {
				// An engine row with a kind this binary does not know cannot be given builds: fail closed.
				fields["engine_id"] = err.Error()
				continue
			}
			binaryRequired = spec.BinaryRequired
			usesLaunchEnv = spec.UsesLaunchEnv
		}
		if !found {
			fields["engine_id"] = "does not exist"
		}
	}
	if _, bad := fields["name"]; !bad {
		all, err := w.Cat.ListBuilds(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range all {
			if o.ID != currentID && strings.TrimPrefix(o.Name, RetiredBuildPrefix) == name && !o.Retired {
				fields["name"], codes["name"] = "a build with this name already exists", "already_exists"
			}
		}
	}
	// A retired build carries no binary; every other build needs one, except an engine kind that is a service, not a file (vLLM).
	if !b.Retired && (b.BinaryPath != "" || binaryRequired) {
		if b.BinaryPath == "" {
			fields["binary_path"], codes["binary_path"] = "is required (only a vllm or retired build may omit it)", "required"
		} else if w.CheckBinary != nil {
			msg, err := w.CheckBinary(ctx, b.BinaryPath)
			if err != nil {
				return nil, err
			}
			if msg != "" {
				fields["binary_path"] = msg
			}
		}
	}
	if len(b.LaunchEnv) > 0 && !b.Retired {
		if err := w.validateLaunchEnv(ctx, b.LaunchEnv, usesLaunchEnv, fields); err != nil {
			return nil, err
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &Invalid{Fields: fields, Codes: codes}, nil
}

// validateLaunchEnv adds a "launch_env" field error when the environment is not acceptable for this engine kind. An engine kind whose builder
// ignores the environment refuses one (a value silently dropped at launch is a trap); a missing checker refuses it too.
func (w BuildWrites) validateLaunchEnv(ctx context.Context, env map[string]string, usesLaunchEnv bool, fields map[string]string) error {
	if !usesLaunchEnv {
		fields["launch_env"] = "is not used by this engine kind; remove it"
		return nil
	}
	if w.CheckLaunchEnv == nil {
		fields["launch_env"] = "cannot be validated on this server; refusing to store it"
		return nil
	}
	problems, err := w.CheckLaunchEnv(ctx, env)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	names := make([]string, 0, len(problems))
	for k := range problems {
		names = append(names, k)
	}
	sort.Strings(names)
	msgs := make([]string, 0, len(names))
	for _, k := range names {
		if k == "" {
			msgs = append(msgs, problems[k])
		} else {
			msgs = append(msgs, k+": "+problems[k])
		}
	}
	fields["launch_env"] = strings.Join(msgs, "; ")
	return nil
}

// configsUsing names the Configs that run on build id. A failed Config list is an error, not an empty answer.
func (w BuildWrites) configsUsing(ctx context.Context, id int64) ([]string, error) {
	cfgs, err := w.Cat.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range cfgs {
		if c.BuildID == id {
			names = append(names, c.Name)
		}
	}
	return names, nil
}

func (w BuildWrites) guardInUse(ctx context.Context, b Build) error {
	users, err := w.configsUsing(ctx, b.ID)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return &BuildInUseError{Build: b.Name, Configs: users}
	}
	return nil
}

// Create validates and saves a new Build and returns it as stored.
func (w BuildWrites) Create(ctx context.Context, b Build) (Build, error) {
	if inv, err := w.Validate(ctx, b, 0); err != nil {
		return Build{}, err
	} else if inv != nil {
		return Build{}, inv
	}
	id, err := w.Cat.CreateBuild(ctx, normalizeBuild(0, b))
	if err != nil {
		return Build{}, err
	}
	w.changed()
	return w.Cat.GetBuild(ctx, id)
}

// Update validates and replaces the Build with the given id (a full replace) and returns the row before and after. Retiring a live Build is refused
// while a Config still runs on it: it would strand that Config on a binary no new Config may use and that smith's path check stops watching.
func (w BuildWrites) Update(ctx context.Context, id int64, b Build) (before, after Build, err error) {
	before, err = w.Cat.GetBuild(ctx, id)
	if err != nil {
		return Build{}, Build{}, err
	}
	if inv, verr := w.Validate(ctx, b, id); verr != nil {
		return Build{}, Build{}, verr
	} else if inv != nil {
		return Build{}, Build{}, inv
	}
	if b.Retired && !before.Retired {
		if err := w.guardInUse(ctx, before); err != nil {
			return Build{}, Build{}, err
		}
	}
	if err := w.Cat.UpdateBuild(ctx, normalizeBuild(id, b)); err != nil {
		return Build{}, Build{}, err
	}
	w.changed()
	after, err = w.Cat.GetBuild(ctx, id)
	return before, after, err
}

// Retire marks a Build retired and clears its binary path, changing nothing else (the reason gets suffix appended). It re-reads the row, so a
// caller holding an old copy cannot write it back, and it refuses while a Config runs on the Build. Retiring a retired Build is a no-op.
func (w BuildWrites) Retire(ctx context.Context, id int64, reasonSuffix string) (Build, error) {
	b, err := w.Cat.GetBuild(ctx, id)
	if err != nil {
		return Build{}, err
	}
	if b.Retired {
		return b, nil
	}
	if err := w.guardInUse(ctx, b); err != nil {
		return Build{}, err
	}
	b.Retired, b.BinaryPath, b.LaunchEnv = true, "", nil
	b.Reason += reasonSuffix
	if err := w.Cat.UpdateBuild(ctx, b); err != nil {
		return Build{}, err
	}
	w.changed()
	return b, nil
}

// Delete removes a retired Build that no Config references and returns what it removed. A live Build is never removed in one step.
func (w BuildWrites) Delete(ctx context.Context, id int64) (Build, error) {
	b, err := w.Cat.GetBuild(ctx, id)
	if err != nil {
		return Build{}, err
	}
	if !b.Retired {
		return b, &BuildNotRetiredError{Build: b.Name}
	}
	if err := w.guardInUse(ctx, b); err != nil {
		return b, err
	}
	if err := w.Cat.DeleteBuild(ctx, id); err != nil {
		return Build{}, err
	}
	w.changed()
	return b, nil
}

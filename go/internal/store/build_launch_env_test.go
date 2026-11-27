// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// radianceBuildWrites is newBuildWrites with a radiance-kind engine (id 2) and a launch_env checker that refuses anything containing "BAD".
func radianceBuildWrites() (store.BuildWrites, *fakeBuildCat) {
	w, cat, _ := newBuildWrites()
	cat.engines = []store.Engine{{ID: 1}, {ID: 2, Name: "Radiance", Kind: "radiance"}}
	w.CheckLaunchEnv = func(_ context.Context, env map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range env {
			if strings.Contains(v, "BAD") {
				out[k] = "bad value"
			}
		}
		return out, nil
	}
	return w, cat
}

func radBuild(name string, env map[string]string) store.Build {
	return store.Build{EngineID: 2, Name: name, BinaryPath: "/opt/radiance/bin/radiance", Backend: "rocm", LaunchEnv: env}
}

func TestBuildWrites_LaunchEnv(t *testing.T) {
	ctx := context.Background()
	w, cat := radianceBuildWrites()
	env := map[string]string{"RADIANCE_HOME": "/opt/radiance/home", "RADIANCE_KERNELS": "libhalo"}
	got, err := w.Create(ctx, radBuild("rad-1", env))
	if err != nil || got.LaunchEnv["RADIANCE_KERNELS"] != "libhalo" {
		t.Fatalf("create: %+v %v", got, err)
	}
	env["RADIANCE_HOME"] = "changed" // the stored map must not alias the caller's
	if cat.builds[got.ID].LaunchEnv["RADIANCE_HOME"] != "/opt/radiance/home" {
		t.Error("stored launch_env aliases the request map")
	}

	invalid := func(name string, w store.BuildWrites, b store.Build, want string) {
		t.Helper()
		var inv *store.Invalid
		_, err := w.Create(ctx, b)
		if !errors.As(err, &inv) || !strings.Contains(inv.Fields["launch_env"], want) {
			t.Errorf("%s: err = %v, want launch_env containing %q", name, err, want)
		}
	}
	invalid("checker problem", w, radBuild("rad-2", map[string]string{"A": "BAD"}), "A: bad value")
	invalid("llama.cpp ignores it", w, store.Build{EngineID: 1, Name: "l", BinaryPath: "/opt/l/llama-server", Backend: "rocm", LaunchEnv: map[string]string{"A": "b"}}, "not used by this engine kind")
	noChecker := w
	noChecker.CheckLaunchEnv = nil
	invalid("no checker fails closed", noChecker, radBuild("rad-3", map[string]string{"A": "b"}), "cannot be validated")

	// No launch_env needs no checker.
	if _, err := noChecker.Create(ctx, radBuild("rad-4", nil)); err != nil {
		t.Errorf("a build with no launch_env needs no checker: %v", err)
	}
	// A failing checker is an error, not a verdict.
	failing := w
	failing.CheckLaunchEnv = func(context.Context, map[string]string) (map[string]string, error) { return nil, errCat }
	if _, err := failing.Create(ctx, radBuild("rad-5", map[string]string{"A": "b"})); !errors.Is(err, errCat) {
		t.Errorf("checker failure = %v, want the store error", err)
	}

	// Update is a full replace; retiring drops the environment with the binary.
	_, after, err := w.Update(ctx, got.ID, radBuild("rad-1", map[string]string{"B": "c"}))
	if err != nil || len(after.LaunchEnv) != 1 || after.LaunchEnv["B"] != "c" {
		t.Fatalf("update: %+v %v", after, err)
	}
	r, err := w.Retire(ctx, got.ID, " (retired)")
	if err != nil || r.LaunchEnv != nil || cat.builds[got.ID].LaunchEnv != nil {
		t.Errorf("retire kept the environment: %+v %v", r, err)
	}
}

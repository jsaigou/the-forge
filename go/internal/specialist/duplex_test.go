// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// ADR-0029: a duplex session resolves its engine by name, strictly, and keeps a lease with an id another process can renew and release.

func (e *env) duplexEngine(t *testing.T, name string) {
	t.Helper()
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: name, Kind: "qwen3-asr-server", Policy: store.PolicyOnDemand, Enabled: true,
		Functions: []string{store.FnDuplex}, MemoryBytes: 1 << 30, Params: map[string]string{"model": "/opt/forge/models/" + name + ".gguf"}}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDuplex_LoadsTheNamedEngineAndLeasesIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.health(8100, true)
	e.duplexEngine(t, "haru")
	tg, err := e.m.ResolveDuplex(ctx, "haru")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Occupant != "haru" || tg.BaseURL != "http://127.0.0.1:8100" || tg.LeaseID == "" || tg.Expires.IsZero() {
		t.Fatalf("target %+v: want haru on 8100 with a lease id and an expiry", tg)
	}
	if !e.m.RenewLease(tg.LeaseID) {
		t.Fatal("a live lease must renew by its id (another process holds only the id)")
	}
	if !e.m.ReleaseSpeech(tg.LeaseID) {
		t.Fatal("release by id")
	}
	if e.m.RenewLease(tg.LeaseID) {
		t.Fatal("a released lease must not renew: the caller has to learn its engine is unprotected")
	}
}

func TestResolveDuplex_IsStrictAboutTheName(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.health(8100, true)
	e.duplexEngine(t, "haru")
	e.add(t, "asr", store.PolicyOnDemand, "", 0) // serves transcription only
	if err := e.sp.SetDefault(ctx, store.FnDuplex, "haru"); err != nil {
		t.Fatal(err)
	}
	// Resolve would fall back to the default for an unknown model string; a speech-to-speech session must not quietly talk to another model.
	if _, err := e.m.ResolveDuplex(ctx, "hary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a mistyped name: %v, want ErrNotFound (not the default)", err)
	}
	var ue *UnavailableError
	if _, err := e.m.ResolveDuplex(ctx, "asr"); !errors.As(err, &ue) {
		t.Fatalf("a member that does not serve duplex: %v, want *UnavailableError", err)
	}
	// An empty name is the function's default.
	tg, err := e.m.ResolveDuplex(ctx, "")
	if err != nil || tg.Occupant != "haru" {
		t.Fatalf("empty name -> %+v, %v, want the default haru", tg, err)
	}
}

func TestResolveDuplex_NoNameAndNoDefaultIsNoMatch(t *testing.T) {
	e := newEnv(t)
	e.duplexEngine(t, "haru")
	if _, err := e.m.ResolveDuplex(context.Background(), ""); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("no name and no default: %v, want ErrNoMatch", err)
	}
}

func TestResolveDuplex_ADisabledEngineIsHardUnavailable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.sp.CreateOccupant(ctx, store.Occupant{Name: "haru", Kind: "qwen3-asr-server", Policy: store.PolicyOnDemand, Enabled: false,
		Functions: []string{store.FnDuplex}, Params: map[string]string{"model": "/opt/forge/models/haru.gguf"}}); err != nil {
		t.Fatal(err)
	}
	var ue *UnavailableError
	if _, err := e.m.ResolveDuplex(ctx, "haru"); !errors.As(err, &ue) || ue.Reason != ReasonDisabled {
		t.Fatalf("a disabled engine: %v, want UnavailableError reason %q", err, ReasonDisabled)
	}
}

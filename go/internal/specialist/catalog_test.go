// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeCatalog resolves catalog configs the way the daemon does from the merged catalog.
type fakeCatalog struct {
	mu      sync.Mutex
	specs   map[string]LaunchSpec
	est     map[string]int64
	lastCtx int
	rowErr  error // LaunchRow fails with this when set
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{specs: map[string]LaunchSpec{}, est: map[string]int64{}}
}

func (c *fakeCatalog) LaunchSpec(_ context.Context, name string) (LaunchSpec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sp, ok := c.specs[name]
	if !ok {
		return LaunchSpec{}, errors.New("no such catalog config")
	}
	c.lastCtx = sp.Ctx
	return sp, nil
}

// LaunchRow is a fake row builder: a row for the slot and port with the config's name, as the real builder produces for an engine launched from a row.
func (c *fakeCatalog) LaunchRow(_ context.Context, name, slot string, port int) (store.SlotLaunch, error) {
	if c.rowErr != nil {
		return store.SlotLaunch{}, c.rowErr
	}
	return store.SlotLaunch{Slot: slot, ConfigName: name, EngineKind: "radiance", Exe: "/opt/radiance/strix-halo-haru/build/bin/radiance",
		Argv: []string{"--model", "/opt/forge/models/Haru-4B/haru-bf16.rad", "--port", fmt.Sprint(port)}, Port: port}, nil
}

func (c *fakeCatalog) EstimateBytes(_ context.Context, name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.est[name]
}

func d1Spec() LaunchSpec {
	return LaunchSpec{
		Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1-2B/d1-2B-Q8_0.gguf",
		MMProj: "/opt/forge/models/d1-2B/mmproj-F16.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000,
		Args: []string{"--load-mode", "none", "--jinja", "--flash-attn", "on", "--threads", "16", "--parallel", "1", "--batch-size", "16384", "--ubatch-size", "16384"},
	}
}

func (e *env) addCatalog(t *testing.T, name, config, policy, home string) {
	t.Helper()
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: name, Kind: KindCatalogConfig, Policy: policy, HomeSlot: home, Enabled: true,
		Functions: []string{store.FnDecisions}, Params: map[string]string{"config": config}}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogConfig_ResolvedRenderedAndVerified(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cat.specs["d1-2b"] = d1Spec()
	e.cat.est["d1-2b"] = 7 << 30
	e.addCatalog(t, "d1-2b", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)

	st, err := e.m.Ensure(ctx, "d1-2b")
	if err != nil || st.State != StateRunning {
		t.Fatalf("Ensure: %+v, %v", st, err)
	}
	envText, _ := os.ReadFile(filepath.Join(e.dir, "s1.env"))
	for _, want := range []string{"FORGE_POOL_KIND=catalog-llama", "FORGE_POOL_OCCUPANT=d1-2b", "FORGE_POOL_PORT=8100", "FORGE_POOL_P_ALIAS=d1-2b",
		"FORGE_POOL_P_CTX=128000", "FORGE_POOL_P_BIN=/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server",
		"FORGE_POOL_P_MMPROJ=/opt/forge/models/d1-2B/mmproj-F16.gguf"} {
		if !strings.Contains(string(envText), want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, envText)
		}
	}
	argsText, _ := os.ReadFile(filepath.Join(e.dir, "s1.args"))
	if string(argsText) != "--load-mode\nnone\n--jinja\n--flash-attn\non\n--threads\n16\n--parallel\n1\n--batch-size\n16384\n--ubatch-size\n16384\n" {
		t.Errorf("args file = %q (one argument per line, as the LLM slots' args files)", argsText)
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, "s1.args")); fi.Mode().Perm() != 0o600 {
		t.Errorf("args file mode %v", fi.Mode().Perm())
	}

	// A later occupant of the same slot must not inherit the catalog args.
	if err := e.m.Unload(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	e.add(t, "asr", store.PolicyOnDemand, "", 0)
	if _, err := e.m.Ensure(ctx, "asr"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "s1.args")); !os.IsNotExist(err) {
		t.Errorf("a stale args file survived into the next occupant of the slot: %v", err)
	}
}

func TestCatalogConfig_ContextSilentlyReducedFailsTheStart(t *testing.T) {
	e := newEnv(t)
	e.cat.specs["d1-2b"] = d1Spec()
	e.addCatalog(t, "d1-2b", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)
	e.nctx[8100] = 60000 // the kernel could not give a contiguous allocation: llama.cpp lowered the context
	var ue *UnavailableError
	_, err := e.m.Ensure(context.Background(), "d1-2b")
	if !errors.As(err, &ue) || !strings.Contains(ue.Reason, "n_ctx was silently reduced to 60000") {
		t.Fatalf("got %v", err)
	}
	if e.sys.active["forge-pool@s1"] {
		t.Error("a member with a reduced context was left running")
	}
	// Within 5% is fine (llama.cpp rounds the context).
	e.nctx[8100] = 125000
	if _, err := e.m.Ensure(context.Background(), "d1-2b"); err != nil {
		t.Errorf("a context within 5%% of the request was refused: %v", err)
	}
}

func TestCatalogConfig_RefusesWhatTheLauncherWouldAndExplainsWhy(t *testing.T) {
	roots := []string{"/opt/forge/models"}
	bad := map[string]func(*LaunchSpec){
		"llama-server outside the root": func(s *LaunchSpec) { s.Bin = "/usr/bin/llama-server" },
		"not llama-server":              func(s *LaunchSpec) { s.Bin = "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/bash" },
		"vllm backend":                  func(s *LaunchSpec) { s.Backend = "vllm" },
		"model outside the roots":       func(s *LaunchSpec) { s.Model = "/etc/passwd" },
		"zero context":                  func(s *LaunchSpec) { s.Ctx = 0 },
		"args set the port":             func(s *LaunchSpec) { s.Args = append(s.Args, "--port", "9999") },
		"args set the host":             func(s *LaunchSpec) { s.Args = append(s.Args, "--host=0.0.0.0") },
		"args set an api key":           func(s *LaunchSpec) { s.Args = append(s.Args, "--api-key", "x") },
		"args carry mmproj":             func(s *LaunchSpec) { s.Args = append(s.Args, "--mmproj", "/opt/forge/models/x.gguf") },
		"an argument with a newline":    func(s *LaunchSpec) { s.Args = append(s.Args, "a\nb") },
	}
	for name, mutate := range bad {
		spec := d1Spec()
		mutate(&spec)
		if err := ValidateLaunchSpec(spec, roots); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateLaunchSpec(d1Spec(), roots); err != nil {
		t.Errorf("the d1-2b spec was refused: %v", err)
	}
	// JSON-valued args (the 5 configs that use --chat-template-kwargs) are fine: they travel in a file, not an environment line.
	spec := d1Spec()
	spec.Args = append(spec.Args, "--chat-template-kwargs", `{"enable_thinking": false}`)
	if err := ValidateLaunchSpec(spec, roots); err != nil {
		t.Errorf("a JSON-valued arg was refused: %v", err)
	}
}

func TestCatalogConfig_UnknownConfigAndNoCatalogGiveSpecificReasons(t *testing.T) {
	e := newEnv(t)
	e.addCatalog(t, "ghost", "no-such-config", store.PolicyOnDemand, "")
	e.health(8100, true)
	var ue *UnavailableError
	if _, err := e.m.Ensure(context.Background(), "ghost"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, `resolve catalog config "no-such-config"`) {
		t.Errorf("unknown config: %v", err)
	}
}

// One owner: the pool knows which catalog configs it hosts, and resolves a request for one to its member (loading it on demand).
func TestCatalogConfig_PooledConfigAndResolveConfig(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cat.specs["d1-2b"] = d1Spec()
	e.addCatalog(t, "d1-member", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)
	if !e.m.PooledConfig(ctx, "d1-2b") || e.m.PooledConfig(ctx, "gemma4-26b") {
		t.Fatal("PooledConfig must be true for a hosted config only")
	}
	if _, err := e.m.ResolveConfig(ctx, "gemma4-26b"); !errors.Is(err, ErrNoMatch) {
		t.Errorf("an unpooled config must be ErrNoMatch (the scheduler keeps it): %v", err)
	}
	tg, err := e.m.ResolveConfig(ctx, "d1-2b")
	if err != nil || tg.Port != 8100 || tg.Slot != "s1" || tg.Occupant != "d1-member" {
		t.Fatalf("ResolveConfig = %+v, %v", tg, err)
	}
	// A disabled member no longer owns the config.
	o, _ := e.sp.OccupantByName(ctx, "d1-member")
	o.Enabled = false
	_ = e.sp.UpdateOccupant(ctx, o)
	if e.m.PooledConfig(ctx, "d1-2b") {
		t.Error("a disabled member still owns its config")
	}
}

// A member serving a long request is never evicted for being idle; once it is quiet it is.
func TestCatalogConfig_BusyMemberIsNotEvicted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cat.specs["d1-2b"] = d1Spec()
	e.addCatalog(t, "llm", "d1-2b", store.PolicyOnDemand, "")
	e.add(t, "a", store.PolicyOnDemand, "", 0)
	e.add(t, "b", store.PolicyOnDemand, "", 0)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	// Fill all three slots with on-demand members: llm (s1), a (s2), b (s3); then ask for a fourth.
	for _, n := range []string{"llm", "a", "b"} {
		if _, err := e.m.Ensure(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	e.add(t, "c", store.PolicyOnDemand, "", 0)
	e.advance(10 * time.Minute) // everyone is long idle; llm is the least recently used
	e.m.Touch("s2")
	e.m.Touch("s3")
	e.advance(190 * time.Second)

	e.mu.Lock()
	e.busy[8100] = true // llm has been generating the whole time
	e.mu.Unlock()
	got, err := e.m.Ensure(ctx, "c")
	if err != nil || got.Slot != "s2" {
		t.Fatalf("c -> %+v, %v: want it to take s2 (a), because llm in s1 is busy even though it is the least recently used", got, err)
	}
	if st := state(t, e, "s1"); st.Occupant != "llm" || st.State != StateRunning {
		t.Errorf("the busy member was disturbed: %+v", st)
	}
}

func TestCatalogConfig_FitGateUsesTheCatalogEstimateWhenUnregistered(t *testing.T) {
	e := newEnv(t)
	e.cat.specs["d1-2b"] = d1Spec()
	e.cat.est["d1-2b"] = 9 << 30
	e.addCatalog(t, "d1-2b", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)
	e.free = 4 << 30
	var ue *UnavailableError
	if _, err := e.m.Ensure(context.Background(), "d1-2b"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "not enough GPU memory") {
		t.Fatalf("got %v", err)
	}
	if len(e.sys.starts) != 0 {
		t.Error("a load that does not fit was started")
	}
}

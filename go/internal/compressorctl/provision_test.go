// SPDX-License-Identifier: Apache-2.0

package compressorctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSystemd is a scriptable Systemd for tests — records every call.
type fakeSystemd struct {
	started, stopped, restarted   []string
	startErr, stopErr, restartErr error
}

func (f *fakeSystemd) Start(_ context.Context, unit string) error {
	f.started = append(f.started, unit)
	return f.startErr
}

func (f *fakeSystemd) Stop(_ context.Context, unit string) error {
	f.stopped = append(f.stopped, unit)
	return f.stopErr
}

func (f *fakeSystemd) Restart(_ context.Context, unit string) error {
	f.restarted = append(f.restarted, unit)
	return f.restartErr
}

func TestUnitName(t *testing.T) {
	p := &Provisioner{}
	if got := p.UnitName("deepseek"); got != "forge-compress@deepseek" {
		t.Errorf("UnitName(deepseek) = %q, want forge-compress@deepseek", got)
	}
}

func TestUnitName_CustomTemplatePrefix(t *testing.T) {
	// A Provisioner can still be pointed at a different template prefix —
	// no real second template exists any more (Sprint 7 dropped the
	// headroom-ai-shaped one once Sprint 6 confirmed zero live rows used
	// it), but the mechanism itself stays generic.
	p := &Provisioner{TemplatePrefix: "other-template@"}
	if got := p.UnitName("local"); got != "other-template@local" {
		t.Errorf("UnitName(local) = %q, want other-template@local", got)
	}
}

func TestIsTemplateUnit(t *testing.T) {
	cases := []struct {
		unit string
		want bool
	}{
		{"forge-compress@deepseek", true},
		{"forge-compress@external", true},
		{"headroom-external", false}, // aiand's real legacy unit, pre-Sprint-3
		{"headroom-a1", false},       // legacy local-fronting unit, pre-Sprint-3
		{"", false},
	}
	p := &Provisioner{}
	for _, c := range cases {
		if got := p.isTemplateUnit(c.unit); got != c.want {
			t.Errorf("isTemplateUnit(%q) = %v, want %v", c.unit, got, c.want)
		}
	}
}

func TestIsTemplateUnit_DoesNotCrossTemplates(t *testing.T) {
	// A row from one template prefix must NOT be claimed by a Provisioner
	// driving a different one — during the Sprint 3 migration window, rows
	// from the old headroom-ai-shaped template and the new
	// forge-compress@ one briefly coexisted in the same store, and each
	// Provisioner must only ever touch its own.
	other := &Provisioner{TemplatePrefix: "other-template@"}
	if other.isTemplateUnit("forge-compress@deepseek") {
		t.Error("an other-template@ Provisioner claimed a forge-compress@ unit")
	}
	compress := &Provisioner{}
	if compress.isTemplateUnit("other-template@local") {
		t.Error("a forge-compress@ Provisioner claimed an other-template@ unit")
	}
}

func newTemplateRow(service string) store.ProxyRow {
	p := &Provisioner{}
	return store.ProxyRow{
		Service:   service,
		Unit:      p.UnitName(service),
		Port:      8792,
		TargetURL: "https://api.deepseek.com/v1",
		Token:     "sekret",
	}
}

func TestProvisionWritesEnvAndStarts(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}

	row := newTemplateRow("deepseek")
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if len(sys.restarted) != 1 || sys.restarted[0] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want [forge-compress@deepseek]", sys.restarted)
	}

	content, err := os.ReadFile(filepath.Join(dir, "deepseek.env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	got := string(content)
	for _, want := range []string{
		"OPENAI_TARGET_API_URL=https://api.deepseek.com/v1",
		"COMPRESS_PROXY_TOKEN=sekret",
		"COMPRESS_PORT=8792",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file missing %q, got:\n%s", want, got)
		}
	}

	info, err := os.Stat(filepath.Join(dir, "deepseek.env"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file mode = %o, want 0600", perm)
	}
}

func TestProvisionNeverUsesStart(t *testing.T) {
	// Regression for the live 2026-08-19 bug: Provision must always go
	// through Restart, never Start, because Start is a silent no-op against
	// a unit name that happens to already be active (e.g. a stray
	// out-of-band process squatting on the instance name before the store
	// ever provisioned it) — the freshly written env file would never
	// actually take effect.
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}

	row := newTemplateRow("deepseek")
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(sys.started) != 0 {
		t.Errorf("started = %v, want none — Provision must use Restart so an already-active unit still picks up the new env", sys.started)
	}
}

func TestProvisionRequiresToken(t *testing.T) {
	p := &Provisioner{Systemd: &fakeSystemd{}, EnvDir: t.TempDir()}
	row := newTemplateRow("deepseek")
	row.Token = ""
	if err := p.Provision(context.Background(), row); err == nil {
		t.Fatal("expected error for missing token, got nil")
	}
}

func TestProvisionRefusesNonTemplateUnit(t *testing.T) {
	// A row whose Unit isn't a forge-compress@ instance (e.g. constructed
	// with a bug, or accidentally pointed at a legacy name) must be
	// refused, not silently started under the wrong assumption.
	p := &Provisioner{Systemd: &fakeSystemd{}, EnvDir: t.TempDir()}
	row := newTemplateRow("external")
	row.Unit = "headroom-external" // aiand's real legacy unit name, pre-Sprint-3
	if err := p.Provision(context.Background(), row); err == nil {
		t.Fatal("expected error provisioning onto a legacy unit name, got nil")
	}
}

func TestReconcileRewritesEnvAndRestarts(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}

	row := newTemplateRow("deepseek")
	row.TargetURL = "https://old.example.com/v1"
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	row.TargetURL = "https://new.example.com/v1"
	if err := p.Reconcile(context.Background(), row); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sys.restarted) != 2 || sys.restarted[1] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want two [forge-compress@deepseek] (Provision, then Reconcile)", sys.restarted)
	}
	content, _ := os.ReadFile(filepath.Join(dir, "deepseek.env"))
	if !strings.Contains(string(content), "OPENAI_TARGET_API_URL=https://new.example.com/v1") {
		t.Errorf("env not rewritten: %s", content)
	}
}

func TestReconcileRefusesLegacyUnit(t *testing.T) {
	// aiand's real legacy proxy ("headroom-external", pre-Sprint-3) baked
	// its target into its own unit file's Environment= lines — rewriting
	// our env file wouldn't touch that, so silently "succeeding" here would
	// restart the process while leaving its real target unchanged. Must
	// refuse instead.
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}
	row := store.ProxyRow{
		Service: "external", Unit: "headroom-external",
		Port: 8791, TargetURL: "https://api.aiand.com/v1", Token: "sekret",
	}
	if err := p.Reconcile(context.Background(), row); err == nil {
		t.Fatal("expected Reconcile to refuse a legacy hand-created unit")
	}
	if len(sys.restarted) != 0 {
		t.Errorf("restarted = %v, want no restart attempted", sys.restarted)
	}
	if _, err := os.Stat(filepath.Join(dir, "external.env")); !os.IsNotExist(err) {
		t.Errorf("env file should not have been written for a legacy unit: err=%v", err)
	}
}

func TestRestartWorksOnAnyUnitShape(t *testing.T) {
	// Restart (unlike Reconcile) never touches an env file, so it's safe
	// for a legacy hand-created unit too — a real request against aiand's
	// old "headroom-external" must restart that exact unit, never a
	// reconstructed template name.
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: t.TempDir()}
	if err := p.Restart(context.Background(), "headroom-external"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "headroom-external" {
		t.Errorf("restarted = %v, want [headroom-external]", sys.restarted)
	}
}

func TestTeardownStopsAndRemovesEnv(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}

	row := newTemplateRow("deepseek")
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := p.Teardown(context.Background(), "forge-compress@deepseek"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "forge-compress@deepseek" {
		t.Errorf("stopped = %v, want [forge-compress@deepseek]", sys.stopped)
	}
	if _, err := os.Stat(filepath.Join(dir, "deepseek.env")); !os.IsNotExist(err) {
		t.Errorf("env file still exists after teardown: err=%v", err)
	}
}

func TestTeardownLegacyUnitStopsWithoutTouchingEnvDir(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}
	if err := p.Teardown(context.Background(), "headroom-external"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "headroom-external" {
		t.Errorf("stopped = %v, want [headroom-external]", sys.stopped)
	}
}

func TestTeardownStopErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir}
	row := newTemplateRow("deepseek")
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	sys.stopErr = context.DeadlineExceeded
	if err := p.Teardown(context.Background(), "forge-compress@deepseek"); err == nil {
		t.Fatal("expected Stop error to propagate")
	}
	// Env file must survive a failed stop — the caller shouldn't lose the
	// instance's config if teardown didn't actually succeed.
	if _, err := os.Stat(filepath.Join(dir, "deepseek.env")); err != nil {
		t.Errorf("env file should still exist after a failed stop: %v", err)
	}
}

func TestProvisionReconcileTeardown_CustomTemplatePrefix(t *testing.T) {
	// End-to-end lifecycle for a Provisioner pointed at a non-default
	// template prefix — the same code path the default forge-compress@
	// Provisioner uses, just under a different template name.
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir, TemplatePrefix: "other-template@"}

	row := store.ProxyRow{
		Service:   "local",
		Unit:      p.UnitName("local"),
		Port:      8788,
		TargetURL: "",
		Token:     "sekret",
	}
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "other-template@local" {
		t.Errorf("restarted (via Provision) = %v, want [other-template@local]", sys.restarted)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.env")); err != nil {
		t.Fatalf("env file not written: %v", err)
	}

	row.TargetURL = "https://api.deepseek.com/v1"
	if err := p.Reconcile(context.Background(), row); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(sys.restarted) != 2 || sys.restarted[1] != "other-template@local" {
		t.Errorf("restarted = %v, want two [other-template@local] (Provision, then Reconcile)", sys.restarted)
	}

	if err := p.Teardown(context.Background(), row.Unit); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "other-template@local" {
		t.Errorf("stopped = %v, want [other-template@local]", sys.stopped)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.env")); !os.IsNotExist(err) {
		t.Errorf("env file should be removed after teardown: err=%v", err)
	}
}

func TestReconcileRefusesUnitFromADifferentTemplate(t *testing.T) {
	// A Provisioner driving one template prefix must refuse to reconcile a
	// row from a different one — same correctness trap as refusing a
	// legacy hand-created unit: rewriting an env file the other template's
	// real process doesn't read would restart it while silently leaving
	// its actual target unchanged.
	dir := t.TempDir()
	sys := &fakeSystemd{}
	p := &Provisioner{Systemd: sys, EnvDir: dir, TemplatePrefix: "other-template@"}
	row := newTemplateRow("deepseek") // Unit: "forge-compress@deepseek"
	if err := p.Reconcile(context.Background(), row); err == nil {
		t.Fatal("expected Reconcile to refuse a different template's unit")
	}
	if len(sys.restarted) != 0 {
		t.Errorf("restarted = %v, want none", sys.restarted)
	}
}

func TestGenerateTokenUnique(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatal("empty token")
	}
	if a == b {
		t.Error("two calls returned the same token")
	}
}

func TestAllocatePort(t *testing.T) {
	existing := []store.ProxyRow{
		{Service: "local", Port: 8788},
		{Service: "deepseek", Port: DefaultBasePort},
	}
	got := AllocatePort(existing, DefaultBasePort)
	if got != DefaultBasePort+1 {
		t.Errorf("AllocatePort = %d, want %d", got, DefaultBasePort+1)
	}
}

func TestAllocatePortNoConflicts(t *testing.T) {
	got := AllocatePort(nil, DefaultBasePort)
	if got != DefaultBasePort {
		t.Errorf("AllocatePort(nil) = %d, want %d", got, DefaultBasePort)
	}
}

func TestDeriveServiceName(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		taken map[string]bool
		want  string
	}{
		{"plain", "DeepSeek", nil, "deepseek"},
		{"ampersand", "AI&", nil, "aiand"},
		{"spaces and caps", "Qwen Cloud", nil, "qwencloud"},
		{"strip leading non-letter", "1st Provider", nil, "stprovider"},
		{"dedupe", "deepseek", map[string]bool{"deepseek": true}, "deepseek-2"},
		{"dedupe twice", "deepseek", map[string]bool{"deepseek": true, "deepseek-2": true}, "deepseek-3"},
		{"empty", "", nil, ""},
		{"symbols only", "!!!", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveServiceName(tc.in, func(s string) bool { return tc.taken[s] })
			if got != tc.want {
				t.Errorf("DeriveServiceName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got != "" && !serviceNameRE.MatchString(got) {
				t.Errorf("DeriveServiceName(%q) = %q, not a valid service name", tc.in, got)
			}
		})
	}
}

// SPDX-License-Identifier: Apache-2.0

package compressorctl

import (
	"context"
	"os"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// T6: while retired (the production default), Provision and Reconcile are
// no-ops — no env file, no systemd call — but Teardown still works so
// existing units can be cleaned up.
func TestRetiredProvisionReconcileAreNoOps(t *testing.T) {
	if !Retired() {
		t.Fatal("must be retired by default")
	}
	sys := &fakeSystemd{}
	dir := t.TempDir()
	p := &Provisioner{Systemd: sys, EnvDir: dir}
	row := store.ProxyRow{Service: "deepseek", Unit: "forge-compress@deepseek", Token: "t", Port: 8792, TargetURL: "https://x/v1"}
	if err := p.Provision(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := p.Reconcile(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(sys.started)+len(sys.restarted) != 0 {
		t.Errorf("no systemd calls expected: %+v", sys)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no env file expected, got %d entries", len(entries))
	}
	if err := p.Teardown(context.Background(), row.Unit); err != nil || len(sys.stopped) != 1 {
		t.Errorf("teardown must still work: err=%v stopped=%v", err, sys.stopped)
	}
}

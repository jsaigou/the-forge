// SPDX-License-Identifier: Apache-2.0

package smith

// checks_tailscale.go implements smith P6's OS/Tailscale guidance module
// (docs/v5-smith.md §4.9 FR8, the tailnet half — kernel_params in checks.go
// already covers the boot-parameter half). A deep-sweep-only check: the
// tailnet doesn't fail fast enough to need a 60m quick-sweep cadence, and
// every fetch goes over the local tailscaled LocalAPI unix socket (fast,
// no network budget concerns like blocked_work_recheck's internet fetches).
//
// Deliberately NOT a hardcoded peer list: smith.tailscale.watch_peers
// (migration 0038, cleared to [] by migration 0044) is an operator-editable
// setting — an operator who wants peer watching adds the hostnames they
// consider infra-critical (e.g. core for LibreChat/Open Notebook); never
// invented in code. An empty setting means "nothing to watch", not an error.

import (
	"context"
	"fmt"
	"strings"
)

// tailscalePeerStatus is one watched peer's resolved state — the finding's
// evidence shape.
type tailscalePeerStatus struct {
	Name    string `json:"name"`
	DNSName string `json:"dns_name,omitempty"`
	Online  bool   `json:"online"`
	Found   bool   `json:"found"` // false: no peer's DNSName matched — distinct from "matched but offline"
}

// runTailscalePeers reports on smith.tailscale.watch_peers, matching
// NodeOnline's own "<name>." DNSName-prefix rule so smith's notion of "is
// this peer up" agrees with the collector's bookmark-health mechanism.
func runTailscalePeers(ctx context.Context, env *CheckEnv) Finding {
	const id = "tailscale_peers"
	if env.TailscalePeers == nil {
		return skipFinding(id, "tailscale LocalAPI not wired")
	}
	watch := env.TailscaleWatchPeers
	if len(watch) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary: "no watched tailscale peers configured (smith.tailscale.watch_peers is empty)"}
	}
	peers, ok := env.TailscalePeers(ctx)
	if !ok {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary: "could not reach tailscaled's LocalAPI to check peer status"}
	}

	statuses := make([]tailscalePeerStatus, 0, len(watch))
	var offline []string
	for _, name := range watch {
		st := tailscalePeerStatus{Name: name}
		for _, p := range peers {
			if strings.HasPrefix(p.DNSName, name+".") {
				st.Found = true
				st.DNSName = p.DNSName
				st.Online = p.Online
				break
			}
		}
		if !st.Found || !st.Online {
			offline = append(offline, name)
		}
		statuses = append(statuses, st)
	}

	ev := map[string]any{"watched": statuses}
	if len(offline) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d of %d watched tailscale peer(s) offline or unreachable: %s", len(offline), len(watch), strings.Join(offline, ", ")),
			Evidence: ev, KBRefs: []string{"infrastructure:tailscale-mesh"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary: fmt.Sprintf("all %d watched tailscale peer(s) online", len(watch)), Evidence: ev}
}

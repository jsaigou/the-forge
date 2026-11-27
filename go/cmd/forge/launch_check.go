// SPDX-License-Identifier: Apache-2.0

package main

// `forge launch-check [-db P]` is the read-only preflight for the slot_launches row (Radiance phase 1b step d1). It loads the infra config and the
// catalog through the daemon's own code (config.LoadFromStore and mergedConfigProvider.buildMerged) from a read-only database handle, then runs
// engine.BuildSlotLaunchRow, the function a load calls, for every config. It never boots a daemon and never writes. Exit 0: every config builds.
// Exit 1: at least one is refused (or was dropped by the merge). Exit 2: it could not run.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/store"
)

const defaultLaunchCheckDB = "/var/lib/forge/forge.db"

// launchCheckRow is one line of the report.
type launchCheckRow struct {
	Name, Engine, Backend, Result string
	OK                            bool
}

func runLaunchCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("launch-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultLaunchCheckDB, "state SQLite database (opened read-only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ro, err := store.OpenReadOnly(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "launch-check: %v\n", err)
		return 2
	}
	defer ro.Close()
	rows, err := launchCheck(context.Background(), ro)
	if err != nil {
		fmt.Fprintf(stderr, "launch-check: %v\n", err)
		return 2
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CONFIG\tENGINE\tBACKEND\tRESULT")
	bad := 0
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Engine, r.Backend, r.Result)
		if !r.OK {
			bad++
		}
	}
	tw.Flush()
	fmt.Fprintf(stdout, "%d configs, %d ok, %d refused\n", len(rows), len(rows)-bad, bad)
	if bad > 0 {
		return 1
	}
	return 0
}

// launchCheckStore is what the check reads; *store.ReadOnlyDB satisfies it.
type launchCheckStore interface {
	config.StoreReader
}

func launchCheck(ctx context.Context, st launchCheckStore) ([]launchCheckRow, error) {
	cfg, err := config.LoadFromStore(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	merged, err := newMergedConfigProvider(func() *config.Config { return cfg }, st.Catalog()).buildMerged(cfg)
	if err != nil {
		return nil, fmt.Errorf("merge catalog: %w", err)
	}
	slot, ok := firstSlot(cfg)
	if !ok {
		return nil, fmt.Errorf("no inference slot is configured")
	}

	var out []launchCheckRow
	seen := map[string]bool{}
	names := make([]string, 0, len(merged.Modes))
	for n := range merged.Modes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := merged.Modes[name]
		if mode.Type == "service" {
			continue
		}
		seen[name] = true
		row := launchCheckRow{Name: name, Result: "OK", OK: true}
		if len(mode.Services) == 0 {
			row.Result, row.OK = "no service in config", false
			out = append(out, row)
			continue
		}
		svc := mode.Services[0]
		svc.PortRole = slot // the same override Load applies; the slot only changes the port
		row.Backend = svc.Backend
		if spec, err := svc.Engine(); err == nil {
			row.Engine = spec.Name
		}
		if _, err := engine.BuildSlotLaunchRow(cfg, slot, name, mode.ConfigID, svc); err != nil {
			row.Result, row.OK = oneLine(err.Error()), false
		}
		out = append(out, row)
	}

	// Configs the merge dropped never reach a load either; list them so none is silently absent.
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	all, err := st.Catalog().ListConfigs(listCtx)
	if err != nil {
		return nil, fmt.Errorf("list configs: %w", err)
	}
	for _, c := range all {
		if !seen[c.Name] {
			out = append(out, launchCheckRow{Name: c.Name, Result: "dropped by the merged-config conversion (no backend or unknown engine kind; see the log above)"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func firstSlot(cfg *config.Config) (string, bool) {
	best, found := "", false
	for n, s := range cfg.Slots {
		if !found || s.Order < cfg.Slots[best].Order || s.Order == cfg.Slots[best].Order && n < best {
			best, found = n, true
		}
	}
	return best, found
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

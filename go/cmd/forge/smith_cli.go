// SPDX-License-Identifier: Apache-2.0

package main

// smith_cli.go — smith's operator CLI. Currently one subcommand:
//
//	forge smith import-local <file>
//
// the layer-2 provisioning seam of the two-layer knowledge architecture
// (internal/smith/local_seed.go): product knowledge ships with the binary;
// live-environment data (mesh inventory, build_refresh fork recipes,
// tracked binaries) is per-install and imported from an operator-maintained
// local file that never ships. The file's schema is smith.LocalSeed;
// docs/examples/smith-local-seed.example.json in the repo shows the shape
// with synthetic values.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/store"
)

func runSmithCLI(args []string) error {
	if len(args) == 0 {
		return errors.New(i18n.T("forge.smith.usage"))
	}
	switch args[0] {
	case "import-local":
		return runSmithImportLocal(args[1:])
	default:
		return fmt.Errorf("%s", i18n.T("forge.smith.unknown_subcommand", args[0]))
	}
}

// runSmithImportLocal reads the local seed file and applies its sections
// to the live settings store — present sections replace wholesale, absent
// sections are untouched (internal/smith/local_seed.go). Idempotent: run
// it again after editing the file whenever the deployment changes.
func runSmithImportLocal(args []string) error {
	fs := flag.NewFlagSet("smith import-local", flag.ContinueOnError)
	dbPath := fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New(i18n.T("forge.smith.import_usage"))
	}

	raw, err := os.ReadFile(rest[0])
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.smith.err_read_seed_file"), err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_db"), err)
	}
	defer db.Close()

	sum, err := smith.ImportLocalSeed(context.Background(), db.Settings(), raw)
	if err != nil {
		return err
	}

	reportSection := func(name string, n int) {
		if n < 0 {
			fmt.Println(i18n.T("forge.smith.not_in_file", name))
		} else {
			fmt.Println(i18n.T("forge.smith.imported_entries", name, n))
		}
	}
	reportSection("smith.mesh.services", sum.MeshServices)
	reportSection("smith.build_refresh.forks", sum.BuildRefreshForks)
	reportSection("smith.binaries.tracked", sum.BinariesTracked)
	if sum.WebProviders < 0 {
		fmt.Println(i18n.T("forge.smith.web_providers_absent"))
	} else {
		fmt.Println(i18n.T("forge.smith.web_providers_imported", sum.WebProviders))
	}
	if sum.ComfyUI < 0 {
		fmt.Println(i18n.T("forge.smith.comfyui_absent"))
	} else {
		fmt.Println(i18n.T("forge.smith.comfyui_imported"))
	}
	fmt.Println(i18n.T("forge.smith.daemon_live_note"))
	return nil
}

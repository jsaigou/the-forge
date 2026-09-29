// SPDX-License-Identifier: Apache-2.0

package main

// config_cli.go — the editability CLI TOML decommission Phase 3 requires
// (docs/v5-toml-decommission.md §7): once there's no forge.toml/router.toml
// to `vim` + restart, an operator needs *some* real edit path for the store-
// backed `infra.*` settings or the system is less operable post-cutover than
// it was before it. Thin CLI over store.Settings() — no FE work needed.
//
// `config dump` is a read-out only (backup/diff/audit), never a read path
// back into the app — the store stays the single source of truth. This is
// what preserves the git-diffable-facts value the original file-based design
// decision protected, without making a file authoritative again.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/router"
	"github.com/jsaigou/the-forge/internal/store"
)

func runConfigCLI(args []string) error {
	if len(args) == 0 {
		return errors.New(i18n.T("forge.config.usage"))
	}
	switch args[0] {
	case "get":
		return runConfigGet(args[1:])
	case "set":
		return runConfigSet(args[1:])
	case "dump":
		return runConfigDump(args[1:])
	default:
		return fmt.Errorf("%s", i18n.T("forge.config.unknown_subcommand", args[0]))
	}
}

// runConfigGet prints a settings key's raw JSON value to stdout.
func runConfigGet(args []string) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	dbPath := fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New(i18n.T("forge.config.get_usage"))
	}
	key := rest[0]

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_db"), err)
	}
	defer db.Close()

	raw, err := db.Settings().Get(context.Background(), key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%s", i18n.T("forge.config.key_not_set", key))
		}
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_get", key), err)
	}
	fmt.Println(string(raw))
	return nil
}

// runConfigSet writes a settings key. value must be valid JSON — the
// settings store is a JSON-KV table (docs/v5-store-schema.md), and every
// reader (config.LoadFromStore, router.LoadFromStore, the scheduler, etc.)
// unmarshals it as JSON, so a non-JSON value would silently break the next
// reload rather than fail loudly here.
func runConfigSet(args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	dbPath := fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New(i18n.T("forge.config.set_usage"))
	}
	key, value := rest[0], rest[1]

	var probe any
	if err := json.Unmarshal([]byte(value), &probe); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_invalid_json"), err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_db"), err)
	}
	defer db.Close()

	if err := db.Settings().Set(context.Background(), key, []byte(value)); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_set", key), err)
	}
	fmt.Printf("%s = %s\n", key, value)
	fmt.Println(i18n.T("forge.config.set_note"))
	return nil
}

// runConfigDump serializes the current store-backed infra config (the
// forge.toml/router.toml shapes, minus Modes — those are catalog-owned
// and have their own dump path via /api/v1/catalog/*) as JSON on stdout.
// Read-only: never parsed back into the app by anything.
//
// JSON, not TOML: this used to marshal through pelletier/go-toml/v2 for a
// file-shaped snapshot, but TOML decommission Phase 8 (docs/v5-toml-
// decommission.md §8) retires that dependency from the whole binary once
// migrate-v4/migrate-infra-to-db are gone — keeping it alive just for this
// one read-out would defeat the point. JSON is also the more honest format
// here: the underlying settings rows are already JSON, this just pretty-
// prints them.
func runConfigDump(args []string) error {
	fs := flag.NewFlagSet("config dump", flag.ContinueOnError)
	dbPath := fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_db"), err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg, err := config.LoadFromStore(ctx, db)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_load_config"), err)
	}
	cfgBody, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_marshal_config"), err)
	}
	fmt.Println(i18n.T("forge.config.dump_header_infra"))
	fmt.Println(i18n.T("forge.config.dump_readonly_infra"))
	fmt.Println(i18n.T("forge.config.dump_note_readonly"))
	fmt.Println(i18n.T("forge.config.dump_modes_empty"))
	fmt.Println(string(cfgBody))

	routerCfg, err := router.LoadFromStore(ctx, db)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_load_router_config"), err)
	}
	routerBody, err := json.MarshalIndent(routerCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_marshal_router_config"), err)
	}
	fmt.Println(i18n.T("forge.config.dump_header_router"))
	fmt.Println(i18n.T("forge.config.dump_readonly_router"))
	fmt.Println(i18n.T("forge.config.dump_note_readonly"))
	fmt.Println(i18n.T("forge.config.dump_router_note1"))
	fmt.Println(i18n.T("forge.config.dump_router_note2"))
	fmt.Println(string(routerBody))
	return nil
}

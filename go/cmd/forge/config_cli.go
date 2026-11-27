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
	"os"
	"os/user"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/router"
	"github.com/jsaigou/the-forge/internal/settingsvc"
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
	force := fs.Bool("force", false, i18n.T("forge.config.flag_force"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New(i18n.T("forge.config.set_usage"))
	}
	key, value := rest[0], rest[1]

	// One validation path for every writer of a setting by name (ADR-0020 F7): JSON syntax, then whether any reader
	// uses the key, then the shape its reader expects.
	if err := settingsvc.Check(key, []byte(value), *force); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_rejected", key), err)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_db"), err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.Settings().Set(ctx, key, []byte(value)); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_set", key), err)
	}
	// The API handlers audit every settings write; this path used to leave no record. The value is not logged
	// (a setting can hold a credential), only which key changed and how large the new value is.
	detail, _ := json.Marshal(map[string]any{"bytes": len(value), "forced": *force && !settingsvc.Known(key)})
	if err := db.Audit().Write(ctx, store.AuditEntry{
		TS: time.Now(), Actor: cliActor(), Action: "settings_set", Target: key, Detail: string(detail),
	}); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.config.err_audit", key), err)
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

// cliActor names who ran a CLI command for the audit log: "cli:<login>", falling back to $USER, then "unknown".
func cliActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return "cli:" + n
	}
	return "cli:unknown"
}

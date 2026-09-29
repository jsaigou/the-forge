// SPDX-License-Identifier: Apache-2.0

package main

// keys_cli.go — key introspection + revocation (docs/v5-mcp-audit.md
// roadmap R4, closing finding F7: auditing keys used to require raw DB
// access, and ForgeHost doesn't even have a sqlite3 CLI). `list` prints only
// non-sensitive columns — kind/name/keyid/role/created/last-used/revoked;
// the secret hash never leaves the store, nothing printed can authenticate.
// `revoke` closes a real, previously-flagged gap (no CLI revoke path
// existed at all — a leaked/compromised key could only be revoked via a
// stepped-up browser session). It matters more as of security sprint 3
// (#37): a bearer key can no longer DELETE /api/v1/keys under any
// circumstance, including itself, so this host-side command is now the
// only non-interactive way to revoke a key at all.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/store"
)

func runKeysCLI(args []string) error {
	if len(args) == 0 {
		return errors.New(i18n.T("forge.keys.usage"))
	}
	switch args[0] {
	case "list":
		return runKeysList(args[1:])
	case "revoke":
		return runKeysRevoke(args[1:])
	case "export":
		// Common typo: "forge keys export" (space) lands here, but the real
		// verb is the hyphenated CLI/TUI client command `forge keys-export`
		// (dispatched in cli_tui.go's isCLIVerb, talks to the running
		// daemon's API — a different mechanism from this host-side `keys`
		// family entirely). Point at it instead of a bare "unknown".
		return fmt.Errorf("%s", i18n.T("forge.keys.export_typo", args[0]))
	default:
		return fmt.Errorf("%s", i18n.T("forge.keys.unknown_subcommand", args[0]))
	}
}

// humanUTC renders a timestamp human-readably in UTC; zero → "never".
func humanUTC(t time.Time) string {
	if t.IsZero() {
		return i18n.T("forge.keys.never")
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// runKeysList prints one row per bearer key. Read-only: it goes through
// store.Keys().List and deliberately drops the only sensitive column the
// row carries (SecretHash) before anything reaches stdout.
func runKeysList(args []string) error {
	fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
	var (
		dbPath = fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		kind   = fs.String("kind", "", i18n.T("forge.keys.flag_kind"))
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *kind {
	case "", "forge", "router", "mcp":
	default:
		return fmt.Errorf("%s", i18n.T("forge.keys.err_kind_invalid", *kind))
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_state_db"), err)
	}
	defer db.Close()

	keys, err := db.Keys().List(context.Background(), *kind)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.keys.err_list"), err)
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, i18n.T("forge.keys.list_header"))
	for _, k := range keys {
		role := k.Role
		if role == "" {
			role = "-"
		}
		revoked := i18n.T("forge.keys.status_active")
		if !k.RevokedAt.IsZero() {
			revoked = humanUTC(k.RevokedAt)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.Kind, k.Name, k.KeyID, role,
			humanUTC(k.CreatedAt), humanUTC(k.LastUsedAt), revoked)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.keys.err_write_output"), err)
	}

	// Summary on stderr, mint-key style — stdout stays a clean table.
	fmt.Fprintf(os.Stderr, "%s", i18n.T("forge.keys.count_summary", len(keys)))
	if *kind != "" {
		fmt.Fprintf(os.Stderr, "%s", i18n.T("forge.keys.count_of_kind", *kind))
	}
	fmt.Fprintln(os.Stderr, i18n.T("forge.keys.hashes_never_printed"))
	return nil
}

// runKeysRevoke soft-revokes one key by keyid, direct against the store —
// no HTTP round-trip, no bearer/session auth involved (host access to the
// DB file is the trust boundary here, same as mint-key and keys list).
func runKeysRevoke(args []string) error {
	fs := flag.NewFlagSet("keys revoke", flag.ContinueOnError)
	var (
		dbPath = fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		keyid  = fs.String("keyid", "", i18n.T("forge.keys.flag_keyid"))
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyid == "" {
		return errors.New(i18n.T("forge.keys.keyid_required"))
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_state_db"), err)
	}
	defer db.Close()

	if err := db.Keys().Revoke(context.Background(), *keyid); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.keys.err_revoke", *keyid), err)
	}
	fmt.Fprintln(os.Stderr, i18n.T("forge.keys.revoked", *keyid))
	return nil
}

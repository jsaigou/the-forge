// SPDX-License-Identifier: Apache-2.0

package main

// forge mint-key: mints a bearer key for an external consumer (a0/
// router clients like OpenCode/LibreChat, or MCP-driven agents) against the
// real state DB. Ported gap: authz.Authorizer.MintKey has existed (fully
// tested) since Phase 3, but nothing ever called it — no CLI subcommand,
// no HTTP endpoint. migrate-v4 deliberately does not carry V4's bearer
// keys ("re-mint post-cutover"), so this is the only way to get a
// consumer working again after a V4→V5 cutover. One key per (kind, name):
// MintKey revokes any existing key with the same kind+name first (V4 mint
// semantics), so re-running this for the same consumer rotates its key
// rather than accumulating stale ones.
//
// Prints ONLY the full token to stdout (so it's cleanly copyable/
// redirectable); everything else goes to stderr. Never logs the secret.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

func runMintKey(args []string) error {
	fs := flag.NewFlagSet("mint-key", flag.ContinueOnError)
	var (
		dbPath  = fs.String("db", "/var/lib/forge/forge.db", "state SQLite database")
		kind    = fs.String("kind", "", "key kind: forge (dashboard API), router (a0), or mcp")
		name    = fs.String("name", "", "consumer name (e.g. opencode-examplehost) — re-minting the same name+kind rotates its key")
		role    = fs.String("role", "", "RBAC role for a forge key: viewer, operator, or admin (required for -kind forge, ignored otherwise)")
		display = fs.String("display-name", "", "preferred human-facing consumer label (slot attribution); optional")
		bindIP  = fs.String("bind-ip", "", "restrict this key to requests from exactly this client IP; empty (default) = unbound")
		ttl     = fs.Duration("ttl", 0, "expire this key after the given duration (e.g. 2160h for 90 days); 0 (default) = never expires")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ttl < 0 {
		return fmt.Errorf("-ttl must be >= 0")
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	var kk authz.KeyKind
	switch *kind {
	case "forge", "foundry": // "foundry" = legacy pre-rebrand alias
		kk = authz.KindForge
	case "router":
		kk = authz.KindRouter
	case "mcp":
		kk = authz.KindMCP
	default:
		return fmt.Errorf("-kind must be one of: forge, router, mcp (got %q)", *kind)
	}
	var r authz.Role
	if kk == authz.KindForge {
		switch *role {
		case "viewer":
			r = authz.RoleViewer
		case "operator":
			r = authz.RoleOperator
		case "admin":
			r = authz.RoleAdmin
		default:
			return fmt.Errorf("-role must be one of: viewer, operator, admin (got %q) for -kind forge", *role)
		}
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer db.Close()

	var expiresAt time.Time
	if *ttl > 0 {
		expiresAt = time.Now().Add(*ttl)
	}

	auth := authz.New(db)
	token, err := auth.MintKey(context.Background(), kk, *name, *display, r, *bindIP, expiresAt)
	if err != nil {
		return fmt.Errorf("mint key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "minted %s key %q — token below (shown once, not recoverable):\n", *kind, *name)
	fmt.Println(token)
	return nil
}

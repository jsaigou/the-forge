// SPDX-License-Identifier: Apache-2.0

package main

// catalog_cli.go — host-side catalog maintenance, same shape as keys_cli.go
// (direct SQLite access via -db, no HTTP/auth — host access to the DB file
// is the trust boundary). Built for the S7-followup catalog hygiene sprint:
// live queries against ForgeHost found 16/21 artifacts (mostly pre-HF-
// acquisition legacy catalog entries) have empty gguf_arch/gguf_trained_ctx,
// and one confirmed byte-identical duplicate artifact row (variant 4,
// gemma4-26b-a4b-mtp, ids 6 and 7 — both configs reference weight_artifact_id
// 6, so 7 is the unreferenced orphan).
//
// backfill-gguf re-reads each artifact's real file via the same
// gguf.ReadMetadata the HF-download registrar already uses
// (internal/hfdownload/registrar.go) — never fabricates a value. A missing
// or unreadable file is logged and skipped, not guessed. Idempotent: rows
// that already carry a non-empty gguf_arch are skipped, so it's safe to
// re-run as more legacy entries get real files.
//
// delete-artifact is a generic, explicit single-row delete (mirrors `keys
// revoke`'s directness) rather than a one-off "dedupe" script — the FK on
// configs.weight_artifact_id is ON DELETE RESTRICT (foreign_keys=ON per
// store/db.go), so deleting a row still referenced by a config fails loudly
// instead of silently orphaning it.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/store"
)

func runCatalogCLI(args []string) error {
	if len(args) == 0 {
		return errors.New(i18n.T("forge.catalog.usage"))
	}
	switch args[0] {
	case "backfill-gguf":
		return runCatalogBackfillGGUF(args[1:])
	case "delete-artifact":
		return runCatalogDeleteArtifact(args[1:])
	default:
		return fmt.Errorf("%s", i18n.T("forge.catalog.unknown_subcommand", args[0]))
	}
}

// infraPaths mirrors the subset of httpapi's infra.paths shape this needs —
// duplicated rather than imported to keep this host-side tool decoupled from
// httpapi's larger dependency graph (same reasoning as keys_cli.go/mint_key.go
// only depending on internal/store).
type infraPaths struct {
	ModelsDir string `json:"models_dir"`
}

func runCatalogBackfillGGUF(args []string) error {
	fs := flag.NewFlagSet("catalog backfill-gguf", flag.ContinueOnError)
	var (
		dbPath  = fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		dryRun  = fs.Bool("dry-run", false, i18n.T("forge.catalog.flag_dry_run"))
		verbose = fs.Bool("v", false, i18n.T("forge.catalog.flag_verbose"))
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_state_db"), err)
	}
	defer db.Close()

	ctx := context.Background()
	raw, err := db.Settings().Get(ctx, "infra.paths")
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.catalog.err_read_infra_paths"), err)
	}
	var paths infraPaths
	if err := json.Unmarshal(raw, &paths); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.catalog.err_parse_infra_paths"), err)
	}
	if paths.ModelsDir == "" {
		return errors.New(i18n.T("forge.catalog.err_models_dir_unset"))
	}

	artifacts, err := db.Catalog().ListArtifacts(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.catalog.err_list_artifacts"), err)
	}

	var updated, alreadySet, missingFile, unreadable int
	for _, a := range artifacts {
		if a.GGUFArch != "" {
			alreadySet++
			if *verbose {
				fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.skip_already_set", a.ID, a.GGUFArch))
			}
			continue
		}
		full := filepath.Join(paths.ModelsDir, a.FilePath)
		if _, statErr := os.Stat(full); statErr != nil {
			missingFile++
			fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.skip_missing_file", a.ID, a.FilePath, full))
			continue
		}
		md, mdErr := gguf.ReadMetadata(full)
		if mdErr != nil {
			unreadable++
			fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.skip_unreadable", a.ID, a.FilePath, mdErr))
			continue
		}
		a.GGUFArch = md.Architecture
		a.GGUFTrainedCtx = md.TrainedCtx
		a.GGUFParameterCount = ggufParamStringCLI(md)
		a.GGUFQuantType = md.QuantType

		if *dryRun {
			fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.would_update", a.ID, a.FilePath, a.GGUFArch, a.GGUFTrainedCtx))
			updated++
			continue
		}
		if err := db.Catalog().UpdateArtifact(ctx, a); err != nil {
			return fmt.Errorf("%s: %w", i18n.T("forge.catalog.err_update_artifact", a.ID), err)
		}
		fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.updated", a.ID, a.FilePath, a.GGUFArch, a.GGUFTrainedCtx))
		updated++
	}

	verb := i18n.T("forge.catalog.verb_updated")
	if *dryRun {
		verb = i18n.T("forge.catalog.verb_would_update")
	}
	fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.summary", verb, updated, alreadySet, missingFile, unreadable, len(artifacts)))
	return nil
}

// ggufParamStringCLI mirrors hfdownload's unexported ggufParamString
// (internal/hfdownload/registrar.go) — same one-line rule (empty rather
// than "0" when the header carries no parameter count), duplicated instead
// of exported solely for this CLI tool's sake.
func ggufParamStringCLI(md gguf.Metadata) string {
	if md.ParameterCount <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", md.ParameterCount)
}

func runCatalogDeleteArtifact(args []string) error {
	fs := flag.NewFlagSet("catalog delete-artifact", flag.ContinueOnError)
	var (
		dbPath = fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		id     = fs.Int64("id", 0, i18n.T("forge.catalog.flag_id"))
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == 0 {
		return errors.New(i18n.T("forge.catalog.id_required"))
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.cli.err_open_state_db"), err)
	}
	defer db.Close()

	if err := db.Catalog().DeleteArtifact(context.Background(), *id); err != nil {
		return fmt.Errorf("%s: %w", i18n.T("forge.catalog.err_delete_artifact", *id), err)
	}
	fmt.Fprintln(os.Stderr, i18n.T("forge.catalog.deleted", *id))
	return nil
}

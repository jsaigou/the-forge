// SPDX-License-Identifier: Apache-2.0

package main

// `forge catalog register-build` records a llama.cpp server that is on this host as a catalog build, through the same guarded API the web page uses
// (the binary must be an executable under catalog.build_roots). A model added without any build cannot be loaded, so the installer registers the
// servers it finds. A binary that is registered already is left alone.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/i18n"
)

func runCatalogRegisterBuild(args []string) error {
	fl := flag.NewFlagSet("catalog register-build", flag.ContinueOnError)
	var (
		binary  = fl.String("binary", "", "the llama-server program (absolute path)")
		backend = fl.String("backend", "vulkan", "vulkan or rocm")
		name    = fl.String("name", "", "the build's name (default: llama.cpp (<backend>))")
		dbPath  = fl.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	)
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 || !path.IsAbs(*binary) || (*backend != "vulkan" && *backend != "rocm") {
		return errors.New(i18n.T("forge.catalog.register_build_usage"))
	}
	if st, err := os.Stat(*binary); err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return errors.New(i18n.T("forge.catalog.register_build_not_a_program", *binary))
	}
	if *name == "" {
		*name = "llama.cpp (" + *backend + ")"
	}
	client, revoke, err := scopedClient(*dbPath, authz.ScopeCatalog, "llama-register")
	if err != nil {
		return err
	}
	defer revoke()
	out, err := registerLlamaBuild(client, *name, path.Clean(*binary), *backend)
	if err != nil {
		return err
	}
	key := "forge.catalog.register_build_already"
	if out.Created {
		key = "forge.catalog.register_build_done"
	}
	fmt.Println(i18n.T(key, out.Name, out.ID))
	return nil
}

// registerLlamaBuild creates the build, unless one in use already runs that binary.
func registerLlamaBuild(api catalogAPI, name, binary, backend string) (registerOutcome, error) {
	var builds []struct {
		catalogBuild
		Retired bool `json:"retired"`
	}
	if err := api.GetJSON("/api/v1/catalog/builds", &builds); err != nil {
		return registerOutcome{}, fmt.Errorf("list builds: %w", err)
	}
	for _, b := range builds {
		if !b.Retired && b.BinaryPath == binary {
			return registerOutcome{ID: b.ID, Name: b.Name}, nil
		}
	}
	return registerRadianceBuild(api, enginekind.LlamaCpp, name, binary, backend, nil)
}

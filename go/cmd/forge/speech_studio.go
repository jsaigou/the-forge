// SPDX-License-Identifier: Apache-2.0

package main

// `forge speech studio-link <url>` records where Voice Studio is, as a Console link (docs/speech-setup-plan.md section 5.4). A link to a Voice
// Studio that is already there, such as one on another host, is left alone.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/store"
)

const externalToolsKey = "infra.external_tools"

func runStudioLink(args []string) error {
	fl := flag.NewFlagSet("forge speech studio-link", flag.ContinueOnError)
	dbPath := fl.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	if len(args) < 1 {
		return errors.New(i18n.T("cli.speech.usage"))
	}
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return errors.New(i18n.T("cli.speech.usage"))
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // the setting is committed when Set returns
	added, at, err := addStudioLink(context.Background(), db, args[0])
	if err != nil {
		return err
	}
	if added {
		fmt.Println(i18n.T("cli.speech.studio.added", at))
	} else {
		fmt.Println(i18n.T("cli.speech.studio.kept", at))
	}
	return nil
}

// addStudioLink appends a Voice Studio entry to the external tools unless one is there; at is the address the Console ends up with.
func addStudioLink(ctx context.Context, db *store.DB, address string) (added bool, at string, err error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false, "", errors.New(i18n.T("cli.speech.studio.bad_url", address))
	}
	var tools []config.ExternalTool
	raw, err := db.Settings().Get(ctx, externalToolsKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return false, "", err
	default:
		if err := json.Unmarshal(raw, &tools); err != nil {
			return false, "", fmt.Errorf("%s is not readable, so it was left alone: %w", externalToolsKey, err)
		}
	}
	for _, t := range tools {
		if config.IsVoiceStudio(t) {
			return false, t.URL, nil
		}
	}
	tools = append(tools, config.ExternalTool{Name: "Voice Studio", URL: address})
	next, err := json.Marshal(tools)
	if err != nil {
		return false, "", err
	}
	if err := db.Settings().Set(ctx, externalToolsKey, next); err != nil {
		return false, "", err
	}
	if err := db.Audit().Write(ctx, store.AuditEntry{TS: time.Now(), Actor: cliActor(), Action: "settings_set", Target: externalToolsKey, Detail: `{"added":"Voice Studio"}`}); err != nil {
		return true, address, fmt.Errorf("the link was added but not recorded in the audit log: %w", err)
	}
	return true, address, nil
}

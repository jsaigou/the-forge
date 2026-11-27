// SPDX-License-Identifier: Apache-2.0

package main

// `forge host-setup` gives a new installation what no migration does: the four model slots (their units are installed beside it) and the folders the
// daemon works with, without which no language model can be downloaded or loaded, and makes downloaded models appear at once. It fills in only what is
// missing, so running it again, or on a host that is already set up, changes nothing. The installer runs it as the service user before the daemon
// starts.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/store"
)

// modelSlots are the model slots of a new installation. The ports are the ones the rest of the product assumes (config.Slot).
var modelSlots = []store.Slot{
	{Name: "a1", Unit: "forge-a1", Port: 8080, Label: "A1", SortOrder: 1},
	{Name: "a2", Unit: "forge-a2", Port: 8081, Label: "A2", SortOrder: 2},
	{Name: "a3", Unit: "forge-a3", Port: 8087, Label: "A3", SortOrder: 3},
	{Name: "a4", Unit: "forge-a4", Port: 8088, Label: "A4", SortOrder: 4},
}

type hostSetup struct {
	SlotsAdded []string
	PathsSet   []string
	// ShowDownloads: new downloads were set to appear at once (a new installation, with no model configured yet).
	ShowDownloads bool
}

func runHostSetup(args []string) error {
	fl := flag.NewFlagSet("forge host-setup", flag.ContinueOnError)
	var (
		dbPath = fl.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		models = fl.String("models-dir", "", "where language models are kept (required)")
		state  = fl.String("state-dir", "/var/lib/forge", "where the daemon keeps its state")
		vulkan = fl.String("vulkan-bin", "", "a Vulkan llama-server, recorded if the file exists")
		rocm   = fl.String("rocm-bin", "", "a ROCm llama-server, recorded if the file exists")
	)
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 || !path.IsAbs(*models) || !path.IsAbs(*state) {
		return errors.New(i18n.T("forge.hostsetup.usage"))
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // every write is committed when its call returns
	paths := map[string]string{"models_dir": *models, "state_dir": *state, "icons_dir": path.Join(*state, "icons")}
	for key, bin := range map[string]string{"vulkan_bin": *vulkan, "rocm_bin": *rocm} {
		if st, err := os.Stat(bin); bin != "" && err == nil && st.Mode().IsRegular() {
			paths[key] = bin
		}
	}
	done, err := setUpHost(context.Background(), db, paths)
	if err != nil {
		return err
	}
	if len(done.SlotsAdded) == 0 && len(done.PathsSet) == 0 && !done.ShowDownloads {
		fmt.Println(i18n.T("forge.hostsetup.unchanged"))
		return nil
	}
	if len(done.SlotsAdded) > 0 {
		fmt.Println(i18n.T("forge.hostsetup.slots", fmt.Sprint(done.SlotsAdded)))
	}
	if len(done.PathsSet) > 0 {
		fmt.Println(i18n.T("forge.hostsetup.paths", fmt.Sprint(done.PathsSet)))
	}
	if done.ShowDownloads {
		fmt.Println(i18n.T("forge.hostsetup.show_downloads", hfdownload.SettingShowNewDownloads))
	}
	return nil
}

// setUpHost adds the model slots when the host has none, and sets each wanted path that is not set. A host with any model slot keeps exactly the
// slots it has; a path that is set keeps its value.
func setUpHost(ctx context.Context, db *store.DB, paths map[string]string) (hostSetup, error) {
	var done hostSetup
	existing, err := db.Catalog().ListSlots(ctx)
	if err != nil {
		return done, err
	}
	hasModelSlot := false
	for _, s := range existing {
		hasModelSlot = hasModelSlot || s.Pool != store.PoolSpecialist
	}
	if !hasModelSlot {
		taken := map[int]string{}
		for _, s := range existing {
			taken[s.Port] = s.Name
		}
		// All or none: a host left with some of its model slots would count as set up on the next run.
		for _, s := range modelSlots {
			if by, used := taken[s.Port]; used {
				return done, fmt.Errorf("port %d of model slot %s is already used by slot %s", s.Port, s.Name, by)
			}
		}
		for _, s := range modelSlots {
			s.Pool = store.PoolLLM
			if _, err := db.Catalog().CreateSlot(ctx, s); err != nil {
				return done, err
			}
			done.SlotsAdded = append(done.SlotsAdded, s.Name)
		}
	}

	stored := map[string]json.RawMessage{}
	raw, err := db.Settings().Get(ctx, "infra.paths")
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return done, err
	default:
		if err := json.Unmarshal(raw, &stored); err != nil {
			return done, fmt.Errorf("infra.paths is not readable, so it was left alone: %w", err)
		}
	}
	var current config.Paths
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return done, fmt.Errorf("infra.paths is not readable, so it was left alone: %w", err)
		}
	}
	have := map[string]string{"models_dir": current.ModelsDir, "state_dir": current.StateDir, "icons_dir": current.IconsDir,
		"vulkan_bin": current.VulkanBin, "rocm_bin": current.RocmBin}
	for _, key := range []string{"models_dir", "state_dir", "icons_dir", "vulkan_bin", "rocm_bin"} {
		want := paths[key]
		if want == "" || have[key] != "" {
			continue
		}
		value, err := json.Marshal(want)
		if err != nil {
			return done, err
		}
		stored[key] = value
		done.PathsSet = append(done.PathsSet, key)
	}
	if len(done.PathsSet) > 0 {
		next, err := json.Marshal(stored)
		if err != nil {
			return done, err
		}
		if err := db.Settings().Set(ctx, "infra.paths", next); err != nil {
			return done, err
		}
	}
	// A new installation shows what it downloads. One that already has models keeps hiding new ones until they are made visible, and a choice that
	// was made stays.
	switch _, err := db.Settings().Get(ctx, hfdownload.SettingShowNewDownloads); {
	case errors.Is(err, store.ErrNotFound):
		configs, err := db.Catalog().ListConfigs(ctx)
		if err != nil {
			return done, err
		}
		if len(configs) == 0 {
			if err := db.Settings().Set(ctx, hfdownload.SettingShowNewDownloads, []byte("true")); err != nil {
				return done, err
			}
			done.ShowDownloads = true
		}
	case err != nil:
		return done, err
	}
	if len(done.SlotsAdded) > 0 || len(done.PathsSet) > 0 || done.ShowDownloads {
		detail, err := json.Marshal(done)
		if err != nil {
			return done, err
		}
		if err := db.Audit().Write(ctx, store.AuditEntry{TS: time.Now(), Actor: cliActor(), Action: "host_setup", Target: "slots,infra.paths,catalog", Detail: string(detail)}); err != nil {
			return done, fmt.Errorf("the host was set up but this was not recorded in the audit log: %w", err)
		}
	}
	return done, nil
}

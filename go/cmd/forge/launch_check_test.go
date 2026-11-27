// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func seedLaunchCheckDB(t *testing.T, bad bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cat := db.Catalog()
	if _, err := cat.CreateSlot(ctx, store.Slot{Name: "a1", Unit: "forge-a1", Port: 8080, Label: "A1", SortOrder: 1}); err != nil {
		t.Fatal(err)
	}
	mdl, _ := cat.CreateModel(ctx, store.Model{Name: "M"})
	vr, _ := cat.CreateVariant(ctx, store.Variant{ModelID: mdl, Name: "base"})
	format, _ := cat.FormatByName(ctx, "safetensors")
	weight, _ := cat.CreateArtifact(ctx, store.Artifact{VariantID: vr, FormatID: format.ID, FilePath: "/models/m.gguf", ArtifactType: "weight"})
	llama, _ := cat.EngineByName(ctx, "llama.cpp")
	add := func(name, backend string, args []string, withBuild bool) {
		t.Helper()
		bin := "/opt/x/llama-server"
		if name == "bad-relative-bin" {
			bin = "llama-server"
		}
		var bid int64
		if withBuild {
			var err error
			if bid, err = cat.CreateBuild(ctx, store.Build{EngineID: llama.ID, Name: "b-" + name, Backend: backend, BinaryPath: bin}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := cat.CreateConfig(ctx, store.Config{Name: name, VariantID: vr, WeightArtifactID: weight, EngineID: llama.ID, BuildID: bid, NCtx: 4096, ExtraArgs: args}); err != nil {
			t.Fatal(err)
		}
	}
	add("good-rocm", "rocm", []string{"--parallel", "1"}, true)
	add("good-vulkan", "vulkan", nil, true)
	if bad {
		add("bad-relative-bin", "rocm", nil, true) // the catalog CHECK stops an unknown backend ever being stored
		add("bad-blank-arg", "rocm", []string{"--jinja", "  "}, true)
		add("bad-quote-alias", "rocm", nil, true)
		if _, err := db.SQL().ExecContext(ctx, `UPDATE configs SET name = 'has"quote' WHERE name = 'bad-quote-alias'`); err != nil {
			t.Fatal(err)
		}
		add("dropped-no-build", "", nil, false)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLaunchCheckAllGood(t *testing.T) {
	path := seedLaunchCheckDB(t, false)
	var out, errb bytes.Buffer
	if code := runLaunchCheck([]string{"-db", path}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\nout: %s\nerr: %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "good-rocm") || !strings.Contains(out.String(), "2 configs, 2 ok, 0 refused") {
		t.Errorf("out = %s", out.String())
	}
}

func TestLaunchCheckReportsEachRefusalClassAndIsReadOnly(t *testing.T) {
	path := seedLaunchCheckDB(t, true)
	before, _ := os.ReadFile(path)
	var out, errb bytes.Buffer
	code := runLaunchCheck([]string{"-db", path}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d\nout: %s\nerr: %s", code, out.String(), errb.String())
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	for name, want := range map[string]string{
		"good-rocm":        "OK",
		"good-vulkan":      "OK",
		"bad-relative-bin": "exe",
		"bad-blank-arg":    "extra arg 1 is empty",
		"has\"quote":       "alias contains a quote",
		"dropped-no-build": "dropped",
	} {
		if l, ok := lines[name]; !ok || !strings.Contains(l, want) {
			t.Errorf("%s: line %q does not contain %q\nfull: %s", name, l, want, out.String())
		}
	}
	if !strings.Contains(out.String(), "refused") {
		t.Errorf("no summary: %s", out.String())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("database file changed")
	}
}

func TestLaunchCheckMissingDB(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runLaunchCheck([]string{"-db", filepath.Join(t.TempDir(), "nope.db")}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "launch-check") {
		t.Errorf("code %d err %q", code, errb.String())
	}
}

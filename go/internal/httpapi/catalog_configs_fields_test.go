// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonKeys lists the JSON field names of a struct (sorted), skipping fields tagged "-".
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// configFieldsGolden is the Config field list the web app's types are checked against (web/scripts/config-fields.test.mjs reads the same file).
// view is what GET returns; write is what PUT/POST accepts (the note in "reason" is not a Config field). Adding a Config field changes
// this list; regenerate it with UPDATE_GOLDEN=1 go test ./internal/httpapi -run TestConfigFieldsGolden, then update web/src/lib/types.ts
// and CATALOG_CONFIG_KEYS in web/src/lib/configPayload.ts so both tests pass.
type configFieldsGolden struct {
	View  []string `json:"view"`
	Write []string `json:"write"`
}

func TestConfigFieldsGolden(t *testing.T) {
	var write []string
	for _, k := range jsonKeys(reflect.TypeOf(configBody{})) {
		if k != "reason" {
			write = append(write, k)
		}
	}
	got := configFieldsGolden{View: jsonKeys(reflect.TypeOf(configJSON{})), Write: write}
	path := filepath.Join("..", "..", "..", "web", "src", "lib", "configFields.golden.json")

	if os.Getenv("UPDATE_GOLDEN") != "" {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with UPDATE_GOLDEN=1)", err)
	}
	var want configFieldsGolden
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the Config fields changed.\n got  %+v\n want %+v\nRegenerate with UPDATE_GOLDEN=1 go test ./internal/httpapi -run TestConfigFieldsGolden, then update web/src/lib/types.ts and CATALOG_CONFIG_KEYS in web/src/lib/configPayload.ts.", got, want)
	}
}

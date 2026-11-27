// SPDX-License-Identifier: Apache-2.0

package buildguard

import (
	"strings"
	"testing"
)

func TestCheckLaunchEnv(t *testing.T) {
	roots := []string{"/opt/radiance", "/opt/forge/rocm-therock-10.1"}
	ok := map[string]string{
		"RADIANCE_HOME":    "/opt/radiance/src/build/radiance_home",
		"RADIANCE_KERNELS": "libhalo",
		"ROCM_PATH":        "/opt/forge/rocm-therock-10.1",
		"LD_LIBRARY_PATH":  "/opt/forge/rocm-therock-10.1/lib:/opt/radiance/lib",
		"HSA_VERSION":      "11.5.1",
		"LEVEL":            "2",
	}
	if p := CheckLaunchEnv(ok, roots); len(p) != 0 {
		t.Fatalf("good environment refused: %v", p)
	}
	if p := CheckLaunchEnv(nil, roots); len(p) != 0 {
		t.Errorf("an empty environment is fine: %v", p)
	}

	long := strings.Repeat("x", MaxLaunchEnvValueLen+1)
	many := map[string]string{}
	for i := 0; i < MaxLaunchEnvEntries+1; i++ {
		many["V"+string(rune('A'+i%26))+string(rune('A'+i/26))] = "1"
	}
	big := map[string]string{}
	for i := 0; i < 6; i++ {
		big["V"+string(rune('A'+i))] = strings.Repeat("y", MaxLaunchEnvValueLen)
	}
	longName := "A" + strings.Repeat("B", MaxLaunchEnvNameLen)
	cases := []struct {
		name string
		env  map[string]string
		key  string // variable the problem is reported under ("" = the object)
		want string
	}{
		{"lowercase name", map[string]string{"home": "x"}, "home", "must match"},
		{"leading digit", map[string]string{"1A": "x"}, "1A", "must match"},
		{"hyphen", map[string]string{"A-B": "x"}, "A-B", "must match"},
		{"long name", map[string]string{longName: "x"}, longName, "longer"},
		{"secret name KEY", map[string]string{"OPENAI_API_KEY": "x"}, "OPENAI_API_KEY", "secret"},
		{"secret name TOKEN", map[string]string{"HF_TOKEN": "x"}, "HF_TOKEN", "secret"},
		{"secret name PASSWORD", map[string]string{"DB_PASSWORD": "x"}, "DB_PASSWORD", "secret"},
		{"FORGE_ prefix", map[string]string{"FORGE_X": "x"}, "FORGE_X", "stripped"},
		{"FOUNDRY_ prefix", map[string]string{"FOUNDRY_X": "x"}, "FOUNDRY_X", "stripped"},
		{"LD_PRELOAD", map[string]string{"LD_PRELOAD": "/opt/radiance/x.so"}, "LD_PRELOAD", "loader"},
		{"empty value", map[string]string{"A": ""}, "A", "empty"},
		{"long value", map[string]string{"A": long}, "A", "longer"},
		{"NUL", map[string]string{"A": "a\x00b"}, "A", "control"},
		{"newline", map[string]string{"A": "a\nb"}, "A", "control"},
		{"carriage return", map[string]string{"A": "a\rb"}, "A", "control"},
		{"credential prefix sk-", map[string]string{"A": "sk-abc"}, "A", "credential"},
		{"credential prefix hf_", map[string]string{"A": "hf_abc"}, "A", "credential"},
		{"bearer", map[string]string{"A": "Bearer abc"}, "A", "credential"},
		{"relative path", map[string]string{"A": "./lib"}, "A", "absolute"},
		{"home path", map[string]string{"A": "~/lib"}, "A", "absolute"},
		{"outside roots", map[string]string{"A": "/usr/lib"}, "A", "allowed root"},
		{"dotdot", map[string]string{"A": "/opt/radiance/../etc"}, "A", "clean"},
		{"trailing slash", map[string]string{"A": "/opt/radiance/"}, "A", "clean"},
		{"prefix is not a root", map[string]string{"A": "/opt/radiance-evil/lib"}, "A", "allowed root"},
		{"empty segment", map[string]string{"A": "/opt/radiance/lib::/opt/radiance/x"}, "A", "empty segment"},
		{"one bad segment", map[string]string{"A": "/opt/radiance/lib:/tmp/x"}, "A", "allowed root"},
		{"too many entries", many, "", "entries"},
		{"too many bytes", big, "", "in total"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := CheckLaunchEnv(tc.env, roots)
			if !strings.Contains(p[tc.key], tc.want) {
				t.Fatalf("problems = %v, want %q under %q", p, tc.want, tc.key)
			}
		})
	}
	// Without any root a path value is refused (fail closed); a non-path value still passes.
	if p := CheckLaunchEnv(map[string]string{"A": "/opt/radiance"}, nil); p["A"] == "" {
		t.Error("a path with no roots must be refused")
	}
	if p := CheckLaunchEnv(map[string]string{"A": "libhalo"}, nil); len(p) != 0 {
		t.Errorf("a non-path value needs no root: %v", p)
	}
}

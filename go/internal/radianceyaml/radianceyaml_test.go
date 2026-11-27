// SPDX-License-Identifier: Apache-2.0

package radianceyaml

import (
	"strings"
	"testing"
)

func TestFromArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "production shape",
			argv: []string{
				"--model", "/models/q.rad",
				"--served-model-name", "qwen36-35b-a3b",
				"--host", "127.0.0.1",
				"--port", "8080",
				"--max-model-len", "262144",
				"--max-num-seqs", "4",
				"--num-speculative-tokens", "2",
				"--kv-cache-dtype", "i8",
				"--tp", "1",
			},
			want: `server:
  host: 127.0.0.1
  port: 8080
models:
  qwen36-35b-a3b:
    model: /models/q.rad
    max-model-len: 262144
    max-num-seqs: 4
    num-speculative-tokens: 2
    kv-cache-dtype: i8
    tp: 1
`,
		},
		{
			// The alias contains a dot (allowed in a plain scalar) and a space
			// (not allowed), so both the path and the alias come out quoted.
			name: "model path with space and alias with dot",
			argv: []string{
				"--model", "/models/q wen.rad",
				"--served-model-name", "qwen3.6 35b",
				"--host", "127.0.0.1",
				"--port", "8080",
				"--max-model-len", "262144",
			},
			want: `server:
  host: 127.0.0.1
  port: 8080
models:
  "qwen3.6 35b":
    model: "/models/q wen.rad"
    max-model-len: 262144
`,
		},
		{
			// A dot alone is in the plain set, so it is not a reason to quote.
			name: "dotted alias stays plain",
			argv: []string{
				"--model", "/models/q.rad",
				"--served-model-name", "qwen3.6-35b",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
			},
			want: `server:
  host: h
  port: 1
models:
  qwen3.6-35b:
    model: /models/q.rad
    max-model-len: 8
`,
		},
		{
			name: "bare boolean in the middle and at the end",
			argv: []string{
				"--model", "/m.rad",
				"--served-model-name", "m",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
				"--flash",
				"--tp", "1",
				"--verbose",
			},
			want: `server:
  host: h
  port: 1
models:
  m:
    model: /m.rad
    max-model-len: 8
    flash: true
    tp: 1
    verbose: true
`,
		},
		{
			name: "negative integer value",
			argv: []string{
				"--model", "/m",
				"--served-model-name", "m",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
				"--seed", "-1",
			},
			want: `server:
  host: h
  port: 1
models:
  m:
    model: /m
    max-model-len: 8
    seed: -1
`,
		},
		{
			name: "reserved words as values versus a bare flag",
			argv: []string{
				"--model", "/m",
				"--served-model-name", "m",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
				"--a", "true",
				"--b", "no",
				"--c", "null",
				"--d",
			},
			want: `server:
  host: h
  port: 1
models:
  m:
    model: /m
    max-model-len: 8
    a: "true"
    b: "no"
    c: "null"
    d: true
`,
		},
		{
			name: "reserved alias is quoted",
			argv: []string{
				"--model", "/m",
				"--served-model-name", "yes",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
			},
			want: `server:
  host: h
  port: 1
models:
  "yes":
    model: /m
    max-model-len: 8
`,
		},
		{
			name: "structure-bearing values are quoted",
			argv: []string{
				"--model", "/m",
				"--served-model-name", "m",
				"--host", "h",
				"--port", "1",
				"--max-model-len", "8",
				"--hash", "a#b",
				"--sep", "a: b",
				"--star", "*x",
				"--amp", "&x",
			},
			want: `server:
  host: h
  port: 1
models:
  m:
    model: /m
    max-model-len: 8
    hash: "a#b"
    sep: "a: b"
    star: "*x"
    amp: "&x"
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromArgv(tc.argv)
			if err != nil {
				t.Fatalf("FromArgv(%v): %v", tc.argv, err)
			}
			if got != tc.want {
				t.Errorf("FromArgv(%v) =\n%s\nwant:\n%s", tc.argv, got, tc.want)
			}
		})
	}
}

func TestFromArgvErrors(t *testing.T) {
	base := []string{
		"--model", "/m",
		"--served-model-name", "m",
		"--host", "h",
		"--port", "1",
		"--max-model-len", "8",
	}

	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"empty argv", nil, "empty argv"},
		{
			"missing model",
			[]string{"--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"missing served-model-name",
			[]string{"--model", "/m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--served-model-name",
		},
		{
			"missing host",
			[]string{"--model", "/m", "--served-model-name", "m", "--port", "1", "--max-model-len", "8"},
			"--host",
		},
		{
			"missing port",
			[]string{"--model", "/m", "--served-model-name", "m", "--host", "h", "--max-model-len", "8"},
			"--port",
		},
		{
			"missing max-model-len",
			[]string{"--model", "/m", "--served-model-name", "m", "--host", "h", "--port", "1"},
			"--max-model-len",
		},
		{
			"required flag without a value",
			[]string{"--model", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"duplicate flag",
			append(append([]string{}, base...), "--port", "2"),
			"--port",
		},
		{
			"stray positional at the start",
			append([]string{"stray"}, base...),
			"stray",
		},
		{
			"stray positional after a value",
			append(append([]string{}, base...), "loose"),
			"loose",
		},
		{
			"--flag=value spelling",
			[]string{"--model=/m", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"invalid flag name (uppercase)",
			append(append([]string{}, base...), "--Bad", "x"),
			"--Bad",
		},
		{
			"invalid flag name (underscore)",
			append(append([]string{}, base...), "--foo_bar", "x"),
			"--foo_bar",
		},
		{
			"empty value",
			[]string{"--model", "", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"newline in value",
			[]string{"--model", "/m\nx", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"carriage return in value",
			[]string{"--model", "/m\rx", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"NUL in value",
			[]string{"--model", "/m\x00x", "--served-model-name", "m", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--model",
		},
		{
			"newline in alias",
			[]string{"--model", "/m", "--served-model-name", "a\nb", "--host", "h", "--port", "1", "--max-model-len", "8"},
			"--served-model-name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromArgv(tc.argv)
			if err == nil {
				t.Fatalf("FromArgv(%v) = %q, want an error", tc.argv, got)
			}
			if got != "" {
				t.Errorf("FromArgv(%v) returned partial output %q alongside its error", tc.argv, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestFromArgvIsDeterministic(t *testing.T) {
	argv := []string{
		"--model", "/models/q.rad",
		"--served-model-name", "qwen36-35b-a3b",
		"--host", "127.0.0.1",
		"--port", "8080",
		"--max-model-len", "262144",
		"--flash",
		"--tp", "1",
	}
	first, err := FromArgv(argv)
	if err != nil {
		t.Fatalf("FromArgv: %v", err)
	}
	second, err := FromArgv(argv)
	if err != nil {
		t.Fatalf("FromArgv: %v", err)
	}
	if first != second {
		t.Errorf("the same argv produced different output:\n%s\n---\n%s", first, second)
	}
	if first == "" {
		t.Error("output is empty")
	}
}

// A plain scalar may not start with the YAML indicators "@" or "%", and a ":" inside one is the mapping indicator: all three are quoted.
func TestFromArgvQuotesIndicatorScalars(t *testing.T) {
	base := []string{"--model", "/m.rad", "--served-model-name", "a", "--host", "127.0.0.1", "--port", "1", "--max-model-len", "2"}
	for _, tc := range []struct{ value, want string }{
		{"@x", `"@x"`},
		{"%x", `"%x"`},
		{"a:b", `"a:b"`},
		{"foo:", `"foo:"`},
		{"x@y", "x@y"}, // an "@" inside is fine
		{"50%", "50%"}, // so is a "%" inside
		{"i8", "i8"},   // the ordinary cases stay plain
		{"-1", "-1"},
	} {
		got, err := FromArgv(append(append([]string{}, base...), "--probe", tc.value))
		if err != nil {
			t.Fatalf("%q: %v", tc.value, err)
		}
		if line := "    probe: " + tc.want + "\n"; !strings.HasSuffix(got, line) {
			t.Errorf("value %q: want the last line %q, got:\n%s", tc.value, line, got)
		}
	}
}

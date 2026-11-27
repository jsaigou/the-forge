// SPDX-License-Identifier: Apache-2.0

// Package radianceyaml translates the Radiance flag vector Forge stores into the
// YAML file Radiance 2.x reads.
//
// Radiance 1.17 accepted command-line flags; 2.x refuses them and only reads a
// config file. Forge keeps storing the flag form so its drift and fit checks
// (which compare the whole flag set) stay valid, and translates at launch. This
// package is that translation and nothing else: no I/O, no discovery, no
// defaults. A value that cannot be written unambiguously is an error rather than
// a guess, because a YAML file that silently changes meaning is worse than a
// failed launch.
package radianceyaml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// flagForm is the only spelling Radiance understands. "=" forms, uppercase and
// underscores are refused rather than normalised: Radiance matches names
// exactly, so translating a name Forge invented would hide a config mistake.
var flagForm = regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)

// plainForm is the YAML plain-scalar subset that needs no quoting: the safe
// punctuation plus every alphanumeric. Anything outside it is quoted so a value
// can never introduce a mapping, sequence, comment or anchor.
var plainForm = regexp.MustCompile(`^[A-Za-z0-9_./:+@%-]+$`)

// plainInt matches a value that is a plain integer, which is the only case
// where a leading "-" does not make a plain scalar look like a list item.
var plainInt = regexp.MustCompile(`^-?[0-9]+$`)

var requiredFlags = []string{
	"--model",
	"--served-model-name",
	"--host",
	"--port",
	"--max-model-len",
}

// requiredSet lets the extra flags be emitted in their original order without
// re-checking the list for each one.
var requiredSet = func() map[string]bool {
	m := make(map[string]bool, len(requiredFlags))
	for _, f := range requiredFlags {
		m[f] = true
	}
	return m
}()

type parsedFlag struct {
	name  string
	value string
	bare  bool
}

// FromArgv converts the argument vector Forge stores for a Radiance engine (the
// flag form) into the YAML text `radiance --config FILE` reads. argv starts at
// the first flag; the binary name is not included.
func FromArgv(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("radianceyaml: empty argv")
	}

	flags := make([]parsedFlag, 0, len(argv)/2)
	seen := make(map[string]bool, len(argv)/2)
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "" {
			return "", fmt.Errorf("radianceyaml: empty token at position %d where a flag was expected", i)
		}
		if !strings.HasPrefix(tok, "--") {
			return "", fmt.Errorf("radianceyaml: token %q at position %d is not a flag", tok, i)
		}
		if name, _, ok := strings.Cut(tok, "="); ok {
			return "", fmt.Errorf("radianceyaml: %q uses the --flag=value spelling; Radiance matches flag names exactly, give the flag and its value as two arguments", name)
		}
		if !flagForm.MatchString(tok) {
			return "", fmt.Errorf("radianceyaml: flag %q does not match the Radiance flag form ^--[a-z][a-z0-9-]*$", tok)
		}
		if seen[tok] {
			return "", fmt.Errorf("radianceyaml: flag %s is given more than once", tok)
		}
		seen[tok] = true

		p := parsedFlag{name: tok}
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			v := argv[i+1]
			if v == "" {
				return "", fmt.Errorf("radianceyaml: flag %s has an empty value", tok)
			}
			if strings.ContainsAny(v, "\x00\r\n") {
				return "", fmt.Errorf("radianceyaml: flag %s value %q contains a NUL, carriage return or newline", tok, v)
			}
			p.value = v
			i++
		} else {
			p.bare = true
		}
		flags = append(flags, p)
	}

	byName := make(map[string]parsedFlag, len(requiredFlags))
	for _, f := range flags {
		if requiredSet[f.name] {
			byName[f.name] = f
		}
	}
	for _, want := range requiredFlags {
		f, ok := byName[want]
		if !ok {
			return "", fmt.Errorf("radianceyaml: missing required flag %s", want)
		}
		if f.bare {
			return "", fmt.Errorf("radianceyaml: required flag %s needs a value", want)
		}
	}

	var b strings.Builder
	b.WriteString("server:\n")
	if err := writeEntry(&b, "  ", "host", byName["--host"].value, false); err != nil {
		return "", err
	}
	if err := writeEntry(&b, "  ", "port", byName["--port"].value, false); err != nil {
		return "", err
	}
	b.WriteString("models:\n")

	alias, err := yamlScalar(byName["--served-model-name"].value)
	if err != nil {
		return "", fmt.Errorf("radianceyaml: model alias: %w", err)
	}
	b.WriteString("  " + alias + ":\n")
	if err := writeEntry(&b, "    ", "model", byName["--model"].value, false); err != nil {
		return "", err
	}
	if err := writeEntry(&b, "    ", "max-model-len", byName["--max-model-len"].value, false); err != nil {
		return "", err
	}
	for _, f := range flags {
		if requiredSet[f.name] {
			continue
		}
		key := strings.TrimPrefix(f.name, "--")
		if err := writeEntry(&b, "    ", key, f.value, f.bare); err != nil {
			return "", fmt.Errorf("radianceyaml: flag %s: %w", f.name, err)
		}
	}
	return b.String(), nil
}

// writeEntry emits one "key: value" line. bare writes the literal boolean true,
// which must stay plain: quoting it would turn it into the string "true".
func writeEntry(b *strings.Builder, indent, key, value string, bare bool) error {
	if bare {
		b.WriteString(indent + key + ": true\n")
		return nil
	}
	scalar, err := yamlScalar(value)
	if err != nil {
		return err
	}
	b.WriteString(indent + key + ": " + scalar + "\n")
	return nil
}

// yamlScalar writes a scalar plain only when YAML would read it back as the same
// string. Everything else is a double-quoted JSON string literal, whose escapes
// are a subset of YAML's and therefore valid there.
func yamlScalar(s string) (string, error) {
	if isPlainScalar(s) {
		return s, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", fmt.Errorf("encode %q as a YAML double-quoted scalar: %w", s, err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func isPlainScalar(s string) bool {
	if !plainForm.MatchString(s) {
		return false
	}
	// "@" and "%" are YAML indicators that may not start a plain scalar, and a ":" inside one (or ending one) is the mapping indicator that a strict
	// reader may refuse or misread. Quoting is always valid, so these are quoted rather than guessed at.
	if strings.ContainsRune(s, ':') || strings.HasPrefix(s, "@") || strings.HasPrefix(s, "%") {
		return false
	}
	if strings.HasPrefix(s, "-") && !plainInt.MatchString(s) {
		return false
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", "y", "n":
		return false
	}
	return true
}

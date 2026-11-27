// SPDX-License-Identifier: Apache-2.0

// Package duplexcard renders the Haru character card to the text the duplex
// engine expects. The engine accepts either the client's structured card or a
// plain string; this package implements the string form, byte-for-byte
// compatible with the Haru reference's render_card (commit 7fe6b76).
//
// Unlike Python's str(value or ""), which silently stringifies numbers and
// booleans, Render accepts only strings and nil. Any other type is rejected
// with an error naming the field. This is deliberate: the card comes from an
// untrusted client, and rendering float64(1) as "1" or bool as "True" would
// bake Python's coercion rules into the wire.
//
// Because the card is untrusted, Render also validates it. Invalid UTF-8 and
// control characters other than newline and tab are refused, as is any field
// over MaxFieldBytes and the seven fields together over MaxTotalBytes. Lengths
// are in bytes, before leading and trailing whitespace is trimmed.
package duplexcard

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on a card accepted from a client. Per-field applies to each of the
// seven fields as supplied (before whitespace trimming); total applies to their
// byte sum. The language selector is not part of that budget: it must match one
// of the nine supported codes.
const (
	MaxFieldBytes = 2000
	MaxTotalBytes = 8000
)

// Errors returned by Render. Use errors.Is to test them; the wrapping message
// names the offending field or language without echoing its content.
var (
	ErrUnsupportedLanguage = errors.New("duplexcard: unsupported language")
	ErrFieldType           = errors.New("duplexcard: field is not a string")
	ErrInvalidUTF8         = errors.New("duplexcard: field is not valid UTF-8")
	ErrControlCharacter    = errors.New("duplexcard: field contains a control character")
	ErrFieldTooLong        = errors.New("duplexcard: field is too long")
	ErrCardTooLong         = errors.New("duplexcard: card is too long")
)

// cardFields is the reference's CARD_FIELDS order.
var cardFields = [...]string{
	"persona", "name", "gender", "backchannel", "other_name", "relationship", "situation",
}

//go:embed card_templates.json
var cardTemplatesJSON []byte

type templateFile struct {
	Templates map[string]template `json:"templates"`
}

type template struct {
	Lead        string            `json:"lead"`
	Headers     map[string]string `json:"headers"`
	Gender      map[string]string `json:"gender"`
	Backchannel map[string]string `json:"backchannel"`
}

var cardTemplates = mustLoadTemplates()

func mustLoadTemplates() templateFile {
	var f templateFile
	if err := json.Unmarshal(cardTemplatesJSON, &f); err != nil {
		panic("duplexcard: embedded card_templates.json: " + err.Error())
	}
	if len(f.Templates) == 0 {
		panic("duplexcard: embedded card_templates.json has no templates")
	}
	return f
}

// Render turns a structured Haru card into the text form of the card for the
// language in card["language"] (default "en"). It joins the lead sentence and
// each non-empty field's header and value with newlines, exactly like the
// reference's render_card. gender and backchannel are translated through the
// language's option table; an unknown or empty option is omitted.
func Render(card map[string]any) (string, error) {
	tpl, err := selectTemplate(card)
	if err != nil {
		return "", err
	}
	values, err := cardValues(card)
	if err != nil {
		return "", err
	}

	lines := make([]string, 0, 1+2*len(cardFields))
	lines = append(lines, tpl.Lead)
	for _, field := range cardFields {
		value := values[field]
		switch field {
		case "gender":
			value = tpl.Gender[value]
		case "backchannel":
			value = tpl.Backchannel[value]
		}
		if value == "" {
			continue
		}
		lines = append(lines, tpl.Headers[field], value)
	}
	return strings.Join(lines, "\n"), nil
}

func selectTemplate(card map[string]any) (template, error) {
	lang := "en"
	if raw, ok := card["language"]; ok && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return template{}, fmt.Errorf("%w: language has type %T", ErrFieldType, raw)
		}
		if s = strings.TrimSpace(s); s != "" {
			lang = s
		}
	}
	tpl, ok := cardTemplates.Templates[lang]
	if !ok {
		return template{}, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, lang)
	}
	return tpl, nil
}

func cardValues(card map[string]any) (map[string]string, error) {
	values := make(map[string]string, len(cardFields))
	total := 0
	for _, field := range cardFields {
		raw, ok := card[field]
		if !ok || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s has type %T", ErrFieldType, field, raw)
		}
		if len(s) > MaxFieldBytes {
			return nil, fmt.Errorf("%w: field %q is %d bytes; the limit is %d", ErrFieldTooLong, field, len(s), MaxFieldBytes)
		}
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("%w: field %q", ErrInvalidUTF8, field)
		}
		for _, r := range s {
			if r == '\n' || r == '\t' {
				continue
			}
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("%w: field %q contains %U; only newline and tab are allowed", ErrControlCharacter, field, r)
			}
		}
		total += len(s)
		values[field] = strings.TrimSpace(s)
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("%w: card fields total %d bytes; the limit is %d", ErrCardTooLong, total, MaxTotalBytes)
	}
	return values, nil
}

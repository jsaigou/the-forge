// SPDX-License-Identifier: Apache-2.0

package style

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Use is one tag as written in the text.
type Use struct {
	Tag      *Tag
	Name     string  // as written
	Strength float64 // as written; 1 when absent
	// Start and End are code-point offsets into Parsed.Spoken: the range the tag covers. A line-scope tag has no range.
	Start, End int
	Open       bool // never closed: it ran to the end of the text
}

// Parsed is a text with its tags taken out.
type Parsed struct {
	Spoken   string
	Uses     []Use
	Warnings []string
}

// HasTags reports whether the text contained any tag.
func (p *Parsed) HasTags() bool { return len(p.Uses) > 0 }

var tagBody = regexp.MustCompile(`^(/?)([A-Za-z][A-Za-z0-9_-]*)?(?::([+-]?[0-9]*\.?[0-9]+))?$`)

// maxTagLen bounds how far a "<" looks for its ">": a tag is short, and a stray "<" in prose must not swallow a paragraph.
const maxTagLen = 40

// Parse takes the tags out of text. Only names in the vocabulary are tags: <b>, a < b and [1] are ordinary text, and \< writes a literal "<". Tags never reach
// the spoken text. It never fails: a malformed tag is reported in Warnings and its text is kept or dropped as described there.
func Parse(text string, v *Vocabulary) *Parsed {
	p := &Parsed{}
	in := []rune(text)
	out := make([]rune, 0, len(in))
	var stack []int // indexes into p.Uses of the open span tags
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '\\' && i+1 < len(in) && in[i+1] == '<' {
			out = append(out, '<')
			i++
			continue
		}
		if c != '<' {
			out = append(out, c)
			continue
		}
		end := -1
		for j := i + 1; j < len(in) && j <= i+maxTagLen; j++ {
			if in[j] == '>' {
				end = j
				break
			}
			if in[j] == '<' {
				break
			}
		}
		if end < 0 {
			out = append(out, c)
			continue
		}
		body := string(in[i+1 : end])
		m := tagBody.FindStringSubmatch(body)
		if m == nil {
			out = append(out, c)
			continue
		}
		closing, name, strength := m[1] == "/", m[2], m[3]
		switch {
		case closing && name == "" && strength == "": // </>
			if len(stack) == 0 {
				p.warn("</> closes nothing")
			} else {
				p.Uses[stack[len(stack)-1]].End = len(out)
				stack = stack[:len(stack)-1]
			}
		case name == "":
			out = append(out, c)
			continue
		default:
			tag, ok := v.Lookup(name)
			if !ok {
				out = append(out, c) // not ours: literal text
				continue
			}
			if closing {
				if strength != "" {
					p.warn("</%s:...>: a closing tag has no strength", name)
				}
				k := -1
				for s := len(stack) - 1; s >= 0; s-- {
					if p.Uses[stack[s]].Tag == tag {
						k = s
						break
					}
				}
				if k < 0 {
					if tag.Scope != "line" {
						p.warn("</%s> closes nothing", name)
					}
					break
				}
				for s := len(stack) - 1; s > k; s-- { // tags opened inside and left open: closed with their parent
					p.Uses[stack[s]].End = len(out)
					p.warn("<%s> was still open at </%s> and was closed there", p.Uses[stack[s]].Name, name)
				}
				p.Uses[stack[k]].End = len(out)
				stack = stack[:k]
				break
			}
			s := 1.0
			if strength != "" {
				f, err := strconv.ParseFloat(strength, 64)
				if err != nil {
					p.warn("<%s:%s>: %q is not a number; strength 1 used", name, strength, strength)
				} else {
					s = f
				}
			}
			if tag.Scope == "line" {
				p.Uses = append(p.Uses, Use{Tag: tag, Name: name, Strength: s, Start: -1, End: -1})
				break
			}
			p.Uses = append(p.Uses, Use{Tag: tag, Name: name, Strength: s, Start: len(out), End: -1})
			stack = append(stack, len(p.Uses)-1)
		}
		i = end
	}
	for _, k := range stack {
		p.Uses[k].End = len(out)
		p.Uses[k].Open = true
		p.warn("<%s> is never closed: it runs to the end of the text", p.Uses[k].Name)
	}
	// A span that covers nothing does nothing.
	kept := p.Uses[:0]
	for _, u := range p.Uses {
		if u.Tag.Scope == "span" && u.End <= u.Start {
			p.warn("<%s></%s> covers no text and was dropped", u.Name, u.Name)
			continue
		}
		kept = append(kept, u)
	}
	p.Uses = kept
	p.Spoken = string(out)
	return p
}

func (p *Parsed) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// Strip removes every tag from text and returns what is left, for engines that cannot steer.
func Strip(text string, v *Vocabulary) string { return Parse(text, v).Spoken }

// LooksTagged is a cheap test for whether text can contain a tag at all.
func LooksTagged(text string) bool { return strings.ContainsRune(text, '<') }

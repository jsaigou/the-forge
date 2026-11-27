// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"errors"
	"regexp"
)

// ErrInvalidRepo is returned for a repo id or revision that is not a plain HuggingFace identifier.
var ErrInvalidRepo = errors.New("hf: not a valid repository id or revision")

var (
	repoRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidRepo reports whether s is an "owner/name" repo id in HuggingFace's charset. Repo ids are concatenated into request paths and, by smith's
// runbooks, into operator-run shell commands; a value that fails here (query strings, "..", whitespace, quotes, ';', '$', newlines) never
// reaches either.
func ValidRepo(s string) bool { return repoRE.MatchString(s) }

// ValidRevision reports whether s is a branch, tag or commit name without a path separator, so it cannot rewrite the request path or query.
func ValidRevision(s string) bool { return revisionRE.MatchString(s) && !hasDoubleDot(s) }

func hasDoubleDot(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '.' && s[i+1] == '.' {
			return true
		}
	}
	return false
}

// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http/httptest"
	"testing"
)

// ?force=false (or no/off/0 or junk) must not force-delete a loaded config; a bare ?force and yes/on/true/1 still do, as before.
func TestQueryFlagRequiresATruthyValue(t *testing.T) {
	for q, want := range map[string]bool{"": false, "?force": true, "?force=": true, "?force=false": false, "?force=0": false, "?force=no": false, "?force=OFF": false, "?force=junk": false,
		"?force=true": true, "?force=1": true, "?force=TRUE": true, "?force=yes": true, "?force=on": true, "?other=1": false} {
		if got := queryFlag(httptest.NewRequest("DELETE", "/x"+q, nil), "force"); got != want {
			t.Errorf("queryFlag(%q) = %v, want %v", q, got, want)
		}
	}
}

// SPDX-License-Identifier: Apache-2.0

package authz

import "testing"

// Every declared resource must have a default, so the evaluator's fallback for an unknown key is never reached by real routes.
func TestEveryResourceHasADefaultPolicy(t *testing.T) {
	for _, r := range allResources {
		if _, ok := DefaultPolicy[r]; !ok {
			t.Errorf("resource %q has no entry in DefaultPolicy", r)
		}
	}
	for k := range DefaultPolicy {
		if !ValidResource(k) {
			t.Errorf("DefaultPolicy names %q, which is not in allResources", k)
		}
	}
}

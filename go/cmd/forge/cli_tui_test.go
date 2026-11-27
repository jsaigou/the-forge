// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestSplitNoWait(t *testing.T) {
	for _, c := range []struct {
		in       []string
		wantRest []string
		wantWait bool
	}{
		{[]string{"qwen36", "a1"}, []string{"qwen36", "a1"}, true},
		{[]string{"qwen36", "--no-wait"}, []string{"qwen36"}, false},
		{[]string{"--no-wait", "qwen36", "a1"}, []string{"qwen36", "a1"}, false},
		{[]string{"qwen36", "a1", "--no-wait"}, []string{"qwen36", "a1"}, false},
		{nil, nil, true},
	} {
		rest, wait := splitNoWait(c.in)
		if !reflect.DeepEqual(rest, c.wantRest) || wait != c.wantWait {
			t.Errorf("splitNoWait(%v) = %v, %v; want %v, %v", c.in, rest, wait, c.wantRest, c.wantWait)
		}
	}
}

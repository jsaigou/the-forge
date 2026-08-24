// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"log"
	"runtime/debug"
)

// goSafe runs fn in a new goroutine, recovering from panics so a bug in a
// background sampler/heartbeat cannot crash the daemon. The recovered value
// and stack trace are logged.
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("httpapi: goroutine %q panic: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

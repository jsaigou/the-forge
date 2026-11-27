// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// slotLaunchLister is the read half of the slot_launches API.
type slotLaunchLister interface {
	List(ctx context.Context) ([]store.SlotLaunch, error)
}

// slotKindTTL bounds how stale the port-to-kind map may be. A load rewrites the slot's row before the engine starts, and the router only
// consults the kind for a slot it has just found healthy, so a few seconds of lag cannot pair a request with the previous engine's dialect for
// longer than the unhealthy window that a kind change always passes through.
const slotKindTTL = 3 * time.Second

// slotKindLookup returns router.Deps.SlotKind: the engine kind the engine manager last launched on a raw slot port (the slot_launches row's
// engine_kind), cached for slotKindTTL. A port with no row, or a read failure, returns "" which the router reads as llama.cpp; a failure is
// logged once per cache window, not swallowed.
func slotKindLookup(launches slotLaunchLister, now func() time.Time) func(port int) string {
	var (
		mu      sync.Mutex
		expires time.Time
		kinds   map[int]string
	)
	return func(port int) string {
		mu.Lock()
		defer mu.Unlock()
		if now().After(expires) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			rows, err := launches.List(ctx)
			cancel()
			kinds = map[int]string{}
			if err != nil {
				log.Printf("forge: read slot launch kinds for the router: %v; slots read as llama.cpp until the next refresh", err)
			}
			for _, r := range rows {
				kinds[r.Port] = r.EngineKind
			}
			expires = now().Add(slotKindTTL)
		}
		return kinds[port]
	}
}

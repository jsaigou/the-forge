ref: compressor-topology:compressor-key-seams
doc: compressor-topology
slug: compressor-key-seams
title: 6. Key seams (files touched)
category: compressor
source: docs/v5-headroom-topology.md

## 6. Key seams (files touched)

- **`internal/router/routing.go`** — `resolveBackend` (`:87`) gains a case that sets a new
  `ResolvedBackend.UpstreamOverride` field for local-Headroom-fronted Configs; `catalogChain`
  (`:321`) emits that kind of backend instead of a bare `foundry_slot` one when local Headroom
  fronting is enabled and not bypassed (`headroomBypassed`, `:140`, unchanged). Delete the dead
  `"foundry_slot_proxied"` case (`:97`) and its now-only-consumers
  (`headroomServiceForPort`/`slotForPort`) once local fronting lands on the new mechanism —
  they were built for address-pinned proxies, which this design does not use.
- **`internal/router/proxy.go`** — the per-request `httputil.ReverseProxy.Rewrite` hook (`:261`)
  gains the strip-then-set pair from §5b: unconditionally `pr.Out.Header.Del("x-headroom-base-url")`
  on every branch, then `pr.Out.Header.Set(...)` to `resolved.UpstreamOverride` only when non-empty
  and server-derived. This is the entire on-the-wire mechanism from §3/§4 — no new proxy code, no
  new indirection service.
- **`internal/store/store.go`** `ProxyRow` (`:241`) — already has everything needed
  (Service/Port/TargetURL/Unit/Provider/Passthrough/OrphanedAt/CreatedAt). Local vs. provider scope
  can be inferred from `Provider == ""` rather than a new column. Seed data: drop the `a1`/`a2`
  rows, add one `headroom-local` row; provider rows unchanged in shape, just provisioned/orphaned
  dynamically instead of hand-inserted.
- **`internal/httpapi/headroom_handlers.go`** `handleHeadroomLifecycle` (`:183`) — currently a
  501 stub; this is where §5's chosen mechanism gets implemented.
- **`internal/httpapi/settings_handlers.go`** `SaveProvider` (`:226`/`:295`/`:339`) — the natural
  hook point for "enable/link a provider ⇒ provision its proxy; disable/unlink ⇒ tear down."
- **Metrics — no structural change needed.** `collector/run.go`'s `recordHeadroomSavings` (`:597`)
  and `main.go`'s `HeadroomTargets()` closure (`:210`) already derive the scraped port set from
  `store.Headroom.Proxies()` — they'll pick up `headroom-local` and any provisioned provider proxy
  automatically. The local savings figure becomes one aggregate line (today it's two *dead* a1/a2
  lines that never accrue anything, since nothing routes through them).

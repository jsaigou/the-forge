# Routing resolves by Config/Offering identity, not physical address

a0's routing table conflated two different addressing schemes under one `[[router.routes]]
model` string: `a1`/`a2` were slot aliases (`forge_slot`/`forge_slot_proxied` — "whatever
is loaded on this physical bay right now", predating a0's on-demand scheduler), while `a3`/`a4`
(added by the a0-catalog-visibility fix, `59af7ec`) already resolved by catalog Config name via
unpinned `scheduler.EnsureLoaded`.

`a1`/`a2` were never meant to be requested literally — the V4 design's actual client contract
(`~/Documents/vault/Forge/a0-connection-guide.md`) was "request by real model name, a
push-sync hook rewrites the route's `model` field to match whatever's actually loaded so that
name always resolves correctly." The slot name was only ever an internal, stable backend
identifier the sync hook rewrote *by*. **v0.5 never rebuilt that sync hook** —
`sched.Deps.RouteSync` is deliberately nil (`59af7ec`'s commit message flagged this: "route
labels are static strings from router.toml... labels go stale") — so in v0.5 specifically, the
static `model` field on these routes is frozen at whatever it was last set to and silently
drifts from reality. **Confirmed live on ForgeHost, 2026-07-28:** the `a1` route reads
`model = 'nemotron-nano-omni'` while slot a1 actually has `Qwen3.6-35B-A3B-MTP` loaded — a real
request for `nemotron-nano-omni` today would silently be served by a different model with no
error, the exact worst-case failure mode `a0-router.md`'s own design history calls out
("a stale alias-keyed route would silently serve the *wrong* model under the name the user
picked, with no error"). This is not a hypothetical or dead-legacy concern — it is a live,
currently-active correctness bug, independent of and predating the TOML-decommission work.

Given that, retiring `a1`/`a2` static routes isn't just consistent with `CONTEXT.md`'s `Slot`
entry ("a0's router handles presentation so callers don't know or care which Slot a Config
landed on") — it removes a route class that cannot currently keep its own contract. **All local
routing resolves by Config name**, uniformly, matching the `a3`/`a4` pattern — which needs no
sync hook at all, since it resolves fresh against the catalog on every request instead of
trusting a field that has to be actively kept in sync.

The same question came up designing the toml-decommission schema for remote routing: a draft
`routing_backends`/`routing_routes` table pair would have duplicated data `Offering` already
carries (`wire_model`, `Provider`) and duplicated resolution logic `router_providers`/
`headroom_proxies` already implement (credential + Headroom-proxy lookup is already
store-backed — only the router.toml backend/route *declaration* was file-only). ADR-0003
already established Offering as "the remote equivalent of a Config... both are route targets
for a0"; the draft table would have quietly built a second, parallel remote-routing mechanism
in violation of that. **Remote routing resolves by Offering identity** instead, reusing the
existing Provider/Headroom-proxy resolution path.

Net effect: physical addressing (slot number, static backend name) is no longer a valid way to
reach a model through a0 — every route is identity-shaped (Config locally, Offering remotely),
consistent with ADR-0002 (Config as the loading unit) and ADR-0003 (Offering as its remote
counterpart).

## Considered Options

- **Config/Offering identity for all routing (chosen):** one addressing scheme, no duplicate
  schema, consistent with existing ADRs, and removes a route class with a confirmed-live
  correctness bug. Before deleting the `a1`/`a2` routes, usage/access logs should still be
  checked for real traffic against the stale names — if something out there is actually
  requesting `nemotron-nano-omni` right now, it's currently getting silently wrong answers and
  that's a more urgent problem than the migration itself.
- **Keep `a1`/`a2` as a permanent slot-alias routing mode alongside Config-name routing
  (rejected):** two addressing paradigms for the same router, no longer justified now that
  operators pick models via a0's own listing rather than a fixed "primary slot" habit.
- **New `routing_backends`/`routing_routes` tables mirroring router.toml's shape 1:1
  (rejected):** would re-derive data and resolution logic that `Offering`/`router_providers`/
  `headroom_proxies` already own, contradicting ADR-0003.

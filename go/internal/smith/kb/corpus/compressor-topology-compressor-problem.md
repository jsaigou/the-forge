ref: compressor-topology:compressor-problem
doc: compressor-topology
slug: compressor-problem
title: 1. Problem
category: compressor
source: docs/v5-headroom-topology.md

## 1. Problem

*(Historical — describes the state that motivated this design, 2026-07-28. As of 2026-07-29 this
is fully resolved; see the status block above for the current topology.)*

Today's Headroom proxies are fixed, statically-named systemd units, confirmed live on this deployment
2026-07-28: `headroom-a1`@8788, `headroom-a2`@8789 (local slots), `headroom-deepseek`@8790,
`headroom-external`@8791 (fronts the `aiand` provider, which itself serves both `kimi-k2.7-code`
and `glm-5.2` behind one upstream URL). Two problems, not one:

1. **Local fronting is dead code.** Since ADR-0007 (`docs/adr/0007-routing-resolves-by-identity-not-address.md`),
   `catalogChain` — the only local routing path — never routes through a1/a2 at all. Binding a
   proxy to a physical slot port was tried and reverted: it reintroduces address-based pinning,
   which a0's placement model is deliberately free of. `internal/router/routing.go`'s
   `resolveBackend`'s `"foundry_slot_proxied"` case is fully dead (nothing ever constructs that
   Backend kind); the a1/a2 units still run on the deployment host, unused.
2. **Nothing is lifecycle-managed.** The whole proxy set is hand-created, by-hand systemd units.
   There's no provisioning/teardown tied to what's actually loaded (local) or which providers are
   enabled (remote) — you'd add a provider and forget the proxy exists, or leave a proxy running
   for a provider that was disabled months ago.

§11's target state: proxies scoped to *actively-loaded Configs* (up to 4 local + one per external
provider), provisioned/torn-down as load state changes. §11 explicitly deferred designing this,
gated on an investigation described next.

# Routing resolves by Config/Offering identity, not physical address

a0's routing used to conflate two different addressing schemes under one route's `model`
string: some routes were slot aliases - "whatever is loaded on this physical bay right now" -
while others already resolved by catalog Config name via the scheduler's on-demand load path,
unpinned to any specific slot.

A slot-alias route depends on something keeping its `model` label in sync with whatever is
actually loaded on that slot whenever a mode switch happens. Without an active sync mechanism
keeping that label current, a slot-alias route's label silently drifts from reality: a request
for the name the route was last synced to would be served by whatever different model is
actually loaded now, with no error - the exact failure mode this class of routing is supposed
to avoid.

## Decision

All local routing resolves by Config name, uniformly - the same pattern the non-aliased routes
already used. This needs no sync mechanism at all, since it resolves fresh against the live
catalog on every request instead of trusting a label that has to be actively kept in sync.

The same reasoning applies to remote routing: rather than a separate backend/route table
duplicating data an Offering already carries (wire model name, provider) and duplicating
resolution logic the provider/credential store already implements, remote routing resolves by
Offering identity, reusing the existing provider resolution path.

Net effect: physical addressing (a slot number, a static backend name) is no longer a valid way
to reach a model through a0 - every route is identity-shaped (a Config locally, an Offering
remotely).

## Considered Options

- **Config/Offering identity for all routing (chosen):** one addressing scheme, no duplicate
  schema, and removes a route class with a real correctness gap.
- **Keep slot-alias routing as a permanent mode alongside Config-name routing (rejected):** two
  addressing paradigms for the same router, no longer justified once a model is always
  discoverable by its own catalog name.
- **A parallel backend/route table mirroring the alias shape 1:1 (rejected):** would re-derive
  data and resolution logic the catalog/provider store already owns.

ref: compressor-topology:compressor-ssrf-hardening
doc: compressor-topology
slug: compressor-ssrf-hardening
title: Never forward a client-supplied upstream-override header verbatim
category: network
source: docs/llm-router.md

If a router accepts a per-request header telling it which backend URL to proxy to (useful for
routing to whichever slot a scheduler just resolved), that header must never be allowed to pass
through from an inbound client request unmodified - otherwise any caller can redirect the
proxy's outbound request anywhere it wants, a classic SSRF via an unsanitized upstream-target
header on a reverse proxy.

**Rule:** unconditionally delete any inbound instance of that header from the outbound request
first, on every code path - not just the branch that's expected to set it - then set it only
when the server itself has resolved a value, built exclusively from server-side state (a
scheduler's own resolved port, never anything derived from request data):

```go
pr.Out.Header.Del("x-upstream-override") // strip client-supplied value unconditionally, first
if resolved.UpstreamOverride != "" {
    pr.Out.Header.Set("x-upstream-override", resolved.UpstreamOverride) // server-derived only
}
```

A bare `.Set()` on the one branch that uses the header would be safe on that branch alone - the
unconditional `.Del()` first is defense in depth for every *other* branch, so a future backend
kind or code path can never accidentally let a client-supplied value through un-scrubbed. Add a
test asserting a request carrying a forged value for this header never reaches the upstream with
that value intact, across every branch, not just the one the header is meant for.

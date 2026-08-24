ref: compressor-topology:compressor-ssrf-hardening
doc: compressor-topology
slug: compressor-ssrf-hardening
title: 5b. Header-injection / SSRF hardening on `x-compress-base-url`
category: network
source: docs/v5-headroom-topology.md

## 5b. Header-injection / SSRF hardening on `x-compress-base-url`

**Sprint 7 note (docs/v5-headroom-replacement.md, 2026-08-20):** the header this section analyzes
was renamed `x-headroom-base-url` → `x-compress-base-url` as part of the full "Headroom" →
"compressor" rename, once headroom-ai itself (the third-party proxy this section's Facts A/B
describe) had been fully replaced by `foundry-compress` (Sprint 3). The mechanism and the
hardening rule below are unchanged; only the wire header name is different now. Facts A/B below
describe headroom-ai's own Python source as it was investigated at the time — left as written,
not updated, since it's a historical record of that (now-retired) third-party code.

Raised by the user 2026-07-28, reviewing §3/§4: since a0 will set `x-headroom-base-url` per
request to route through the shared local proxy, does an external client's own request get a
chance to set/inject that header and redirect Headroom's outbound request anywhere it wants
(classic SSRF via an unsanitized upstream-target header on a reverse proxy)? Investigated rather
than assumed; two independent facts bound the real risk, and one implementation rule closes the
remaining gap.

**Fact A — Headroom itself does no origin restriction.** `_resolve_openai_upstream_base`
(`proxy/handlers/openai.py:113`) only checks that the header parses to a URL with a hostname and a
`http`/`https` scheme (`_normalize_origin`, `openai.py:131`) — it does **not** constrain the host to
loopback or to any allowlist. Taken alone, this would make Headroom happy to proxy to any origin a
caller names. This is third-party code (`/opt/headroom/tools/headroom-ai`, installed via `uv`, not
part of this repo) — we cannot durably patch its site-packages as part of this design; any local
edit would be silently lost on the next `headroom-ai` upgrade/reinstall. Not something to "fix" in
this repo.

**Fact B — all four Headroom proxy ports are bound to `127.0.0.1` only**, both by the tool's own
default (`cli/proxy.py:145-148`, `--host` defaults to `127.0.0.1`, no unit overrides it) and
confirmed live on this host (`ss -tlnp` shows all of 8788-8791 as `127.0.0.1:<port>`, not `0.0.0.0`).
**No remote host, and no consumer reaching a0 over the tailnet, can open a TCP connection to a
Headroom proxy port directly** — only a process already running locally on this host can. a0 itself is
the only such caller today. This means Fact A's lack of origin validation is not, by itself,
remotely exploitable through a0's public surface — the attacker would need code execution on the proxy host
already, a materially higher bar than "any a0 consumer," and one this redesign does not create or
worsen.

**The gap this redesign must close is narrower: a0 must never forward a client-supplied
`x-headroom-base-url` verbatim.** Checked the router package for any path that could do that: only
two `httputil.ReverseProxy` instances exist in `internal/router` — `embeddings.go:57` (unrelated to
Headroom, no `x-headroom-*` handling) and `proxy.go:261` (`chatCompletions`, the one this design
touches). There is no generic/catch-all passthrough route in the mux (`router.go`, `httpapi.go`)
that could carry a client's raw headers to a Headroom port bypassing `resolveBackend`.

**Mandatory implementation rule (Phase 1):** in the `proxy.go` Rewrite hook, before setting the
resolved value, **unconditionally delete any inbound `x-headroom-base-url` (and any other
`x-headroom-*`) header from `pr.Out`** — not just on the local-Headroom-fronted branch, on *every*
branch — then set the header only when `resolved.UpstreamOverride` is non-empty, to a value built
exclusively from `s.deps.Slots`/the scheduler's own resolved port (never from request data). i.e.:

```go
pr.Out.Header.Del("x-compress-base-url") // strip client-supplied value unconditionally, first
if resolved.UpstreamOverride != "" {
    pr.Out.Header.Set("x-compress-base-url", resolved.UpstreamOverride) // server-derived only
}
```

`Header.Set` already replaces rather than appends, so a bare `.Set()` on the one branch that uses
it would have been safe *on that branch* — the explicit unconditional `.Del()` first is defense in
depth for every *other* branch (remote/passthrough), so a future backend kind or code path can
never accidentally let a client value through un-scrubbed. Add a test asserting a request carrying
a forged `x-headroom-base-url` never reaches the upstream with that value intact, for both the
local-Headroom branch and at least one other branch (remote/bypass).

**Net assessment:** not currently exploitable (Headroom's loopback binding is the effective
boundary today), but worth closing precisely because this redesign is what starts routing live
production traffic's headers through a0's Rewrite hook with a proxy that itself performs no
validation — the strip-then-set rule above is the durable fix, since it lives in code we own and
doesn't depend on a third-party package's future behavior.

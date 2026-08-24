# Firecrawl SSRF guard covers all fetch adapters, not just direct

## Context

Smith's `web_fetch` tool accepts LLM-controlled URLs and fetches them through a
chain of adapters: firecrawl (tried first), then `direct` (fallback). The `direct`
adapter uses `newGuardedHTTPClient` (`web/client.go`), whose `DialContext` rejects
loopback, RFC1918, link-local, and CGNAT (100.64.0.0/10 — this fleet's tailnet
range) addresses at dial time.

**The problem:** firecrawl uses `newPlainHTTPClient` — no SSRF guard. Since
firecrawl is tried *before* `direct`, the guard never runs when firecrawl succeeds.
On ForgeHost, firecrawl is self-hosted on the tailnet, so it can reach internal services
(a0 :8085, MCP :8095, ops :5000, ComfyUI, embeddings). An LLM coerced via prompt
injection from fetched web content can call `web_fetch` on an internal URL; firecrawl
fetches it and returns the content; `direct`'s SSRF guard never executes.

The `direct` adapter's guard was correctly designed (dial-time IP validation, not
pre-resolve, to resist DNS rebinding), but its scope was wrong — it only covered one
of two fetch adapters.

## Decision

Move the SSRF validation to `doFetch` in `web/web.go`, before *any* adapter runs.
The new `validateFetchURL` function (`web/client.go`) parses the URL, resolves the
host, and rejects non-public IPs — the same `isPublicIP` check the `direct` adapter
used, but applied universally. The `direct` adapter retains its dial-time check as
defense-in-depth against DNS rebinding (a resolver that answers differently between
the pre-check and the connect).

## Consequences

- Firecrawl and any future fetch adapter are covered by the same SSRF guard.
- The `direct` adapter's dial-time guard is now the second layer, not the only one.
- The `AllowDirectHost` override (used by tests pointing at `httptest.Server` on
  127.0.0.1) is passed through to `validateFetchURL`, preserving test ergonomics.
- `file://` and other non-http schemes are rejected explicitly at the URL-parse
  stage, before any adapter sees them.

## Remediation

Implemented in Phase 1 of the QA remediation sprint (2026-08-13). See
`QA-REPORT.md` finding #4 and `docs/v5-qa-remediation-2026-08-13.md` Phase 1.

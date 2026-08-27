# Firecrawl SSRF guard covers all fetch adapters, not just direct

## Context

Smith's `web_fetch` tool accepts LLM-controlled URLs and fetches them through a chain of
adapters: firecrawl (tried first), then `direct` (fallback). The `direct` adapter uses a guarded
HTTP client whose dial hook rejects loopback, RFC1918, link-local, and CGNAT addresses at dial
time.

**The problem:** the firecrawl adapter used a plain HTTP client with no SSRF guard at all. Since
firecrawl is tried before `direct`, the guard never ran when firecrawl succeeded. If firecrawl
itself is reachable from inside your private network (e.g. self-hosted on the same tailnet as
your other internal services), it can reach those internal services on the caller's behalf. An
LLM coerced via prompt injection from fetched web content could call `web_fetch` on an internal
URL; firecrawl would fetch it and return the content, with `direct`'s SSRF guard never executing
at all.

The `direct` adapter's guard was correctly designed (dial-time IP validation, not pre-resolve,
to resist DNS rebinding), but its scope was wrong - it only covered one of two fetch adapters.

## Decision

Move the SSRF validation to run before *any* adapter, not just `direct`. A shared
`validateFetchURL` function parses the URL, resolves the host, and rejects non-public IPs - the
same check the `direct` adapter used, but applied universally. The `direct` adapter retains its
own dial-time check as defense-in-depth against DNS rebinding (a resolver that answers
differently between the pre-check and the connect).

## Consequences

- Firecrawl and any future fetch adapter are covered by the same SSRF guard.
- The `direct` adapter's dial-time guard is now a second layer, not the only one.
- A configurable allow-list for direct test/loopback hosts is passed through to the shared
  validator, preserving test ergonomics without weakening the production check.
- `file://` and other non-http schemes are rejected explicitly at the URL-parse stage, before any
  adapter sees them.

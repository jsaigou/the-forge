ref: compressor-topology:compressor-target-topology
doc: compressor-topology
slug: compressor-target-topology
title: 4. Target topology: "one proxy per scope"
category: compressor
source: docs/v5-headroom-topology.md

## 4. Target topology: "one proxy per scope"

Decided with the user 2026-07-28, after the corrections in §2–3 made the tradeoffs clear.

- **One shared LOCAL proxy** (`headroom-local`), fronting all four A1–A4 slots. `catalogChain`
  (`routing.go:321`) routes local Configs through it, and `resolveBackend` sets
  `x-headroom-base-url: http://127.0.0.1:<currently-resolved-slot-port>` per request (the same
  port `EnsureLoaded`/the scheduler already resolved for this request — no new placement lookup).
  **Update 2026-08-15:** the header value is the bare slot ROOT, without `/v1` — headroom-ai
  ≥0.35.0 appends `/v1` to `x-headroom-base-url` itself before forwarding, so the original
  contract (base WITH `/v1`, correct for 0.30.0) now doubles to `/v1/v1/chat/completions` and
  the slot answers 404 "File Not Found". Found during the 0.35.0 OOM incident investigation
  (item 14 in `investigations.md`); fixed in `resolveBackend` (`UpstreamOverride: slotRoot`).
  Replaces the dead a1+a2 pair. Local slots need no upstream auth, so there's no credential to
  route per-request alongside the header.
- **One proxy per enabled remote provider** (deepseek, external/aiand, future ones), each with its
  fixed upstream unchanged from today (`offeringChain`, `providerCredential`) — but **provisioned
  when a provider is enabled + Headroom-linked, torn down/orphaned when it isn't**, instead of
  hand-created. Kept as separate processes (not folded into the local scope's shared-header
  pattern) deliberately: separate processes keep per-proxy *request routing* independent even
  though — **correction, see "Update 2026-07-30" below** — `headroom_persistent_savings_*` turned
  out to be shared across all instances via a common file, not actually per-process as originally
  assumed here; the real per-proxy savings breakdown now comes from each proxy's own volatile
  counters instead (`collector/run.go recordHeadroomSavings`, §7).

This is not the literal "up to 4 local + one per provider" reading of §11 (one proxy per
*Config*) — it's coarser, and deliberately so: §2 established that model-sharing a proxy carries no
correctness risk, so there is no reason to pay for 4 local processes (4× the memory/CPU/systemd
unit overhead, 4× the savings-counter fragmentation to reconcile in the dashboard) when one process
does the job exactly as safely.

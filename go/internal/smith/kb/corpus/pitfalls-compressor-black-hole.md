ref: pitfalls:compressor-black-hole
doc: pitfalls
slug: compressor-black-hole
title: A host reboot can leave every Compressor proxy dead while a0 reports healthy
category: compressor
source: docs/v5-headroom-topology.md

*Outage:* the Foundry host did a full reboot. `foundry-daemon` (systemd-enabled) came back on its own;
the three `headroom@<service>` template instances are **deliberately not enabled** (§5's whole
point is that they're provisioned/torn down dynamically, never meant to blanket-autostart) and
stayed dead. Nothing re-synced them against the store's non-orphaned `headroom_proxies` rows, so
`router.resolveBackend` kept routing every request — local and remote alike — through a dead
proxy, hit connection-refused at the `httputil.ReverseProxy` level, and every single a0 request
failed for about 1.5 hours before the operator noticed via LibreChat. Root-caused live via
`journalctl` + a direct `curl` reproduction (83ms 502 against an already-healthy, already-loaded
slot), not guessed. Fixed with a **boot-time reconcile** in `cmd/foundryd/main.go`: right after
`headroomProvisioner` is constructed and before the router starts listening, every non-orphaned
`store.ProxyRow` gets `Provisioner.Restart`'d (not `Reconcile` — that would also rewrite the env
file, which isn't needed and isn't safe for a legacy hand-created unit; `Restart` matches the
existing `POST /api/v1/headroom/restart` endpoint's own "safe for any unit" contract). Best-effort
— a failure here logs and does not block daemon startup. Live-verified in the post-deploy
restart's journal: all three proxies restart automatically *before* "router config loaded from
store" logs, closing the gap — any future reboot self-heals instead of silently blackholing every
a0 request. Deployed `v5.0.21-a0headroomfix-8d07b31`.

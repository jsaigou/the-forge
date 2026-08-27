ref: pitfalls:compressor-black-hole
doc: pitfalls
slug: compressor-black-hole
title: A host reboot can leave a compressor proxy dead while the router reports healthy
category: compressor
source: docs/llm-router.md

A compressor proxy that is provisioned and torn down dynamically (rather than a
statically-enabled systemd unit) does not come back on its own after a full host reboot -
that's the whole point of provisioning it dynamically instead of enabling it. If nothing
re-syncs proxy state against what the router believes is available at startup, the router
keeps routing requests - local and remote alike - through a proxy that is simply not running,
and every request through it fails at the connection level while the rest of the system reports
healthy. Because the failure is a fast connection-refused rather than a hang, this can look like
a targeted outage rather than the actual cause (a stale process-management gap).

**Standing defense:** a boot-time reconcile step restarts every non-orphaned proxy before the
router starts accepting requests, so any reboot self-heals instead of silently black-holing
traffic through a dead proxy. If you build something with the same "provisioned on demand, not
enabled at boot" shape, give it the same boot-time reconcile - don't rely on nothing ever
rebooting the host.

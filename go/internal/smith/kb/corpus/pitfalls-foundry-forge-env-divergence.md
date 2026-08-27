ref: pitfalls:foundry-forge-env-divergence
doc: pitfalls
slug: foundry-forge-env-divergence
title: A rename that updates code but not deployed scripts can leave the running process silently stale
category: slots
source: docs/pitfalls.md

Renaming an environment-variable prefix (or any config key) in code is not the same as
completing the rename everywhere it matters. If the env-file preservation logic that writes
per-slot sysconfig files only *skips writing* lines under the old prefix rather than actively
removing them, any old-prefixed line frozen at its last pre-rename value sticks around
indefinitely, alongside the fresh values, unrelated to them. If the actual deployed launcher
script on the host is not also redeployed to read the new prefix, the **running process** stays
governed by the frozen old values while the dashboard/API - reading the new prefix - confidently
reports something else. Nothing errors; the two views just silently disagree, and the gap is
invisible until the process is restarted or reloaded against a changed value.

**Fix pattern:** an env-file preserve-filter should actively strip lines under a retired prefix,
not merely stop writing new ones under it. **Standing defense:** a ground-truth check that
compares the actually-running process's own self-reported identity (from its live status
endpoint) against the engine's configured belief, for every loaded slot on every sweep - this
catches this entire class of drift regardless of what specifically caused it, not just the one
rename that first surfaced it.

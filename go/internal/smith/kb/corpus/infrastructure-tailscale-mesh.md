ref: infrastructure:tailscale-mesh
doc: infrastructure
slug: tailscale-mesh
title: Tailscale mesh — inventory settings and provisioning
category: network
source: docs/infrastructure.md

The mesh inventory is deployment data - smith reads it live from the
`smith.mesh.services` settings key (one JSON entry per service: name,
surface aliases, tailnet address) and combines it with live probes to
answer reachability questions. Provision it per install with
`forge smith import-local <file>` - schema in
`internal/smith/local_seed.go`, synthetic example in
`docs/examples/smith-local-seed.example.json`; the seed file itself is
layer-2 knowledge and never ships. A fresh install with no inventory
still answers the deployment-agnostic probes ("internet", "tailnet"),
and mesh reachability beyond that is an honest gap until the import.

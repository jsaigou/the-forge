# The Forge — Roadmap

Public roadmap for [github.com/jsaigou/the-forge](https://github.com/jsaigou/the-forge).
No dates are promised; ordering reflects intent, not commitment.

## Now shipped (v0.5)

- **Single-binary daemon** — one Go binary serves the dashboard, the a0 router, and
  the MCP control surface on their canonical ports; all configuration lives in the
  embedded store, no config files.
- **a0 router** — one OpenAI-compatible endpoint for every configured model, local
  or remote. On-demand loading: request a model and the scheduler places it on a
  bay, evicting to make room when memory is tight.
- **Token compression** — prompts sent to remote providers pass through Compressor,
  with realized savings reported per provider in the dashboard.
- **Smith agent** — built-in maintenance agent: scheduled checks (hourly/daily),
  drift and failure detection, plain-language Q&A from inside the dashboard.
- **Ops TUI** — full-screen terminal console (`forge tui`) over the same machinery
  the dashboard drives.
- **Dashboard PWA** — console, cost/power/Compressor analytics, model catalog with
  benchmarks, scheduling controls, settings, and Smith chat/diagnostics.
- **MCP control surface** — fleet introspection and management tools (`status`,
  `can_fit`, `ensure_loaded`, reservations) for agents, with per-key identity.
- **Strix Halo installer** — guided KGC installer for AMD Ryzen AI Max+ 395 boxes
  (gfx1151), including kernel mitigations and systemd units.

## Next

- **Public release hardening** — first-run experience, docs pass, and packaging
  review ahead of opening installs beyond the current operator base.
- **Broader installer profiles** — hardware profiles beyond Strix Halo (other AMD
  APUs/discrete GPUs, Apple Silicon, CPU-only) driven by the same fit-check logic
  that already powers scheduling.
- **Scheduled-jobs UX expansion** — richer recurrence windows, job chains, and
  per-job notifications in the Scheduling tab.
- **Smith autonomous catalog writes** — let Smith apply vetted catalog edits
  (benchmarks, notes, variant metadata) itself, behind an explicit policy switch,
  instead of only proposing them.

## Later

- **Multi-node fleet orchestration** — schedule across paired/remote nodes as one
  pool, building on the existing node-agent pairing model.
- **Public non-tailnet auth path** — password/TOTP/passkey factors as a primary
  auth path for deployments exposed beyond a tailnet, replacing today's
  tailnet-presence bypass assumption with per-deployment policy.
- **Packaged releases** — signed binaries and container images so installs don't
  require a Go toolchain.

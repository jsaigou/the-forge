# Config replaces Mode as the unit of loading

The V4 "Mode" (`forge.toml [modes.*]`) was a flat struct combining launch recipe, slot preference, and display data — it was both the *how* (extra_args, ctx, parallel) and the *what* (label, icon, family). We dissolved Mode entirely and replaced it with **Config**: a named, loadable recipe that is the operator-facing unit of loading ("load qwen36" = load the Config named qwen36). The dashboard switcher, scheduler, registry, and usage tracking all key on Config. Mode was a holdover from an earlier system that made the CLI easier; the CLI will be redesigned around Config.

Key consequences:
- `Services[]` (the per-mode launch list) collapses to a single pointer — one Config = one Artifact + one Engine Build + args. Multi-service modes are gone (not needed today).
- `port_role` (slot preference) is removed — slot assignment is a pure runtime decision by the scheduler; a0 handles presentation.
- Each Config has a visibility flag (`visible`/`hidden`) and each Variant has one Config flagged `is_default`. The dashboard shows visible Configs; Settings manages all Configs.
- The `creative` mode (`type="service"`) becomes a separate **Service** concept — ComfyUI is not a Config.

## Considered Options

- **Config replaces Mode (chosen):** Config is the named unit; display data moves to Model/Variant; slot is runtime-only.
- **Keep Mode, add Config underneath (rejected):** Mode stays as the operator-facing unit, references a Config. Rejected because Mode was duplicating data (two Modes sharing 95% of their launch recipe had to duplicate the entire `Services[]` block) and the indirection served no purpose once Config existed.

# Store-backed model catalog replaces toml files

The V4/v0.5 model/launch data lived smeared across three files — `models.toml` (card display), `forge.toml [modes.*]` (launch recipes), and live-derived GGUF header reads — with no write path in the Go code (V4's `config_writer.py`/tomlkit was deliberately dropped in v0.5). We decided to migrate the editable subset (models, variants, configs, remote offerings, benchmarks, notes) into SQLite tables, with `migrate-v4` extended to seed from the existing toml files on first run. The toml files become a one-time seed, not the source of truth. This matches how providers, settings, model_profiles, and policy already live in the store, and avoids building a lossless toml writer + SIGHUP reload path (Option B) that nothing else in the system wants.

## Considered Options

- **Option A (chosen):** Store-backed source of truth. Toml files are one-time seed. Edits are live — no file writes, no SIGHUP, no perms issues. Atomic + transactional + validated at the DB boundary.
- **Option B (rejected):** UI rewrites toml files + SIGHUP reload. Files stay authoritative, but requires building the file-writer that was deliberately omitted, daemon write access to `/etc/forge/*`, and a reload race with in-flight loads. The router has no SIGHUP reload path today — would need one built.

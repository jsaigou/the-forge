-- Schema v68 — API key display_name: the operator's preferred human-facing
-- consumer label (e.g. "ExampleHost (OpenCode)"). Surfaced on bearer identities
-- and used verbatim by a0's slot-consumer attribution; empty → the router
-- falls back to User-Agent/key-name derivation.
ALTER TABLE api_keys ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

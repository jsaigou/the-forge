-- Schema v69 — API key host binding + expiry (security sprint 3, #34/#36).
-- bound_ip: '' (default) = unbound — required for router/MCP keys and the
-- documented remote-admin-over-tailnet CLI/TUI use case; non-empty = the
-- exact client IP (post authz.EffectiveRemoteAddr resolution) the key must
-- present to verify. expires_at: NULL = never expires.
ALTER TABLE api_keys ADD COLUMN bound_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN expires_at INTEGER;

-- SPDX-License-Identifier: Apache-2.0
-- Schema v54 (smith experience Sprint S1 — docs/v5-smith-experience.md §2.6 R2).
--
-- smith.web.enabled now defaults to true in code (web/config.go's
-- DefaultConfig). This migration seeds the setting true for existing installs
-- where the key is ABSENT — never overrides an explicit operator-off. A fresh
-- install with no settings gets the code default (true) without needing this
-- row; this row is for installs that already have other smith.web.* settings
-- but never touched the enabled key.
INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.web.enabled', 'true', strftime('%s', 'now'));

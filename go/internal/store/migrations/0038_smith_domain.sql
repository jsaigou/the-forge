-- SPDX-License-Identifier: Apache-2.0
-- Schema v38 (smith P6 — domain modules, docs/v5-smith.md §4.9).
--
-- smith_binaries backs FR6's binary/dependency tracker. A table, not the
-- checks_blocked_recheck.go "carry state in the previous finding's evidence"
-- trick used elsewhere in this package: that pattern fits ONE recheck loop
-- with one cadence, but a tracked binary has THREE independent probe
-- cadences (installed version — checked on every deep sweep; source-tree
-- HEAD — same; upstream release — checked at most daily, its own cache-TTL
-- cooldown per web/), each with its own checked_at, and the Settings/
-- Diagnostics surface renders a live inventory (current state per binary),
-- not a sweep-result history — an append-only findings-shaped table would
-- make "what's the latest known version of headroom-ai" an aggregate query
-- instead of a row read.
--
-- Conventions: unix-seconds INTEGER timestamps, 0/1 booleans, JSON as TEXT
-- (matches migration 0033/0037).

CREATE TABLE IF NOT EXISTS smith_binaries (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    name                  TEXT    NOT NULL,               -- operator-facing identity, e.g. "llama.cpp (vulkan)"
    kind                  TEXT    NOT NULL CHECK (kind IN ('llama_build','python_pkg','runtime','service')),
    path                  TEXT    NOT NULL DEFAULT '',    -- binary path or source tree root
    installed_version     TEXT    NOT NULL DEFAULT '',    -- from --version / /props build_info / pip metadata
    installed_checked_at  INTEGER,
    source_kind           TEXT    NOT NULL DEFAULT '',    -- '' | 'git'
    source_ref            TEXT    NOT NULL DEFAULT '',    -- git remote/tree path, when source_kind='git'
    source_version        TEXT    NOT NULL DEFAULT '',    -- HEAD short SHA read from source_ref/.git
    source_checked_at     INTEGER,
    latest_version        TEXT    NOT NULL DEFAULT '',    -- upstream release/commit, via web research
    latest_checked_at     INTEGER,
    notes                 TEXT    NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_smith_binaries_name ON smith_binaries(name);

-- smith.comfyui / smith.binaries / smith.sourcing seeds (docs/v5-smith.md
-- §5; "seeded once, no hardcoded default anywhere in code" — the smith.web.*
-- convention from 0037). Roots/paths below are ForgeHost's real values,
-- ground-truthed live 2026-08-11 (see the P6 plan's "Ground truth found live
-- on ForgeHost" section) — a non-ForgeHost install simply edits these in Settings.
--
-- comfyui.model_roots carries BOTH real model trees found live: ComfyUI
-- resolves a loader reference by basename across every root declared in its
-- own extra_model_paths.yaml, and /opt/comfyui/app/extra_model_paths.yaml
-- declares base_path /opt/comfyui/models as a SECOND tree distinct from
-- /opt/comfyui/app/models — a single hardcoded root (as the original design
-- doc assumed) would silently miss half the disk in the dependency map's
-- root-coverage guardrail.
INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.comfyui.enabled', 'true', strftime('%s', 'now')),
    ('smith.comfyui.url', '"http://127.0.0.1:3001"', strftime('%s', 'now')),
    ('smith.comfyui.unit', '"ai-mode-comfyui"', strftime('%s', 'now')),
    ('smith.comfyui.model_roots',
        '["/opt/comfyui/app/models","/opt/comfyui/models"]', strftime('%s', 'now')),
    ('smith.comfyui.workflow_dirs',
        '["/opt/comfyui/app/user/default/workflows"]', strftime('%s', 'now')),
    ('smith.binaries.enabled', 'true', strftime('%s', 'now')),
    ('smith.binaries.tracked',
        '[{"name":"llama.cpp (vulkan)","kind":"llama_build","path":"/opt/forge/llama.cpp/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp"},' ||
        '{"name":"llama.cpp (puzzle)","kind":"llama_build","path":"/opt/forge/llama.cpp-puzzle/build-rocm715/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-puzzle"},' ||
        '{"name":"llama.cpp (kintsugi)","kind":"llama_build","path":"/opt/forge/llama.cpp-kintsugi/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-kintsugi"},' ||
        '{"name":"llama.cpp (poolside)","kind":"llama_build","path":"/opt/forge/llama.cpp-poolside/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-poolside"},' ||
        '{"name":"headroom-ai","kind":"python_pkg","path":"/opt/headroom/bin/headroom","source_kind":"","source_ref":""}]',
        strftime('%s', 'now')),
    ('smith.sourcing.enabled', 'true', strftime('%s', 'now')),
    ('smith.tailscale.watch_peers', '["pairnode","core"]', strftime('%s', 'now'));

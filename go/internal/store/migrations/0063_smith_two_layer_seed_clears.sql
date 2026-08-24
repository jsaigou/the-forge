-- SPDX-License-Identifier: Apache-2.0
-- Schema v63 (two-layer knowledge architecture, phase 2 — S0 of the ops
-- sprint series, docs/v5-ops-sprints-2026-08-21.md).
--
-- Conditional-clears the deployment seeds that 0037/0038 shipped. The
-- two-layer rule (operator directive 2026-08-21): migrations must not
-- provision live-environment data; the seams are created EMPTY and each
-- install provisions its own values via `foundryd smith import-local` from
-- an operator-maintained local file (schema smith.LocalSeed, which now
-- carries web_providers + comfyui sections so every cleared key has a
-- provisioning path).
--
-- Each DELETE is value-equality guarded on EXACTLY the string its seed
-- migration inserted: an install that never customized the key is reset to
-- the empty seam; an install that edited the key afterwards keeps its own
-- value untouched. A fresh database runs these after 0037/0038's INSERTs in
-- the same boot, so it never observes the seeded values at all.
--
-- The guard literals necessarily quote the strings being removed — this
-- migration is a tombstone for them, not a carrier: nothing reads these
-- rows as configuration, and every future seed ships empty.

DELETE FROM settings WHERE key = 'smith.web.searxng'
    AND value = '{"base_url":"https://searxng.example.ts.net","enabled":true,"api_key":""}';

DELETE FROM settings WHERE key = 'smith.web.firecrawl'
    AND value = '{"base_url":"https://firecrawl.example.ts.net","enabled":true,"api_key":""}';

DELETE FROM settings WHERE key = 'smith.comfyui.url'
    AND value = '"http://127.0.0.1:3001"';

DELETE FROM settings WHERE key = 'smith.comfyui.unit'
    AND value = '"ai-mode-comfyui"';

DELETE FROM settings WHERE key = 'smith.comfyui.model_roots'
    AND value = '["/opt/comfyui/app/models","/opt/comfyui/models"]';

DELETE FROM settings WHERE key = 'smith.comfyui.workflow_dirs'
    AND value = '["/opt/comfyui/app/user/default/workflows"]';

-- smith.binaries.tracked is cleared under its exact original seed string
-- too, but note the guard's reach is deliberately narrow: an install whose
-- tracked-binary inventory has since been edited (fork registrations are
-- ordinary operator actions) no longer matches and KEEPS its evolved
-- values — those are legitimate per-install layer-2 state by definition.
-- Such an install can re-export its inventory into its local seed file at
-- leisure; nothing here forces a reset.

DELETE FROM settings WHERE key = 'smith.binaries.tracked'
    AND value =
        '[{"name":"llama.cpp (vulkan)","kind":"llama_build","path":"/opt/forge/llama.cpp/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp"},' ||
        '{"name":"llama.cpp (puzzle)","kind":"llama_build","path":"/opt/forge/llama.cpp-puzzle/build-rocm715/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-puzzle"},' ||
        '{"name":"llama.cpp (kintsugi)","kind":"llama_build","path":"/opt/forge/llama.cpp-kintsugi/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-kintsugi"},' ||
        '{"name":"llama.cpp (poolside)","kind":"llama_build","path":"/opt/forge/llama.cpp-poolside/build/bin/llama-server","source_kind":"git","source_ref":"/opt/forge/llama.cpp-poolside"},' ||
        '{"name":"headroom-ai","kind":"python_pkg","path":"/opt/headroom/bin/headroom","source_kind":"","source_ref":""}]';

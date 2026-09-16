-- SPDX-License-Identifier: Apache-2.0
-- Schema v86 (per-request thinking control, Sprint T3, 2026-09-14).
-- model_aliases lets a wire-visible model name resolve to a real catalog
-- Config plus a set of request fields FORCED onto every request through
-- that name (unlike Config.ReasoningEffortDefault, which only applies when
-- the client sends nothing — an alias's request_defaults always win, even
-- over a client-sent value, since the whole point of requesting through
-- this name is to guarantee that behavior for a consumer that structurally
-- cannot send it itself).
--
-- The motivating case: gemma4-26b-a4b-nothink exists today as a whole
-- second catalog Config over the identical GGUF as gemma4-26b-a4b, purely
-- to bake `--chat-template-kwargs '{"enable_thinking":false}'` into its
-- launch flags for podcast_creator (docs/opennotebook.md), which has no way
-- to send custom chat_template_kwargs per-request. T2's reasoning_effort
-- translation layer makes that unnecessary: an alias with
-- request_defaults={"reasoning_effort":"none"} gets the identical effect
-- with no duplicate config, no duplicate artifact rows, and no second
-- instance of the same-weights admission hazard (ADR-0006). This migration
-- only adds the table — converting gemma4-26b-a4b-nothink itself is a
-- separate, deliberately later step (needs a live check that
-- podcast_creator still works after).
--
-- ON DELETE CASCADE (not SET NULL, unlike the optional FKs elsewhere in
-- this schema): an alias with no target config is meaningless and
-- dangerous — routing a live request through it would either 404 or
-- resolve to nothing sensible. Deleting the target config should delete
-- the alias with it, never leave a dangling reference.
CREATE TABLE model_aliases (
    id                INTEGER PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    config_id         INTEGER NOT NULL REFERENCES configs(id) ON DELETE CASCADE,
    request_defaults  TEXT NOT NULL DEFAULT '{}',
    visibility        TEXT NOT NULL DEFAULT 'visible' CHECK (visibility IN ('visible', 'hidden'))
);

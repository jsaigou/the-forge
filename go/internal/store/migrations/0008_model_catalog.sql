-- SPDX-License-Identifier: Apache-2.0
-- Schema v8 (Sprint MODEL CATALOG — docs/v5-modes-config-editable.md §Phase 1,
-- ADRs 0001–0005). The store-backed model database that replaces the
-- models.toml + foundryd.toml [modes.*] + router.toml (remote) data smear.
-- Conventions match 0001–0007: unix-seconds INTEGER timestamps, 0/1 booleans,
-- JSON as TEXT, nullable columns map to Go zero values.
--
-- The toml files become a one-time seed via `foundryd migrate-v4` (seed only
-- if the target table is empty — never clobber). Phase 2 migrates the 17
-- cfg.Modes[...] read sites + the router catalog to consult these tables.

-- ── Identity & Derivation ────────────────────────────────────────────────────

-- Family: broad grouping by base architecture or release lineage (Gemma, Qwen,
-- NVIDIA, …). Optional — a Model may have no Family.
CREATE TABLE IF NOT EXISTS families (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

-- Model: one specific parameter configuration within a Family (CONTEXT.md).
-- Architecture is per-Model (GGUF general.architecture); empty until the
-- registry populates it from a live GGUF read (Phase 2).
CREATE TABLE IF NOT EXISTS models (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    family_id       INTEGER REFERENCES families(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    architecture    TEXT NOT NULL DEFAULT '',
    parameter_count TEXT NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    creator         TEXT NOT NULL DEFAULT '',
    license_name    TEXT NOT NULL DEFAULT '',
    license_url     TEXT NOT NULL DEFAULT '',
    hf_repo         TEXT NOT NULL DEFAULT '',
    logo            TEXT NOT NULL DEFAULT '',
    key_features    TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_models_family ON models(family_id);
CREATE INDEX IF NOT EXISTS idx_models_name ON models(name);

-- Variant: a structural derivation of a Model (abliteration, finetune, merge,
-- mtp-head-add, uncensor, …). Same parameter count, different tensor prep.
-- Derivation graph is Variant → Variant (source_variant_id).
CREATE TABLE IF NOT EXISTS variants (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id             INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    derivation_type      TEXT NOT NULL DEFAULT '',
    source_variant_id    INTEGER REFERENCES variants(id) ON DELETE SET NULL,
    trained_ctx          INTEGER NOT NULL DEFAULT 0,
    is_abliterated       INTEGER NOT NULL DEFAULT 0,
    abliteration_quality TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_variants_model ON variants(model_id);

-- Quantization: the precision scheme (Q6_K, Q8_0, Q4_K_M, …). Enum-like —
-- seeded here so the seed path and CRUD APIs can reference stable IDs.
CREATE TABLE IF NOT EXISTS quantizations (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);
INSERT OR IGNORE INTO quantizations (name) VALUES
    ('Q4_0'), ('Q4_K_M'), ('Q4_K_S'), ('Q4_K_L'), ('Q4_K_XL'),
    ('Q5_K_M'), ('Q5_K_S'), ('Q5_K_L'), ('Q5_K_XL'),
    ('Q6_K'), ('Q6_K_P'), ('Q6_K_L'),
    ('Q8_0'), ('F16'), ('F32'),
    ('MXFP4'), ('BF16'), ('UD_Q4_K_XL'), ('UD_Q5_K_XL'), ('UD_Q6_K_XL');

-- Format: the container format (GGUF, safetensors). Enum-like — seeded here.
CREATE TABLE IF NOT EXISTS formats (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);
INSERT OR IGNORE INTO formats (name) VALUES ('GGUF'), ('safetensors');

-- Artifact: the concrete file on disk. Each shard is an Artifact; a shard set
-- is a group sharing shard_set_id. Weight Artifacts back one
-- (Variant, Quantization, Format). Auxiliary Artifacts (mmproj, tokenizer)
-- are shared across Variants via compatibilities. The `missing` flag is set
-- when the file is no longer on disk (re-scan on access); the catalog labels
-- rather than gates.
CREATE TABLE IF NOT EXISTS artifacts (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    variant_id            INTEGER NOT NULL REFERENCES variants(id) ON DELETE CASCADE,
    quantization_id       INTEGER REFERENCES quantizations(id),
    format_id             INTEGER NOT NULL REFERENCES formats(id),
    file_path             TEXT NOT NULL,
    shard_set_id          TEXT,
    is_auxiliary          INTEGER NOT NULL DEFAULT 0,
    artifact_type         TEXT NOT NULL DEFAULT 'weight'
        CHECK (artifact_type IN ('weight', 'mmproj', 'tokenizer')),
    missing               INTEGER NOT NULL DEFAULT 0,
    sha256                TEXT,
    file_size_bytes       INTEGER NOT NULL DEFAULT 0,
    gguf_arch             TEXT NOT NULL DEFAULT '',
    gguf_trained_ctx      INTEGER NOT NULL DEFAULT 0,
    gguf_parameter_count TEXT NOT NULL DEFAULT '',
    gguf_quant_type       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_artifacts_variant ON artifacts(variant_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_path ON artifacts(file_path);

-- Compatibility: a factual relationship between an auxiliary Artifact (e.g. an
-- mmproj) and the Variants it can serve. Usually scoped within one Model;
-- cross-Model compatibility is unusual but not structurally prohibited.
CREATE TABLE IF NOT EXISTS compatibilities (
    auxiliary_artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    variant_id           INTEGER NOT NULL REFERENCES variants(id) ON DELETE CASCADE,
    PRIMARY KEY (auxiliary_artifact_id, variant_id)
);

-- ── Launch & Inference ───────────────────────────────────────────────────────

-- Engine: the inference software (llama.cpp, vLLM). Extensible (mojo/max).
CREATE TABLE IF NOT EXISTS engines (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);
INSERT OR IGNORE INTO engines (name) VALUES ('llama.cpp'), ('vLLM');

-- Build: a specific compiled version of an Engine (vulkan-build, rocm-build,
-- puzzle-port-branch). One Engine has many Builds.
CREATE TABLE IF NOT EXISTS builds (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    engine_id   INTEGER NOT NULL REFERENCES engines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    binary_path TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_builds_engine ON builds(engine_id);

-- Config: a named, loadable recipe for running one weight Artifact with one
-- Engine Build (ADR-0002 — replaces V4 Mode). The operator-facing unit of
-- loading ("load qwen36" = load the Config named qwen36). Each Config is
-- experimental (default) or verified (PROFILE ran clean + coherent + n_ctx
-- matched). Each has a visibility flag; each Variant has one is_default
-- Config.
CREATE TABLE IF NOT EXISTS configs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT NOT NULL UNIQUE,
    variant_id         INTEGER NOT NULL REFERENCES variants(id) ON DELETE RESTRICT,
    weight_artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
    engine_id          INTEGER NOT NULL REFERENCES engines(id),
    build_id           INTEGER REFERENCES builds(id) ON DELETE SET NULL,
    mmproj_artifact_id INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,
    n_ctx              INTEGER NOT NULL DEFAULT 0,
    parallel           INTEGER NOT NULL DEFAULT 1,
    extra_args         TEXT NOT NULL DEFAULT '[]',
    status             TEXT NOT NULL DEFAULT 'experimental'
        CHECK (status IN ('experimental', 'verified')),
    visibility         TEXT NOT NULL DEFAULT 'visible'
        CHECK (visibility IN ('visible', 'hidden')),
    is_default         INTEGER NOT NULL DEFAULT 0,
    fingerprint        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_configs_variant ON configs(variant_id);

-- Service: a standalone process the dashboard manages via Start/Stop
-- (ComfyUI, aligner, …). Not a model launch — has no Artifact, Engine,
-- Config, or Slot. Distinct from the catalog's model world. The V4
-- `creative` mode (type="service") becomes a Service with name `comfyui`.
CREATE TABLE IF NOT EXISTS services (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    label        TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    icon         TEXT NOT NULL DEFAULT '',
    color        TEXT NOT NULL DEFAULT '',
    unit         TEXT NOT NULL DEFAULT '',
    health_check TEXT NOT NULL DEFAULT '{}'
);

-- ── Remote Hosting ───────────────────────────────────────────────────────────

-- Extend router_providers with data-residency facts (ADR-0003). Providers are
-- typically single-region, so data residency is a Provider-level fact
-- inherited by all its Offerings.
ALTER TABLE router_providers ADD COLUMN country TEXT NOT NULL DEFAULT '';
ALTER TABLE router_providers ADD COLUMN data_residency_group TEXT NOT NULL DEFAULT '';

-- Offering: a remote availability of a (Model, Variant) through a specific
-- Provider (ADR-0003). The same Model on two Providers is two distinct
-- Offerings — not interchangeable (data residency, cost, reliability).
-- Replaces the V4 provider_models remote-model rows; migrate-v4 seeds from
-- router.toml [[router.backends]] kind='remote' + [[router.routes]].
CREATE TABLE IF NOT EXISTS offerings (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id         INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    variant_id       INTEGER REFERENCES variants(id) ON DELETE SET NULL,
    provider         TEXT NOT NULL REFERENCES router_providers(name) ON DELETE CASCADE,
    wire_model       TEXT NOT NULL,
    price_in_per_1m  REAL NOT NULL DEFAULT 0,
    price_out_per_1m REAL NOT NULL DEFAULT 0,
    currency         TEXT NOT NULL DEFAULT 'USD',
    context_length   INTEGER NOT NULL DEFAULT 0,
    enabled          INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_offerings_model ON offerings(model_id);
CREATE INDEX IF NOT EXISTS idx_offerings_provider ON offerings(provider);

-- ── Annotations ──────────────────────────────────────────────────────────────

-- Benchmark: one concept unifying capability scores, performance metrics,
-- provider-reported specs, and quality assessments (ADR-0005). Three sources:
-- published / self_measured / provider_reported. Three subject levels:
-- model / variant / config / offering. The F7 fabrication-prevention gate is
-- structural: published requires source_url + source_date (CHECK below).
CREATE TABLE IF NOT EXISTS benchmarks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    metric       TEXT NOT NULL,
    value        TEXT NOT NULL,
    source       TEXT NOT NULL CHECK (source IN ('published', 'self_measured', 'provider_reported')),
    source_url   TEXT NOT NULL DEFAULT '',
    source_date  TEXT NOT NULL DEFAULT '',
    subject_type TEXT NOT NULL CHECK (subject_type IN ('model', 'variant', 'config', 'offering')),
    subject_id   INTEGER NOT NULL,
    notes        TEXT NOT NULL DEFAULT '',
    CHECK (
        source != 'published'
        OR (source_url != '' AND source_date != '')
    )
);
CREATE INDEX IF NOT EXISTS idx_benchmarks_subject ON benchmarks(subject_type, subject_id);

-- Note: an operator annotation attached to a Model, Config, or Offering.
-- Distinct from Benchmark (measured/published data) — Notes are operator
-- judgment.
CREATE TABLE IF NOT EXISTS notes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('model', 'config', 'offering')),
    subject_id   INTEGER NOT NULL,
    author       TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notes_subject ON notes(subject_type, subject_id);

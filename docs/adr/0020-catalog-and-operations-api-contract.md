# Fix the catalog and operations API contract

Status: **accepted** (2026-11-01; the three open decisions below were settled by the operator the same day).
**Implemented and deployed** (phases 0 to 5, 2026-11-01 and 2026-11-02; forge v0.6.46). The plan and its status, including
live verification per phase, are in `docs/api-contract-implementation-plan.md`.

Where the implementation differs from the text below, on purpose: (1) the error envelope keeps `error` as the English message
and adds a stable `code`, instead of `{error: <code>, message}`, because the web app, CLI, a0 and MCP read `error` as the
message; (2) `/load` and `/unload` keep HTTP 200 with `in_progress:true` and an additive `operation` field, instead of 202
(frozen contract); (3) validation errors keep message-only `fields` without per-field codes. Decisions left as they are:
`catalog.require_if_match` stays off (only the config form sends `If-Match`; the rollout to every writer, then the flip, is
planned in `docs/if-match-rollout-plan.md`), and there is no required `reason` for `status=verified`. F11 was implemented
afterwards (2026-11-02): a forge key can carry `scope=catalog` (only `/api/v1/catalog/*`), must expire within 24 hours, and
any bearer key can revoke itself with `DELETE /api/v1/keys/self` without a step-up; minting is unchanged (host CLI
`-scope catalog`, or an admin session via `POST /api/v1/keys` with `scope` and `ttl_seconds`).

## Context

While replacing the Kintsugi llama.cpp patches (ADR-0019) we moved nine configs between builds, created and
edited several others, changed a smith setting, and loaded and unloaded slots dozens of times, all through the
API and the CLI. That exposed gaps between what the API appears to promise and what it does. Some cost real
time (registering and retiring build rows took two SQL migrations and a daemon deploy, for what is routine
operational data), one is a latent misconfiguration, and several are silent-failure or lost-update hazards.

This ADR records the findings, ranks them, and states the contract we want. Every finding below was checked in
the code or reproduced on ForgeHost during the work; where something is inferred it says so.

Scope: `/api/v1/catalog/*`, `/api/v1/load` and `/unload`, `/api/v1/status`, the a0 router's error behaviour, and
the `forge` CLI's settings path. Not in scope: the smith chat API, the MCP server, the web UI's look.

## Findings, ranked

Priority: **P0** can lose data, hide a failure, or is a live misconfiguration. **P1** gives consumers a misleading
contract. **P2** is a missing capability or a docs/code mismatch. **P3** is friction.

| # | P | Finding | Evidence |
|---|---|---|---|
| F1 | P0 | **The registrar never writes `--parallel` into `extra_args`.** It sets only `Parallel: 1` on a column the launcher ignores, while `docs/adding-a-model.md` says it adds `--parallel 1` as "a hard rule". Registrar-made configs rely on llama-server's default slot count. | `hfdownload/registrar.go:196`; `registry/registry.go:143` ("a dead column ... never read"); as registered today `sage-a15`, `carbon-x`, `carbon-8b` have no `--parallel`. Their effective `n_ctx` equals the configured value (one slot), so nothing is broken yet, but nothing pins it across upstream changes. |
| F2 | P0 | **PUT is strict full-replace, and a GET response is not a valid PUT body.** Unknown fields are rejected (`DisallowUnknownFields`), so `GET` output containing `id` returns 422 on `PUT`; every client keeps a hand-written allowlist. Any field omitted from a PUT is silently reset. There is one PATCH in the whole API (smith investigations). | `httpapi/validate.go:58-59`; 422 `unknown field "id"` reproduced on `PUT /catalog/configs/{id}`; route scan: 1 `PATCH` of 256 routes. An earlier session also reported a partial PUT wiping real data (recorded in that session's notes, not in the repo). |
| F3 | P0 | **No concurrency control on catalog writes.** No ETag, version or `updated_at`; `fingerprint` is an opaque stored string. With full-replace PUTs and several agent sessions working at once (this repo documents concurrent deploys), the last write wins silently. | `catalog_configs.go` `configBody`/`configJSON`: no revision field; `fingerprint` never compared. |
| F4 | P0 | **Slot loads report success before they run, and a failed load has no API signal.** `POST /load` is accepted at once and the CLI prints `loaded X (n_ctx=0)`; a startup crash was visible only in the daemon journal. `/status` exposes `slot_loading`/`slot_unloading` but no state or error. Clients end up grepping `journalctl`. | Observed repeatedly (Clover with `--spec-accept-ratio` crashed on startup while `forge load` had printed `loaded`); `/status` top-level keys are `slot_loading`, `slot_unloading`, `slot_activity`, `slots`, ... with no error field. |
| F5 | P1 | **a0 turns any backend 5xx into failover and then a generic 502 `all_backends_unavailable`, discarding the backend's own message**, and records a slot error. llama-server uses 501 for "not supported", so a request the model can never serve (an image sent to a text-only model) looks like an outage. Separately, `visibility: hidden`, a gallery setting, makes a0 answer 404 `model_not_found`, so a UI property controls API routing. | `router/proxy.go:415` (`resp.StatusCode >= 500` triggers `ErrorHandler`); llama-server `server-common.cpp:117-119` (`ERROR_TYPE_NOT_SUPPORTED` is 501). The image-to-text-only 502 was observed on Clover; that the backend sent 501 there is inferred, not traced. Hidden-config 404 reproduced with Groot and `carbon-x`. |
| F6 | P1 | **No single error envelope.** At least four shapes in use: `{"error":"admin role required"}` (no code), `{"error":"validation_failed","fields":...,"field_codes":...}`, `{"success":false,"message":...}`, and a0's `{"error":...,"layer":...,"message":...}`. CLAUDE.md requires every new HTTP error to carry a translated code. | Call-site count in `httpapi/*.go`: 491 coded (`writeErrorCode`, `writeValidationErrorCodes`), 59 bare `writeError(`, 122 uncoded `writeValidationError(`. |
| F7 | P1 | **`forge config set` bypasses validation and audit.** It checks only that the value parses as JSON and writes it straight to the settings table. The API settings handlers validate and call `audit`. A change to `smith.binaries.tracked` made this way left no audit record and was not schema-checked. | `cmd/forge/config_cli.go:80-110` (`runConfigSet`) vs `httpapi/*_handlers.go` (`s.audit(...)`). |
| F8 | P2 | **Several catalog resources have no write path in the API, and the docs imply they do.** `builds`, `engines`, `formats`, `quantizations` and `artifacts` are GET-only. Build rows are written only by smith's `build_refresh` and by migrations; artifacts only by the registrar and smith's sourcing. `adding-a-model.md` tells operators to "New Artifact" in Settings, and no UI or route does that. "Retired" is encoded as a name prefix (`"(retired) "`) that the registrar string-matches. | route table (`httpapi.go:872-873, 906`); `smith/build_refresh_ops.go:761,1112`; no web code creates artifacts. |
| F9 | P2 | **`validate` cannot validate an update.** `POST /catalog/configs/validate` always validates as a create (`excludeID` is hard-coded 0), so it reports "name already exists" for the record being edited. | `catalog_configs.go:273` (`validateConfig(ctx, b, 0)`); reproduced. |
| F10 | P2 | **The API exposes a dead field.** `parallel` is accepted, validated, persisted and returned, but never read. | `registry/registry.go:143`. |
| F11 | P3 | **Privilege friction and unguarded claims.** Every catalog write needs admin; admin keys come only from a host CLI with a TTL (we minted more than a dozen) and cannot be revoked early. `status=verified` is a claim the API lets any admin set without evidence. | observed; `docs/adding-a-model.md` "What verified means" relies on discipline. |

## Decision

Adopt these contract rules, implemented in the order given by the plan:

1. **One write contract per catalog resource.** Every resource an operator or agent must change gets create,
   update and delete through the API, enforced by one shared service layer (validation, audit, guards).
   Migrations are for schema and seed data, not routine operational rows. The registrar, smith and the CLI call
   the same layer instead of writing around it.
2. **Representations round-trip.** A `GET` body is acceptable as a `PUT` body (read-only fields are ignored and
   listed in the docs). Add `PATCH` as JSON merge patch for catalog resources so partial updates never reset
   other fields.
3. **Optimistic concurrency.** Writes honour `If-Match` against an `ETag` returned by `GET`; a mismatch is 412.
   Opt-in at first so existing clients keep working.
4. **Long operations are resources.** `load`/`unload` return 202 with an operation (state, error, timestamps),
   and `/status` slots carry `state` and `last_error`. A failed load is observable without the journal.
5. **One error envelope, honest status codes.** Every error is `{error: <code>, message, fields?, details?}`.
   a0 passes through deterministic client-side upstream errors, fails over only on real transport or
   availability failures, and pre-checks modality before dispatch.
6. **No dead or duplicated fields; no state in names.** Remove `parallel` from the contract, make `extra_args`
   the single source of launch flags (and have the registrar write `--parallel`), and replace the name-prefix
   convention for retired builds with an explicit field.

## Priority order

1. **F1** first: it is a real misconfiguration, cheap to fix, and a backfill is a no-op today.
2. **F2 + F3** together: they are the same code path and the data-loss hazard.
3. **F4**: the silent-failure hazard, and the biggest day-to-day friction.
4. **F6 + F5**: error envelope, then a0's status mapping (a0 consumers depend on shapes, so this needs the most
   compatibility care).
5. **F7**: share validation and audit with the CLI.
6. **F8 + F9 + F10**: missing write paths, validate-with-id, remove the dead field.
7. **F11** last.

## Alternatives considered

* **Keep builds smith-only and document it.** Cheapest, and smith's `build_refresh` already has the right
  guards (no retire while referenced, allowed-root check). But it makes a routine operator task (register a
  build, retire a build) depend on a procedure designed for scheduled refreshes, and this session needed two
  migrations and a daemon deploy for it. Rejected as the default; the plan still lets smith keep its flow
  through the shared service layer.
* **PATCH only, leave PUT strict.** Fixes the wipe hazard but not the round-trip problem. Rejected: both are
  cheap and clients already use PUT.
* **Version column instead of a content-hash ETag.** Stronger, needs a migration per table. The plan starts with
  the hash and revisits if spurious 412s (for example after a slot probe updates `chat_template_caps`) annoy.
* **Do nothing for a0 status codes.** a0 consumers (OpenCode, LibreChat) work today. Rejected because the
  masking hides which failures are ours; the change is additive (same `error` key, better status and detail).

## Consequences

* Positive: routine catalog changes no longer need a migration and deploy; partial updates and stale edits are
  safe; a failed load is visible; one error contract for clients and for the Japanese translations.
* Costs: changes to the catalog handlers plus shared helpers (the plan sizes them; the sizes are estimates); new error codes need `en` and `ja` entries
  (`TestErrorCodesHaveTranslations`); the web UI and CLI need small client changes; a0 behaviour changes need a
  compatibility window because external agents parse its errors.
* Risks and mitigations are in the plan's risk register. The main one is breaking a0 consumers, so a0's
  envelope keeps its current keys and only adds fields.

## Decisions for the operator

1. **Builds and artifacts write API (F8): decided yes (2026-11-01).** Both get an admin-only create/update/retire
   API with the in-use guard; smith keeps its `build_refresh` flow through the same service layer.
2. **Routing vs visibility (F5): decided, keep (2026-11-01).** `visibility: hidden` continues to mean "not served
   by a0"; there is no separate `routable` field. What changes is that the behaviour is documented and that a0
   says so: a hidden model answers 404 with the distinct code `model_not_routable` and a message explaining that
   the config exists but is hidden, instead of the indistinguishable `model_not_found`. The cost of this choice is
   that "hidden in the gallery" and "unavailable to agents" stay the same switch; revisit if a real need for
   hidden-but-callable models appears.
3. **ETag basis (F3): decided, content hash (2026-11-01).** The ETag is a hash of the canonical JSON representation
   excluding volatile probe fields; no schema change. Clients treat it as opaque, so a revision column can replace
   it later without changing the contract.

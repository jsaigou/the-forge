# Speech engines are described by compiled recipes and registered by one function

Status: accepted (2026-11-26). Plan: [speech-setup-plan.md](../speech-setup-plan.md) sections 2 and 4. Builds on [ADR-0022](0022-specialist-slot-pool.md) (the
slot pool and its registered launch kinds) and ADR-0030 (the clone engine).

## Context

A new install has no speech engine and no pool slot, and nothing says what is missing or how to add it. Registering an engine meant knowing its launch kind, parameter
names and file paths, adding a slot, setting a behaviour and a default, and restarting the daemon, because the pool was only built if slots existed at start.

## Decision

1. **Recipes are compiled data** (`go/internal/speechsetup/recipes.go`): for each engine its launch kind, parameters, files (with the size and SHA-256 of pinned
   downloads), speech mode, memory estimate, behaviour and licence. They are reviewed like code and cannot be edited through the database: a recipe decides what gets
   registered, so it is part of the launcher's trust model. Paths are relative to the engine root (`specialist.EngineRoot`, fixed per distribution).
2. **One registration function** (`speechsetup.Register`) behind one endpoint, `POST /api/v1/speech/recipes/{id}/register` (admin and step-up), used by the web page and
   `forge speech register`. It adds a slot if needed, creates the engine, sets it as the default for its kind only if that kind has none, and records the speech front
   door's address only if none is set. Running it again changes nothing, and it never turns on an engine the operator turned off or moves an existing default.
3. **Readiness looks at files.** A registered engine whose program or parameter files are gone reads `files_missing`; a kind with no engine reads `not_registered`
   (its files are here) or `not_installed`. Only existence, type, permission and, for recipes, size are checked: hashing gigabytes belongs to a download, not a report.
4. **The pool is always built.** With no slots it answers "no match" everywhere, so a host without engines behaves as before, and the first slot works without a restart.
5. **A `speech-setup` key scope** (migration 0113) for the command line: list recipes, register, read readiness; at most a day. `forge speech register` mints one for ten
   minutes and revokes it, as `forge radiance register-build` does with the catalog scope.
6. **Smith's `speech_readiness` check** speaks only when speech is wanted (the speech service has a key, or an engine is registered and not turned off) and only about what
   was meant to work: lost files, failed starts, no slot, the front door, and a key in its last 30 days.

## Not decided here

Downloading and building engines (release 4 and 5) adds sources, commits, build steps and packages to the recipes. Nothing in the web page offers to install until then.

## Consequences

An engine's pins live in the repository, so moving a pin is a reviewed change and a deploy. A hand-built engine with other weights is not matched by a recipe's size
check and cannot be registered through it; it can still be registered as before, and once registered it is judged only by the files it names.

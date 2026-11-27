# The Forge builds speech engines itself, as its service user, and installers warn instead of stopping

Status: accepted (2026-11-27). Plan: [speech-setup-plan.md](../speech-setup-plan.md) section 5. Builds on [ADR-0034](0034-speech-setup-recipes.md) (recipes).

## Context

A new install could not produce speech. The installer stopped on other hardware, used a user nobody created, shipped no web app, and reported success when
the daemon had not started; no tool made an engine's files.

## Decision

1. **`forge speech install <id>`** builds an engine from the recipe's pinned inputs. It runs as the service user and refuses root. Work happens in
   `<root>/.staging/<id>`; the engine is moved into place only after a **trial run produced sound** (for Kokoro, English and Japanese, plus a direct check
   of the Japanese front end, which otherwise fails silently). A complete engine is adopted after its pinned files are verified; nothing existing is
   overwritten without `--replace`, which keeps the old directory.
2. **Pins:** Python packages by hash (`kokoro/requirements.lock`, the versions the reference host runs), weights by size and SHA-256, the interpreter
   fetched by `uv` into `<root>/python`. The package manager is never run; a missing program is named with the command that installs it.
3. **Secrets** are written by the binary into mode-0600 files (`forge speech key rotate`) and never printed or passed as arguments.
4. **The public installer warns and continues** on any platform difference (operator decision D3). A step that cannot work is skipped by name and listed at
   the end. It stops only for bad arguments or a missing build tool, before changing anything, and exits non-zero when the daemon does not start.
5. **Voice Studio** is built from a pinned tag whose commit must match, runs as the service user on loopback only (it has no sign-in), and is linked from
   the Console unless a Voice Studio is linked already.
6. **An always-on engine in its home slot is not fit-gated** on a request, as the supervisor never was: its memory is already outside the model budget
   (ADR-0027).

## Consequences

Building Kokoro needs `uv`, CMake, a C++ compiler and `make`, and about 2.5 GB of disk (the `uv` cache stays under `<root>/.cache`). Moving a pin means a
change here and a deploy. On a machine with no GPU memory, **on-demand** engines are still refused by the fit gate; that matters for the cloning and voice
design engines, which come next.

# Blocked work — EXAMPLE (synthetic)

This is the synthetic example the parser tests pin against — the shape a
real deployment's operator-local blocked-work tracker (Deps.BlockedWorkPath)
uses. Real trackers are layer-2 deployment data: maintained on the host,
never committed, never shipped (two-layer knowledge architecture).

---

## 1. Synthetic upstream fix for the example tokenizer

**Status:** waiting on upstream — PR open since 2026-01-10, last checked
below.

**Blocked on:** upstream merging the example tokenizer crash fix.

**Where to check:** https://github.com/example-org/example-repo/pull/1234

**When unblocked:** bump the example dependency and re-run the checks.

**Checked 2026-02-01** — still open upstream.

---

## 2. Synthetic scheduler slot-drop regression

**Status: closed 2026-03-05, fix shipped in the example release.**

**Blocked on:** a scheduler bug that dropped slots under load.

**Where to check:** https://github.com/example-org/example-repo/issues/77

**When unblocked:** upgrade past the example release.

---

## 3. Synthetic memory-accounting question

**Status:** still investigating — the referenced upstream issue was closed
(a closed issue #55 turned out to be unrelated), but our repro persists.

**Blocked on:** understanding the accounting gap.

**Where to check:** https://github.com/example-org/example-repo/issues/88

**When unblocked:** a clean explanation of the gap.
